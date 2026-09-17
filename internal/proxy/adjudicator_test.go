package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// refuseLock answers the statement in flight the way the backend answers one it
// would have had to wait for, and then services the repair the adjudicator runs:
// the rollback to the transaction's savepoint, and the shadow that reports what
// the refused statement would have.
//
// The shadow's reply is sent from its own goroutine, because a shadow whose
// rows are forwarded to the client cannot be written in full until the client
// reads them and the pipes are unbuffered. The returned wait must be called
// before the caller sends anything else to the backend, since the goroutine
// holds the backend encoder until then: right away when the shadow only
// reports a count, and after reading the rows when it reports rows.
func (ts *testSession) refuseLock(t *testing.T, code, shadow string, shadowRows []string) func() {
	t.Helper()

	ts.sendBackend(t, &pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     code,
		Message:  "canceling statement due to lock timeout",
	})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'E'})

	if got := ts.expectBackendQuery(t); got != "ROLLBACK TO SAVEPOINT "+savepointName {
		t.Fatalf("backend received %q want the rollback to the savepoint", got)
	}
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'T'})

	if shadow == "" {
		return func() {}
	}
	if got := ts.expectBackendQuery(t); got != shadow {
		t.Fatalf("backend received shadow %q want %q", got, shadow)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ts.be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("shadow")}}})
		for _, v := range shadowRows {
			ts.be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte(v)}})
		}
		ts.be.Send(&pgproto3.CommandComplete{CommandTag: []byte(fmt.Sprintf("SELECT %d", len(shadowRows)))})
		ts.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'})
		_ = ts.be.Flush()
	}()
	return func() { <-done }
}

// A transaction the backend refused a lock is answered as if its statement had
// run, and fails at COMMIT the way Aurora DSQL fails it.
func TestSessionDefersRefusedLockToCommit(t *testing.T) {
	for _, code := range []string{"55P03", "40001"} {
		t.Run(code, func(t *testing.T) {
			ts := newTestSession(t)
			ts.roundTrip(t, "BEGIN", "BEGIN", 'T')

			const sql = "UPDATE t SET v = 'b' WHERE id = 1"
			ts.send(t, &pgproto3.Query{String: sql})
			if got := ts.expectBackendQuery(t); got != sql {
				t.Fatalf("backend received %q want %q", got, sql)
			}

			ts.refuseLock(t, code, "SELECT count(*) FROM t WHERE id = 1", []string{"1"})()

			cc, ok := ts.receive(t).(*pgproto3.CommandComplete)
			if !ok {
				t.Fatal("expected the refused statement to be answered as if it ran")
			}
			if got := string(cc.CommandTag); got != "UPDATE 1" {
				t.Fatalf("got command tag %q want UPDATE 1", got)
			}
			rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
			if !ok {
				t.Fatal("expected ReadyForQuery after the refused statement")
			}
			if rfq.TxStatus != 'T' {
				t.Fatalf("got tx status %q want T: the transaction stays open", rfq.TxStatus)
			}

			// The conflict surfaces at COMMIT, where Aurora DSQL reports it.
			ts.send(t, &pgproto3.Query{String: "COMMIT"})
			er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
			if !ok {
				t.Fatal("expected the commit to be refused")
			}
			if er.Code != "40001" || !strings.Contains(er.Message, "OC000") {
				t.Fatalf("got %s %q want 40001 with OC000", er.Code, er.Message)
			}

			if got := ts.expectBackendQuery(t); got != "ROLLBACK" {
				t.Fatalf("backend received %q want ROLLBACK", got)
			}
			ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'I'})
			final, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
			if !ok {
				t.Fatal("expected ReadyForQuery after the refused commit")
			}
			if final.TxStatus != 'I' {
				t.Fatalf("got tx status %q want I", final.TxStatus)
			}
		})
	}
}

