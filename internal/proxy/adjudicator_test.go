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
	if _, ok := ts.receiveBackend(t).(*pgproto3.Bind); !ok {
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
