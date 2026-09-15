package proxy

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/Dreamescaper/dsql-emulator/internal/classify"
	"github.com/Dreamescaper/dsql-emulator/internal/txn"
	"github.com/Dreamescaper/dsql-emulator/internal/wire"
)

// SQLSTATE and message reported for statements that arrive while a transaction
// is already failed.
const (
	stateInFailedTransaction   = "25P02"
	messageInFailedTransaction = "current transaction is aborted, commands ignored until end of transaction block"
)

// abortTransactionQuery fails on purpose, which puts the upstream transaction
// into the aborted state. Emitting it lets PostgreSQL itself answer COMMIT with
// the ROLLBACK command tag and refuse later statements, matching Aurora DSQL.
var abortTransactionQuery = func() []byte {
	encoded, _ := (&pgproto3.Query{String: "SELECT 1/0"}).Encode(nil)
	return encoded
}()

func encodeBackend(msg pgproto3.BackendMessage) []byte {
	encoded, _ := msg.Encode(nil)
	return encoded
}

// session relays one client connection. Client messages are framed and
// classified, transaction rules are enforced, and backend messages are
// forwarded verbatim while the session tracks transaction status.
type session struct {
	logger     *slog.Logger
	client     net.Conn
	upstream   net.Conn
	classifier *classify.Classifier
	tracker    *txn.Tracker

	clientMu   sync.Mutex
	upstreamMu sync.Mutex
	stateMu    sync.Mutex

	txStatus byte
	// skipSync drops extended-protocol messages until the client's next Sync.
	skipSync bool
	// txnAborted reports that the client's transaction has failed and only
	// COMMIT or ROLLBACK may end it.
	txnAborted bool
	// aborting suppresses upstream output while an abort is injected.
	aborting    bool
	abortTarget int
	abortRFQs   int
	// abortPending waits for the client's Sync before injecting the abort,
	// because the upstream extended-query batch is still open.
	abortPending bool
	// inExtended is true between an extended-protocol message and the Sync that
	// closes its batch.
	inExtended bool

	// statements maps a prepared statement name to the kinds of the statement
	// it holds. Transaction rules are enforced when a statement is bound, not
	// when it is parsed, because clients cache prepared statements and re-run
	// them without sending Parse again.
	statements map[string][]classify.Kind

	fromClient   atomic.Int64
	fromUpstream atomic.Int64
}

// handshake relays the startup exchange. It reports whether protocol
// interception is possible: it is not once the client negotiates TLS with the
// upstream, because the emulator cannot see inside that session.
func (s *session) handshake() (bool, error) {
	for {
		startup, err := wire.ReadStartup(s.client)
		if err != nil {
			return false, err
		}
		s.fromClient.Add(int64(len(startup.Raw)))

		raw := startup.Raw
		if wire.IsStartup(startup.Code) {
			raw = wire.SetStartupParameter(startup, "default_transaction_isolation", "repeatable read")
		}
		if err := s.writeUpstream(raw); err != nil {
			return false, err
		}

		switch startup.Code {
		case wire.SSLRequestCode, wire.GSSENCRequest:
			resp := make([]byte, 1)
			if _, err := io.ReadFull(s.upstream, resp); err != nil {
				return false, err
			}
			s.fromUpstream.Add(1)
			if _, err := s.client.Write(resp); err != nil {
				return false, err
			}
			if resp[0] == 'S' {
				s.logger.Warn("client negotiated TLS with the upstream directly; interception disabled for this connection")
				return false, nil
			}
		case wire.ProtocolVersion3, wire.ProtocolVersion32:
			return true, nil
		default:
			s.logger.Debug("unrecognized startup message; not intercepting", "code", startup.Code)
			return false, nil
		}
	}
}

// run pumps both directions until the connection is finished.
func (s *session) run() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.pumpBackend()
	}()

	s.pumpFrontend()
	_ = s.upstream.Close()
	<-done
}

func (s *session) pumpBackend() {
	for {
		msg, err := wire.ReadTagged(s.upstream)
		if err != nil {
			return
		}
		s.fromUpstream.Add(int64(len(msg.Raw)))

		if s.swallowingAbort() {
			s.consumeAbortMessage(msg)
			continue
		}

		switch msg.Type {
		case 'Z':
			var rfq pgproto3.ReadyForQuery
			if err := rfq.Decode(msg.Body); err == nil {
				s.setStateTxStatus(rfq.TxStatus)
			}
		case 'C':
			var cc pgproto3.CommandComplete
			if err := cc.Decode(msg.Body); err == nil {
				s.tracker.RecordRows(txn.RowsFromCommandTag(string(cc.CommandTag)))
				if s.rowLimitCrossed() {
					// The statement already ran, so its success is withheld and
					// the transaction is failed, matching DSQL.
					s.sendError(txn.CodeProgramLimit, "transaction row limit exceeded", "dml_rows")
					s.startRowLimitAbort()
					continue
				}
			}
		}

		if err := s.writeClient(msg.Raw); err != nil {
			return
		}
	}
}