// An INSERT states its own row count, so no shadow is needed to answer it.
func TestSessionAnswersRefusedInsertWithoutAShadow(t *testing.T) {
	ts := newTestSession(t)
	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')

	const sql = "INSERT INTO child (id, parent_id) VALUES (1, 2)"
	ts.send(t, &pgproto3.Query{String: sql})
	if got := ts.expectBackendQuery(t); got != sql {
		t.Fatalf("backend received %q want %q", got, sql)
	}

	ts.refuseLock(t, "55P03", "", nil)()

	cc, ok := ts.receive(t).(*pgproto3.CommandComplete)
	if !ok {
		t.Fatal("expected the refused insert to be answered")
	}
	if got := string(cc.CommandTag); got != "INSERT 0 1" {
		t.Fatalf("got command tag %q want INSERT 0 1", got)
	}
}

// A refused locking SELECT is re-run without its locking clause, so the client
// gets the rows it was waiting for.
func TestSessionAnswersRefusedLockingSelectWithItsRows(t *testing.T) {
	ts := newTestSession(t)
	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')

	const sql = "SELECT name FROM t WHERE id = 1 FOR UPDATE"
	ts.send(t, &pgproto3.Query{String: sql})
	if got := ts.expectBackendQuery(t); got != sql {
		t.Fatalf("backend received %q want %q", got, sql)
	}

	waitForShadow := ts.refuseLock(t, "55P03", "SELECT name FROM t WHERE id = 1", []string{"ww-b"})

	if _, ok := ts.receive(t).(*pgproto3.RowDescription); !ok {
		t.Fatal("expected the shadow's row description to reach the client")
	}
	row, ok := ts.receive(t).(*pgproto3.DataRow)
	if !ok {
		t.Fatal("expected the shadow's row to reach the client")
	}
	if got := string(row.Values[0]); got != "ww-b" {
		t.Fatalf("got row %q want ww-b", got)
	}
	if _, ok := ts.receive(t).(*pgproto3.CommandComplete); !ok {
		t.Fatal("expected the shadow's command tag to reach the client")
	}
	rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatal("expected ReadyForQuery after the refused select")
	}
	if rfq.TxStatus != 'T' {
		t.Fatalf("got tx status %q want T", rfq.TxStatus)
	}
	waitForShadow()
}

// Outside a transaction there is nothing to defer a conflict to: the implicit
// transaction has already ended, so the statement itself reports it.
func TestSessionReportsConflictOnImplicitTransaction(t *testing.T) {
	ts := newTestSession(t)

	ts.send(t, &pgproto3.Query{String: "UPDATE t SET v = 'b' WHERE id = 1"})
	ts.expectBackendQuery(t)

	ts.sendBackend(t, &pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     "55P03",
		Message:  "canceling statement due to lock timeout",
	})

	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected the conflict to reach the client")
	}
	if er.Code != "40001" || !strings.Contains(er.Message, "OC000") {
		t.Fatalf("got %s %q want 40001 with OC000", er.Code, er.Message)
	}
}

// A statement whose answer the emulator cannot reproduce keeps PostgreSQL's
// behavior rather than being answered with a fabricated one.
func TestSessionDoesNotDeferWhatItCannotReproduce(t *testing.T) {
	ts := newTestSession(t)
	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')

	const sql = "UPDATE t SET v = 'b' WHERE id = 1 RETURNING id"
	ts.send(t, &pgproto3.Query{String: sql})
	ts.expectBackendQuery(t)

	ts.sendBackend(t, &pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     "55P03",
		Message:  "canceling statement due to lock timeout",
	})

	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected the conflict to reach the client")
	}
	if er.Code != "40001" {
		t.Fatalf("got %s want 40001", er.Code)
	}
}

// The savepoint is established once per transaction, not once per statement.
func TestSessionEstablishesOneSavepointPerTransaction(t *testing.T) {
	ts := newTestSession(t)

	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.roundTrip(t, "INSERT INTO t VALUES (1)", "INSERT 0 1", 'T')
	ts.roundTrip(t, "INSERT INTO t VALUES (2)", "INSERT 0 1", 'T')
	ts.roundTrip(t, "COMMIT", "COMMIT", 'I')

	// A second transaction gets its own savepoint.
	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.roundTrip(t, "COMMIT", "COMMIT", 'I')
}

