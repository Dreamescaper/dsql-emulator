package proxy

import (
	"strconv"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/Dreamescaper/dsql-emulator/internal/occ"
	"github.com/Dreamescaper/dsql-emulator/internal/wire"
)

// savepointName is what the adjudicator rolls a transaction back to after the
// backend refused it a lock. One savepoint per transaction is enough: the
// transaction it repairs is doomed, so the work it loses is discarded at COMMIT
// anyway, and holding one subtransaction rather than one per statement keeps
// the backend's subtransaction stack shallow.
const savepointName = "dsql_occ"

var (
	savepointQuery  = simpleQuery("SAVEPOINT " + savepointName)
	rollbackToQuery = simpleQuery("ROLLBACK TO SAVEPOINT " + savepointName)
)

func simpleQuery(sql string) []byte {
	encoded, _ := (&pgproto3.Query{String: sql}).Encode(nil)
	return encoded
}

// inFlight is the statement the client is waiting on, and what it would report
// if the backend refuses it a lock.
type inFlight struct {
	intent   occ.Intent
	extended bool
	// formats are the result format codes the client's Bind asked for, so a
	// shadow answers in the encoding the client is already decoding.
	formats []int16
	// params and paramFormats are the values the client bound, which a shadow
	// that kept any of the statement's parameters is run with.
	params       [][]byte
	paramFormats []int16
}

// shadowParams picks out the values the shadow asks for, in its own order, from
// the ones the client bound.
func (f *inFlight) shadowParams() (values [][]byte, formats []int16, ok bool) {
	for _, pos := range f.intent.Params {
		if pos < 1 || pos > len(f.params) {
			return nil, nil, false
		}
		values = append(values, f.params[pos-1])
		formats = append(formats, bindFormat(f.paramFormats, pos))
	}
	return values, formats, true
}

// bindFormat reports the format code the client bound one parameter with. A
// Bind carries no codes when every value is text, one when they all share it,
// and otherwise one per parameter.
func bindFormat(codes []int16, position int) int16 {
	switch {
	case len(codes) == 0:
		return 0
	case len(codes) == 1:
		return codes[0]
	case position-1 < len(codes):
		return codes[position-1]
	default:
		return 0
	}
}

// hiddenExchange is an exchange the emulator runs on the upstream on its own
// behalf. Every backend message is offered to consume until it reports the
// exchange over; finish then completes the client's own exchange.
type hiddenExchange struct {
	consume func(msg wire.Message) bool
	finish  func()
}

// consumeHidden routes a backend message to the exchange the emulator is
// running, and reports whether it belonged to one.
func (s *session) consumeHidden(msg wire.Message) bool {
	s.stateMu.Lock()
	h := s.hidden
	s.stateMu.Unlock()
	if h == nil {
		return false
	}
	if !h.consume(msg) {
		return true
	}
	// Cleared before finish, so a step may start the next one.
	s.stateMu.Lock()
	s.hidden = nil
	s.stateMu.Unlock()
	h.finish()
	return true
}

func (s *session) setHidden(h *hiddenExchange) {
	s.stateMu.Lock()
	s.hidden = h
	s.stateMu.Unlock()
}

// armAdjudicator prepares the transaction to have this statement's conflict
// deferred to COMMIT: the savepoint that a repair rolls back to is established
// first, ahead of the statement itself, and the statement is remembered only if
// that succeeded. A statement the emulator could not answer for needs neither.
func (s *session) armAdjudicator(intent *occ.Intent, bind *pgproto3.Bind) {
	if intent == nil || !s.savepointReady(bind) {
		s.setInFlight(nil, nil)
		return
	}
	s.setInFlight(intent, bind)
}

// savepointReady reports whether the transaction carries the savepoint a repair
// rolls back to, establishing one when that can still be done safely.
func (s *session) savepointReady(bind *pgproto3.Bind) bool {
	s.stateMu.Lock()
	has := s.occSavepoint
	s.stateMu.Unlock()
	if has {
		return true
	}
	// A simple query destroys the unnamed prepared statement, so one cannot be
	// written between a client's Parse and the Bind that uses it. The Parse is
	// where that statement's savepoint is established instead; a named
	// statement is unaffected, and a simple query has no Parse to come between.
	if bind != nil && bind.PreparedStatement == "" {
		return false
	}
	return s.establishSavepoint()
}

// setInFlight records what the statement now executing would report if the
// backend refuses it a lock. A nil intent means the emulator cannot reproduce
// the answer, so a conflict is reported where PostgreSQL raises it. bind is the
// client's Bind, or nil for a simple query, which binds nothing.
func (s *session) setInFlight(intent *occ.Intent, bind *pgproto3.Bind) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	switch {
	case intent == nil:
		s.occInFlight = nil
	case bind == nil:
		// A simple query carries no values, so a shadow that needs any of them
		// cannot be run for it.
		if len(intent.Params) > 0 {
			s.occInFlight = nil
			return
		}
		s.occInFlight = &inFlight{intent: *intent}
	default:
		s.occInFlight = &inFlight{
			intent:       *intent,
			extended:     true,
			formats:      bind.ResultFormatCodes,
			params:       bind.Parameters,
			paramFormats: bind.ParameterFormatCodes,
		}
	}
}