func (s *session) pumpFrontend() {
	for {
		msg, err := wire.ReadTagged(s.client)
		if err != nil {
			return
		}
		s.fromClient.Add(int64(len(msg.Raw)))

		if msg.Type == 'X' {
			_ = s.writeUpstream(msg.Raw)
			return
		}

		if msg.Type == 'S' {
			s.handleSync(msg)
			continue
		}
		if s.droppingUntilSync() {
			continue
		}

		switch msg.Type {
		case 'Q':
			s.markExtended(false)
			s.handleQuery(msg)
		case 'P':
			s.markExtended(true)
			s.handleParse(msg)
		case 'B':
			s.markExtended(true)
			s.handleBind(msg)
		case 'D', 'E':
			s.markExtended(true)
			s.forward(msg)
		case 'C':
			s.handleClose(msg)
		default:
			s.forward(msg)
		}
	}
}

func (s *session) handleSync(msg wire.Message) {
	s.markExtended(false)

	s.stateMu.Lock()
	pending := s.abortPending
	s.abortPending = false
	s.skipSync = false
	s.stateMu.Unlock()

	if pending {
		// Close the upstream batch, then fail the transaction.
		s.forward(msg)
		s.beginAbort(2)
		return
	}
	s.forward(msg)
}

func (s *session) handleQuery(msg wire.Message) {
	decoded, err := wire.DecodeFrontend(msg.Type, msg.Body)
	if err != nil {
		s.forward(msg)
		return
	}
	query, ok := decoded.(*pgproto3.Query)
	if !ok {
		s.forward(msg)
		return
	}

	if s.aborted() {
		if s.endsTransaction(query.String) {
			s.endAborted()
			s.forward(msg)
			return
		}
		s.rejectFailed(false)
		return
	}

	result, err := s.classifier.Classify(query.String)
	if err != nil {
		s.forward(msg)
		return
	}
	if result.Verdict.Rejected() {
		s.reject(result.Verdict.Code, result.Verdict.Message, result.Verdict.RuleID, false)
		return
	}
	if v, refused := s.tracker.Admit(result.Kinds, time.Now()); refused {
		s.reject(v.Code, v.Message, v.Rule, false)
		return
	}
	s.forward(msg)
}

// handleParse refuses statements the dialect does not allow, and remembers the
// kinds of the rest so the rules can be applied when they run.
func (s *session) handleParse(msg wire.Message) {
	decoded, err := wire.DecodeFrontend(msg.Type, msg.Body)
	if err != nil {
		s.forward(msg)
		return
	}
	parse, ok := decoded.(*pgproto3.Parse)
	if !ok {
		s.forward(msg)
		return
	}

	if s.aborted() {
		result, err := s.classifier.Classify(parse.Query)
		if err == nil && endsTransaction(result.Kinds) {
			s.statements[parse.Name] = result.Kinds
			s.forward(msg)
			return
		}
		s.rejectFailed(true)
		return
	}

	result, err := s.classifier.Classify(parse.Query)
	if err != nil {
		// Unparseable SQL is not evidence of an incompatibility; let the
		// backend answer it. Its kinds are unknown, so it is never admitted.
		delete(s.statements, parse.Name)
		s.forward(msg)
		return
	}
	if result.Verdict.Rejected() {
		delete(s.statements, parse.Name)
		s.reject(result.Verdict.Code, result.Verdict.Message, result.Verdict.RuleID, true)
		return
	}

	s.statements[parse.Name] = result.Kinds
	s.forward(msg)
}

// handleBind applies the transaction rules at execution time. The statement
// itself was already checked when it was parsed.
func (s *session) handleBind(msg wire.Message) {
	decoded, err := wire.DecodeFrontend(msg.Type, msg.Body)
	if err != nil {
		s.forward(msg)
		return
	}
	bind, ok := decoded.(*pgproto3.Bind)
	if !ok {
		s.forward(msg)
		return
	}

	kinds, known := s.statements[bind.PreparedStatement]
	if !known {
		s.forward(msg)
		return
	}

	if s.aborted() {
		if endsTransaction(kinds) {
			s.endAborted()
			s.forward(msg)
			return
		}
		s.rejectFailed(true)
		return
	}

	if v, refused := s.tracker.Admit(kinds, time.Now()); refused {
		s.reject(v.Code, v.Message, v.Rule, true)
		return
	}
	s.forward(msg)
}

func (s *session) handleClose(msg wire.Message) {
	decoded, err := wire.DecodeFrontend(msg.Type, msg.Body)
	if err != nil {
		s.forward(msg)
		return
	}
	closeMsg, ok := decoded.(*pgproto3.Close)
	if !ok {
		s.forward(msg)
		return
	}
	if closeMsg.ObjectType == 'S' {
		delete(s.statements, closeMsg.Name)
	}
	s.forward(msg)
}