// The startup exchange asks the backend to bound its lock waits, which is what
// turns a block into the conflict the adjudicator resolves.
func TestSessionAdvertisesLockTimeoutUpstream(t *testing.T) {
	ts := newTestSession(t)

	if options := ts.startup.Parameters["options"]; !strings.Contains(options, "lock_timeout=50ms") {
		t.Fatalf("startup options %q do not bound the lock wait", options)
	}
}

// A shadow is a different statement, so it keeps only the parameters it still
// refers to, renumbered from $1, and is bound with the values the client bound
// for those. This is the shape application code writes, and it goes over the
// extended protocol because a simple query carries no values.
func TestSessionRunsAParameterisedShadowWithTheBoundValues(t *testing.T) {
	ts := newTestSession(t)
	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')

	const sql = "UPDATE t SET v = $1 WHERE id = $2"
	ts.parse(t, "s1", sql)

	// The client binds the new value and the row it is changing.
	ts.send(t, &pgproto3.Bind{
		DestinationPortal: "p1",
		PreparedStatement: "s1",
		Parameters:        [][]byte{[]byte("new-value"), []byte("42")},
	})
	if _, ok := ts.nextBackend(t).(*pgproto3.Bind); !ok {
		t.Fatal("expected Bind at the backend")
	}
	ts.sendBackend(t, &pgproto3.BindComplete{})
	if _, ok := ts.receive(t).(*pgproto3.BindComplete); !ok {
		t.Fatal("expected BindComplete at the client")
	}
	ts.execute(t, "p1")

	// The backend refuses the lock, and the client's Sync ends the exchange.
	ts.sendBackend(t, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "55P03", Message: "canceling statement due to lock timeout"})
	ts.send(t, &pgproto3.Sync{})
	if _, ok := ts.receiveBackend(t).(*pgproto3.Sync); !ok {
		t.Fatal("expected Sync at the backend")
	}
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'E'})

	if got := ts.expectBackendQuery(t); got != "ROLLBACK TO SAVEPOINT "+savepointName {
		t.Fatalf("backend received %q want the rollback to the savepoint", got)
	}
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'T'})

	// The shadow keeps only the predicate's parameter, renumbered to $1.
	parsed, ok := ts.receiveBackend(t).(*pgproto3.Parse)
	if !ok {
		t.Fatal("expected the shadow to be parsed at the backend")
	}
	if want := "SELECT count(*) FROM t WHERE id = $1"; parsed.Query != want {
		t.Fatalf("shadow is %q want %q", parsed.Query, want)
	}
	bound, ok := ts.receiveBackend(t).(*pgproto3.Bind)
	if !ok {
		t.Fatal("expected the shadow to be bound at the backend")
	}
	if len(bound.Parameters) != 1 {
		t.Fatalf("shadow bound %d values want 1", len(bound.Parameters))
	}
	// The row's id, not the new value the SET list dropped.
	if got := string(bound.Parameters[0]); got != "42" {
		t.Fatalf("shadow was bound with %q want %q", got, "42")
	}
	if _, ok := ts.receiveBackend(t).(*pgproto3.Execute); !ok {
		t.Fatal("expected the shadow to be executed")
	}
	if _, ok := ts.receiveBackend(t).(*pgproto3.Sync); !ok {
		t.Fatal("expected the shadow's Sync")
	}

	ts.sendBackend(t, &pgproto3.ParseComplete{})
	ts.sendBackend(t, &pgproto3.BindComplete{})
	ts.sendBackend(t, &pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'T'})

	cc, ok := ts.receive(t).(*pgproto3.CommandComplete)
	if !ok {
		t.Fatal("expected the refused statement to be answered as if it ran")
	}
	if got := string(cc.CommandTag); got != "UPDATE 1" {
		t.Fatalf("got command tag %q want UPDATE 1", got)
	}
	rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatal("expected ReadyForQuery after the refused statement")
	}
	if rfq.TxStatus != 'T' {
		t.Fatalf("got tx status %q want T", rfq.TxStatus)
	}
}