// occAdjudicated reports whether this transaction was refused a lock, so its
// COMMIT must fail the way Aurora DSQL fails it.
func (s *session) occAdjudicated() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.occDoomed
}

// noteTxStatus forgets what belonged to a transaction that has ended. An
// aborted transaction still holds its savepoint, and cannot take a new one.
func (s *session) noteTxStatus(status byte) {
	if status != 'I' {
		return
	}
	s.stateMu.Lock()
	s.occSavepoint = false
	s.occDoomed = false
	s.occInFlight = nil
	s.stateMu.Unlock()
}

// establishSavepoint gives the adjudicator somewhere to roll back to, before
// the statement that might need it is forwarded.
//
// It is written from the frontend path, in the same goroutine and the same
// order as the client's own statements, because that is the only way to know
// where its answer falls in the stream. Injecting it when a ReadyForQuery
// arrives instead would put it behind anything a pipelining client had already
// sent, and its hidden exchange would then swallow the client's answer.
//
// It reports whether the transaction now carries one.
func (s *session) establishSavepoint() bool {
	s.stateMu.Lock()
	switch {
	case s.occSavepoint:
		s.stateMu.Unlock()
		return true
	case s.pendingExchanges > 0 || s.extendedBatchOpen:
		// The client is pipelining -- either with answers still outstanding, or
		// part-way through a batch its Sync has not closed -- so there is no gap
		// to write into. The transaction goes without, and a conflict in it is
		// reported where PostgreSQL raises it.
		s.stateMu.Unlock()
		return false
	}
	s.occSavepoint = true
	s.stateMu.Unlock()

	var failed bool
	s.setHidden(&hiddenExchange{
		consume: consumeUntilReady(&failed),
		finish: func() {
			if !failed {
				return
			}
			s.stateMu.Lock()
			s.occSavepoint = false
			s.stateMu.Unlock()
			s.logger.Debug("savepoint refused; conflicts stay where postgresql raises them")
		},
	})
	if err := s.writeUpstreamExchange(savepointQuery); err != nil {
		s.close()
		return false
	}
	return true
}

// consumeUntilReady swallows an exchange through its ReadyForQuery, recording
// whether it failed.
func consumeUntilReady(failed *bool) func(wire.Message) bool {
	return func(msg wire.Message) bool {
		if msg.Type == 'E' {
			*failed = true
		}
		return msg.Type == 'Z'
	}
}

// occIntercept takes over an error that is evidence of a conflict Aurora DSQL
// would have resolved at COMMIT. It reports whether the error was taken over,
// in which case the client must not see it.
func (s *session) occIntercept(msg wire.Message) bool {
	var er pgproto3.ErrorResponse
	if err := er.Decode(msg.Body); err != nil || !occ.IsConflict(er.Code) {
		return false
	}

	s.stateMu.Lock()
	flight := s.occInFlight
	// Outside a transaction there is nothing to defer the failure to; without a
	// savepoint there is no way back to a usable one; and with more than the
	// failed request outstanding the client has pipelined past it, so there is
	// no gap to run the repair in.
	ready := s.occSavepoint && s.txStatus == 'T' && flight != nil && s.pendingExchanges <= 1
	if ready {
		s.occRepair = flight
		s.occInFlight = nil
	}
	s.stateMu.Unlock()
	if !ready {
		return false
	}

	s.logger.Info("conflict deferred to commit", "code", er.Code, "statement", flight.intent.Tag)
	return true
}

// repairPending reports whether a refused statement is waiting for the
// ReadyForQuery that ends its failed exchange.
func (s *session) repairPending() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.occRepair != nil
}

// startRepair undoes the refused statement and answers the client as Aurora
// DSQL would have. It reports whether a repair was started, in which case the
// ReadyForQuery that triggered it belongs to the emulator.
func (s *session) startRepair() bool {
	s.stateMu.Lock()
	repair := s.occRepair
	if repair != nil {
		s.occRepair = nil
		s.occDoomed = true
		// The client is never told its transaction failed, so as far as it is
		// concerned the transaction is still open.
		s.txStatus = 'T'
	}
	s.stateMu.Unlock()
	if repair == nil {
		return false
	}

	var failed bool
	s.setHidden(&hiddenExchange{
		consume: consumeUntilReady(&failed),
		finish: func() {
			if failed {
				s.failRepair()
				return
			}
			s.runShadow(repair)
		},
	})
	if err := s.writeUpstreamExchange(rollbackToQuery); err != nil {
		s.close()
	}
	return true
}