// reject answers a refused statement without involving the upstream. A refusal
// inside a transaction fails the transaction, as it would on a real server.
func (s *session) reject(code, message, rule string, extended bool) {
	s.logger.Info("rejected statement", "rule", rule, "code", code, "message", message)
	s.sendError(code, message, rule)

	if s.tracker.Stats().InTxn {
		if extended {
			s.deferAbortToSync()
		} else {
			s.beginAbort(1)
		}
		return
	}

	if extended {
		s.stateMu.Lock()
		s.skipSync = true
		s.stateMu.Unlock()
		return
	}
	s.writeClient(encodeBackend(&pgproto3.ReadyForQuery{TxStatus: s.getStateTxStatus()}))
}

// rejectFailed refuses a statement because the transaction is already failed.
func (s *session) rejectFailed(extended bool) {
	s.logger.Info("rejected statement in failed transaction", "code", stateInFailedTransaction)
	s.sendError(stateInFailedTransaction, messageInFailedTransaction, "failed_transaction")

	if extended {
		s.stateMu.Lock()
		s.skipSync = true
		s.stateMu.Unlock()
		return
	}
	s.writeClient(encodeBackend(&pgproto3.ReadyForQuery{TxStatus: 'E'}))
}

// beginAbort fails the upstream transaction and swallows the given number of
// upstream ReadyForQuery messages before completing the client's exchange with
// a failed-transaction status.
func (s *session) beginAbort(swallowRFQs int) {
	s.stateMu.Lock()
	s.aborting = true
	s.abortTarget = swallowRFQs
	s.abortRFQs = 0
	s.txnAborted = true
	s.stateMu.Unlock()

	_ = s.writeUpstream(abortTransactionQuery)
}

// deferAbortToSync waits for the client's Sync, because the upstream is still
// inside an extended-query batch.
func (s *session) deferAbortToSync() {
	s.stateMu.Lock()
	s.abortPending = true
	s.skipSync = true
	s.stateMu.Unlock()
}

func (s *session) startRowLimitAbort() {
	if s.getInExtended() {
		s.deferAbortToSync()
		return
	}
	s.beginAbort(2)
}

func (s *session) swallowingAbort() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.aborting
}

func (s *session) consumeAbortMessage(msg wire.Message) {
	if msg.Type != 'Z' {
		return
	}
	s.stateMu.Lock()
	s.abortRFQs++
	done := s.abortRFQs >= s.abortTarget
	if done {
		s.aborting = false
		s.abortRFQs = 0
	}
	s.stateMu.Unlock()

	if done {
		s.writeClient(encodeBackend(&pgproto3.ReadyForQuery{TxStatus: 'E'}))
	}
}

func (s *session) endAborted() {
	s.stateMu.Lock()
	s.txnAborted = false
	s.skipSync = false
	s.stateMu.Unlock()
	s.tracker.Admit([]classify.Kind{classify.KindRollback}, time.Now())
}

func (s *session) aborted() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.txnAborted
}

func (s *session) getInExtended() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.inExtended
}

func (s *session) markExtended(in bool) {
	s.stateMu.Lock()
	s.inExtended = in
	s.stateMu.Unlock()
}

func (s *session) droppingUntilSync() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.skipSync
}

func (s *session) getStateTxStatus() byte {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.txStatus
}

func (s *session) setStateTxStatus(status byte) {
	s.stateMu.Lock()
	s.txStatus = status
	s.stateMu.Unlock()
}

func (s *session) rowLimitCrossed() bool {
	stats := s.tracker.Stats()
	return stats.InTxn && stats.OverRows
}

func (s *session) endsTransaction(sql string) bool {
	result, err := s.classifier.Classify(sql)
	return err == nil && endsTransaction(result.Kinds)
}

func endsTransaction(kinds []classify.Kind) bool {
	for _, k := range kinds {
		if k == classify.KindCommit || k == classify.KindRollback {
			return true
		}
	}
	return false
}

func (s *session) sendError(code, message, rule string) {
	if encoded := encodeBackend(&pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     code,
		Message:  message,
	}); encoded != nil {
		s.writeClient(encoded)
	}
	s.logger.Debug("sent error", "rule", rule, "code", code)
}

func (s *session) forward(msg wire.Message) {
	if err := s.writeUpstream(msg.Raw); err != nil {
		s.close()
	}
}

func (s *session) writeUpstream(b []byte) error {
	s.upstreamMu.Lock()
	defer s.upstreamMu.Unlock()
	_, err := s.upstream.Write(b)
	return err
}

func (s *session) writeClient(b []byte) error {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	_, err := s.client.Write(b)
	return err
}

func (s *session) close() {
	_ = s.client.Close()
	_ = s.upstream.Close()
}