// When the shadow itself cannot run, the conflict is reported where PostgreSQL
// raised it rather than answered with a fabricated result. This is the fallback
// behind every statement shape the adjudicator declines to take over.
func TestSessionReportsTheConflictWhenTheShadowFails(t *testing.T) {
	ts := newTestSession(t)
	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')

	const sql = "UPDATE t SET v = 'b' WHERE id = 1"
	ts.send(t, &pgproto3.Query{String: sql})
	if got := ts.expectBackendQuery(t); got != sql {
		t.Fatalf("backend received %q want %q", got, sql)
	}

	ts.sendBackend(t, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "55P03", Message: "canceling statement due to lock timeout"})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'E'})

	if got := ts.expectBackendQuery(t); got != "ROLLBACK TO SAVEPOINT "+savepointName {
		t.Fatalf("backend received %q want the rollback to the savepoint", got)
	}
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'T'})

	// The shadow is refused, so there is no row count to answer with.
	if got := ts.expectBackendQuery(t); got != "SELECT count(*) FROM t WHERE id = 1" {
		t.Fatalf("backend received shadow %q", got)
	}
	ts.sendBackend(t, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "42P01", Message: `relation "t" does not exist`})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'E'})

	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected the conflict to reach the client")
	}
	if er.Code != "40001" || !strings.Contains(er.Message, "OC000") {
		t.Fatalf("got %s %q want 40001 with OC000", er.Code, er.Message)
	}

	// The transaction is failed, as a statement-level failure leaves it.
	if got := ts.expectBackendQuery(t); got != "SELECT 1/0" {
		t.Fatalf("backend received %q want the statement that fails the transaction", got)
	}
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'E'})
	rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatal("expected ReadyForQuery after the failed repair")
	}
	if rfq.TxStatus != 'E' {
		t.Fatalf("got tx status %q want E", rfq.TxStatus)
	}
}

// A client that pipelines sends its next batch before the previous one is
// answered, which is what Npgsql does: it defers BEGIN and sends it with the
// first command. The emulator writes into the same stream, so anything it
// injects after the fact lands behind those messages, and the hidden exchange
// that was meant to swallow its own answer swallows the client's instead.
// Nothing may be written into that gap.
func TestSessionWritesNothingIntoAPipelinedBatch(t *testing.T) {
	ts := newTestSession(t)

	// Both batches reach the backend before either is answered.
	ts.send(t, &pgproto3.Parse{Name: "b", Query: "BEGIN"})
	assertBackend[*pgproto3.Parse](t, ts, "BEGIN Parse")
	ts.send(t, &pgproto3.Bind{DestinationPortal: "pb", PreparedStatement: "b"})
	assertBackend[*pgproto3.Bind](t, ts, "BEGIN Bind")
	ts.send(t, &pgproto3.Execute{Portal: "pb"})
	assertBackend[*pgproto3.Execute](t, ts, "BEGIN Execute")
	ts.send(t, &pgproto3.Sync{})
	assertBackend[*pgproto3.Sync](t, ts, "BEGIN Sync")

	ts.send(t, &pgproto3.Parse{Name: "u", Query: "UPDATE t SET v = 'x' WHERE id = 1"})
	assertBackend[*pgproto3.Parse](t, ts, "UPDATE Parse")
	ts.send(t, &pgproto3.Bind{DestinationPortal: "pu", PreparedStatement: "u"})
	assertBackend[*pgproto3.Bind](t, ts, "UPDATE Bind")
	ts.send(t, &pgproto3.Execute{Portal: "pu"})
	assertBackend[*pgproto3.Execute](t, ts, "UPDATE Execute")
	ts.send(t, &pgproto3.Sync{})
	assertBackend[*pgproto3.Sync](t, ts, "UPDATE Sync")

	// The backend answers both batches in order. Every message must reach the
	// client, in order, with nothing swallowed and nothing added.
	answer := []pgproto3.BackendMessage{
		&pgproto3.ParseComplete{}, &pgproto3.BindComplete{},
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
		&pgproto3.ParseComplete{}, &pgproto3.BindComplete{},
		&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42P01", Message: `relation "t" does not exist`},
		&pgproto3.ReadyForQuery{TxStatus: 'E'},
	}
	for _, want := range answer {
		ts.sendBackend(t, want)
		got := ts.receive(t)
		if gotType, wantType := fmt.Sprintf("%T", got), fmt.Sprintf("%T", want); gotType != wantType {
			t.Fatalf("client received %s where the backend sent %s", gotType, wantType)
		}
		if er, ok := got.(*pgproto3.ErrorResponse); ok && er.Code != "42P01" {
			t.Fatalf("client received %s, want the backend's own 42P01", er.Code)
		}
	}
}