// runShadow reports what the refused statement would have, using a read-only
// statement that takes no locks and so cannot be refused in turn.
func (s *session) runShadow(repair *inFlight) {
	switch {
	case repair.intent.Shadow == "":
		// The statement stated its own row count.
		s.completeRepair(repair, repair.intent.Rows)
	case repair.intent.Rowset:
		s.runRowsetShadow(repair)
	default:
		s.runCountShadow(repair)
	}
}

// runCountShadow counts the rows the refused statement would have changed.
func (s *session) runCountShadow(repair *inFlight) {
	values, formats, ok := repair.shadowParams()
	if !ok {
		s.failRepair()
		return
	}

	var failed bool
	rows := 0
	untilReady := consumeUntilReady(&failed)
	s.setHidden(&hiddenExchange{
		consume: func(msg wire.Message) bool {
			if msg.Type == 'D' {
				if n, ok := firstColumnInt(msg); ok {
					rows = n
				}
			}
			return untilReady(msg)
		},
		finish: func() {
			if failed {
				s.failRepair()
				return
			}
			s.completeRepair(repair, rows)
		},
	})
	// The count is read here rather than forwarded, so it is asked for as text
	// whatever the client's own statement was bound for.
	var err error
	if len(values) > 0 {
		err = s.writeUpstreamExchange(extendedQuery(repair.intent.Shadow, values, formats, nil))
	} else {
		err = s.writeUpstreamExchange(simpleQuery(repair.intent.Shadow))
	}
	if err != nil {
		s.close()
	}
}

// runRowsetShadow re-runs a refused locking SELECT without its locking clause.
// The rows are the same; only the lock is not taken.
func (s *session) runRowsetShadow(repair *inFlight) {
	values, formats, ok := repair.shadowParams()
	if !ok {
		s.failRepair()
		return
	}

	var failed bool
	s.setHidden(&hiddenExchange{
		consume: func(msg wire.Message) bool {
			switch msg.Type {
			case 'E':
				failed = true
			case 'Z':
				return true
			case 'D', 'C':
				s.writeClient(msg.Raw)
			case 'T':
				// An extended-protocol client already has the row description
				// from its own Describe; a second one would break the exchange.
				if !repair.extended {
					s.writeClient(msg.Raw)
				}
			}
			return false
		},
		finish: func() {
			if failed {
				s.failRepair()
				return
			}
			s.writeClient(encodeBackend(&pgproto3.ReadyForQuery{TxStatus: 'T'}))
		},
	})

	var err error
	if repair.extended {
		err = s.writeUpstreamExchange(extendedQuery(repair.intent.Shadow, values, formats, repair.formats))
	} else {
		err = s.writeUpstreamExchange(simpleQuery(repair.intent.Shadow))
	}
	if err != nil {
		s.close()
	}
}

// completeRepair answers the refused statement the way Aurora DSQL answers it:
// as if it had run. The transaction is left open and doomed, and fails at
// COMMIT.
func (s *session) completeRepair(repair *inFlight, rows int) {
	tag := repair.intent.CommandTag(rows)
	s.writeClient(encodeBackend(&pgproto3.CommandComplete{CommandTag: []byte(tag)}))
	s.writeClient(encodeBackend(&pgproto3.ReadyForQuery{TxStatus: 'T'}))
	s.logger.Debug("answered refused statement", "tag", tag)
}

// failRepair gives up on deferring the conflict to COMMIT and reports it where
// PostgreSQL raised it, which fails the transaction.
func (s *session) failRepair() {
	occRules := s.classifier.Ruleset().OCC
	code, message := conflictError(occRules)

	s.logger.Warn("could not defer conflict to commit", "code", code)
	s.stateMu.Lock()
	s.occDoomed = false
	s.stateMu.Unlock()

	s.sendError(code, message, "occ_conflict")
	s.beginFailure(txnFailure{statement: abortTransactionQuery, status: 'E', swallow: 1})
}

// extendedQuery runs a statement through the extended protocol, bound with the
// values the shadow asks for and asking for the result format codes the caller
// wants. The parameter types are left to the backend to infer: a shadow keeps
// each parameter in the expression it was already used in, so it is inferred
// from the same context as in the statement that was refused.
func extendedQuery(sql string, values [][]byte, paramFormats, resultFormats []int16) []byte {
	var out []byte
	out, _ = (&pgproto3.Parse{Query: sql}).Encode(out)
	out, _ = (&pgproto3.Bind{
		Parameters:           values,
		ParameterFormatCodes: paramFormats,
		ResultFormatCodes:    resultFormats,
	}).Encode(out)
	out, _ = (&pgproto3.Execute{}).Encode(out)
	out, _ = (&pgproto3.Sync{}).Encode(out)
	return out
}

// firstColumnInt reads the first column of a DataRow as an integer.
func firstColumnInt(msg wire.Message) (int, bool) {
	var row pgproto3.DataRow
	if err := row.Decode(msg.Body); err != nil || len(row.Values) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(string(row.Values[0]))
	if err != nil {
		return 0, false
	}
	return n, true
}
