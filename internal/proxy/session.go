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

// session relays one client connection. Client messages are framed and
// classified, transaction rules are enforced, and backend messages are
// forwarded verbatim while the session tracks transaction status.
type session struct {
	logger     *slog.Logger
	client     net.Conn
	upstream   net.Conn
	classifier *classify.Classifier
	tracker    *txn.Tracker

	clientMu sync.Mutex
	txStatus byte
	skipSync bool

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

		switch msg.Type {
		case 'Z':
			var rfq pgproto3.ReadyForQuery
			if err := rfq.Decode(msg.Body); err == nil {
				s.txStatus = rfq.TxStatus
			}
		case 'C':
			var cc pgproto3.CommandComplete
			if err := cc.Decode(msg.Body); err == nil {
				s.tracker.RecordRows(txn.RowsFromCommandTag(string(cc.CommandTag)))
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
		if s.skipSync {
			if msg.Type == 'S' {
				s.skipSync = false
				s.forward(msg)
			}
			continue
		}

		switch msg.Type {
		case 'Q':
			s.handleQuery(msg)
		case 'P':
			s.handleParse(msg)
		default:
			s.forward(msg)
		}
	}
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

	result, err := s.classifier.Classify(parse.Query)
	if err != nil {
		s.forward(msg)
		return
	}
	if result.Verdict.Rejected() {
		s.reject(result.Verdict.Code, result.Verdict.Message, result.Verdict.RuleID, true)
		return
	}
	if v, refused := s.tracker.Admit(result.Kinds, time.Now()); refused {
		s.reject(v.Code, v.Message, v.Rule, true)
		return
	}
	s.forward(msg)
}

// reject answers a refused statement without involving the upstream. In the
// simple protocol the error closes the exchange with a ReadyForQuery; in the
// extended protocol the client must be told to skip ahead to its next Sync.
func (s *session) reject(code, message, rule string, extended bool) {
	s.logger.Info("rejected statement", "rule", rule, "code", code, "message", message)

	errMsg, err := (&pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     code,
		Message:  message,
	}).Encode(nil)
	if err != nil {
		s.close()
		return
	}
	if err := s.writeClient(errMsg); err != nil {
		s.close()
		return
	}

	if extended {
		s.skipSync = true
		return
	}

	rfq, err := (&pgproto3.ReadyForQuery{TxStatus: s.txStatus}).Encode(nil)
	if err != nil {
		s.close()
		return
	}
	if err := s.writeClient(rfq); err != nil {
		s.close()
	}
}

func (s *session) forward(msg wire.Message) {
	if err := s.writeUpstream(msg.Raw); err != nil {
		s.close()
	}
}

func (s *session) writeUpstream(b []byte) error {
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