// assertBackend reads the next backend message and requires it to be the
// client's own, so an injected statement fails the test rather than being
// serviced by the harness.
func assertBackend[T pgproto3.FrontendMessage](t *testing.T, ts *testSession, what string) {
	t.Helper()
	msg := ts.receiveBackend(t)
	if _, ok := msg.(T); !ok {
		if q, isQuery := msg.(*pgproto3.Query); isQuery {
			t.Fatalf("the emulator wrote %q into a pipelined batch, before the client's %s", q.String, what)
		}
		t.Fatalf("backend received %T, want the client's %s", msg, what)
	}
}

// With nothing outstanding the savepoint is established, and it goes ahead of
// the Parse rather than between it and the Bind, because a simple query
// destroys the unnamed prepared statement a Bind is about to use.
func TestSessionEstablishesTheSavepointAheadOfTheParse(t *testing.T) {
	ts := newTestSession(t)
	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')

	ts.send(t, &pgproto3.Parse{Name: "", Query: "UPDATE t SET v = 'x' WHERE id = 1"})

	if got := ts.expectBackendQueryRaw(t); got != "SAVEPOINT "+savepointName {
		t.Fatalf("backend received %q first, want the savepoint before the Parse", got)
	}
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("SAVEPOINT")})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'T'})

	if _, ok := ts.receiveBackend(t).(*pgproto3.Parse); !ok {
		t.Fatal("expected the client's Parse after the savepoint")
	}

	// The savepoint is established once, so a second statement adds nothing.
	ts.send(t, &pgproto3.Parse{Name: "", Query: "UPDATE t SET v = 'y' WHERE id = 2"})
	if _, ok := ts.receiveBackend(t).(*pgproto3.Parse); !ok {
		t.Fatal("a second savepoint was written for the same transaction")
	}
}

// A client that pipelines leaves no room to establish a savepoint, so the
// transaction is repaired by being started again. That is only sound while the
// refused statement is the only one the transaction holds -- which is exactly
// the case a pipelining client creates, because it sends BEGIN with the command
// that follows it.
func TestSessionRestartsATransactionItCouldNotSavepoint(t *testing.T) {
	ts := newTestSession(t)

	// BEGIN and the statement reach the backend together, so nothing can be
	// written between them.
	for _, msg := range []pgproto3.FrontendMessage{
		&pgproto3.Parse{Name: "b", Query: "BEGIN"},
		&pgproto3.Bind{DestinationPortal: "pb", PreparedStatement: "b"},
		&pgproto3.Execute{Portal: "pb"},
		&pgproto3.Sync{},
		&pgproto3.Parse{Name: "u", Query: "UPDATE t SET v = 'x' WHERE id = 1"},
		&pgproto3.Bind{DestinationPortal: "pu", PreparedStatement: "u"},
		&pgproto3.Execute{Portal: "pu"},
		&pgproto3.Sync{},
	} {
		ts.send(t, msg)
		if got := ts.receiveBackend(t); fmt.Sprintf("%T", got) != fmt.Sprintf("%T", msg) {
			t.Fatalf("backend received %T, want the client's %T", got, msg)
		}
	}

	// The BEGIN succeeds.
	for _, msg := range []pgproto3.BackendMessage{
		&pgproto3.ParseComplete{}, &pgproto3.BindComplete{},
		&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")},
		&pgproto3.ReadyForQuery{TxStatus: 'T'},
	} {
		ts.sendBackend(t, msg)
		ts.receive(t)
	}

	// The statement is refused a lock.
	ts.sendBackend(t, &pgproto3.ParseComplete{})
	ts.receive(t)
	ts.sendBackend(t, &pgproto3.BindComplete{})
	ts.receive(t)
	ts.sendBackend(t, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "55P03", Message: "canceling statement due to lock timeout"})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'E'})

	// With no savepoint to return to, the transaction is started again.
	if got := ts.expectBackendQueryRaw(t); got != "ROLLBACK; BEGIN" {
		t.Fatalf("backend received %q, want the transaction to be restarted", got)
	}
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'T'})

	// The shadow reports what the refused statement would have.
	if got := ts.expectBackendQueryRaw(t); got != "SELECT count(*) FROM t WHERE id = 1" {
		t.Fatalf("backend received shadow %q", got)
	}
	ts.sendBackend(t, &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("count")}}})
	ts.sendBackend(t, &pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'T'})

	cc, ok := ts.receive(t).(*pgproto3.CommandComplete)
	if !ok {
		t.Fatal("expected the refused statement to be answered as if it ran")
	}
	if got := string(cc.CommandTag); got != "UPDATE 1" {
		t.Fatalf("got command tag %q want UPDATE 1", got)
	}
	if rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok || rfq.TxStatus != 'T' {
		t.Fatal("expected the transaction to stay open")
	}

	// And it fails at COMMIT, where Aurora DSQL fails it.
	ts.send(t, &pgproto3.Query{String: "COMMIT"})
	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected the commit to be refused")
	}
	if er.Code != "40001" || !strings.Contains(er.Message, "OC000") {
		t.Fatalf("got %s %q want 40001 with OC000", er.Code, er.Message)
	}
}

// A transaction that holds work of its own cannot be started again without
// losing it, so without a savepoint the conflict is reported where PostgreSQL
// raised it rather than silently discarding what came before.
func TestSessionWillNotRestartATransactionHoldingOtherWork(t *testing.T) {
	ts := newTestSession(t)

	// Everything is pipelined, so no statement gets a savepoint, and by the
	// time the second one is refused the transaction holds the first.
	batches := []pgproto3.FrontendMessage{
		&pgproto3.Parse{Name: "b", Query: "BEGIN"},
		&pgproto3.Bind{DestinationPortal: "pb", PreparedStatement: "b"},
		&pgproto3.Execute{Portal: "pb"},
		&pgproto3.Sync{},
		&pgproto3.Parse{Name: "i", Query: "INSERT INTO t (id) VALUES (1)"},
		&pgproto3.Bind{DestinationPortal: "pi", PreparedStatement: "i"},
		&pgproto3.Execute{Portal: "pi"},
		&pgproto3.Sync{},
		&pgproto3.Parse{Name: "u", Query: "UPDATE t SET v = 'x' WHERE id = 1"},
		&pgproto3.Bind{DestinationPortal: "pu", PreparedStatement: "u"},
		&pgproto3.Execute{Portal: "pu"},
		&pgproto3.Sync{},
	}
	for _, msg := range batches {
		ts.send(t, msg)
		if got := ts.receiveBackend(t); fmt.Sprintf("%T", got) != fmt.Sprintf("%T", msg) {
			t.Fatalf("backend received %T, want the client's %T", got, msg)
		}
	}

	// The BEGIN and the insert succeed.
	for range 2 {
		for _, msg := range []pgproto3.BackendMessage{
			&pgproto3.ParseComplete{}, &pgproto3.BindComplete{},
			&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")},
			&pgproto3.ReadyForQuery{TxStatus: 'T'},
		} {
			ts.sendBackend(t, msg)
			ts.receive(t)
		}
	}

	// The update is refused a lock, with the insert still to preserve.
	ts.sendBackend(t, &pgproto3.ParseComplete{})
	ts.receive(t)
	ts.sendBackend(t, &pgproto3.BindComplete{})
	ts.receive(t)
	ts.sendBackend(t, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "55P03", Message: "canceling statement due to lock timeout"})

	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected the conflict to reach the client")
	}
	if er.Code != "40001" {
		t.Fatalf("got %s want 40001", er.Code)
	}
}
