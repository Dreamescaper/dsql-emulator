package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/Dreamescaper/dsql-emulator/internal/classify"
	"github.com/Dreamescaper/dsql-emulator/internal/txn"
	"github.com/Dreamescaper/dsql-emulator/internal/wire"
	"github.com/Dreamescaper/dsql-emulator/rules"
)

type testSession struct {
	s       *session
	client  net.Conn
	backend net.Conn
	fe      *pgproto3.Frontend
	be      *pgproto3.Backend
	startup *pgproto3.StartupMessage
	// savepointDone tracks the savepoint the adjudicator establishes once per
	// transaction, which a real backend answers and a real client never sees.
	savepointDone bool
}

// newTestSession wires a session between two in-memory pipes and completes the
// startup exchange, leaving it ready to classify frontend messages.
func newTestSession(t *testing.T) *testSession {
	t.Helper()
	return newTestSessionWith(t, nil)
}

// newTestSessionWith builds a session from an optional ruleset, defaulting to
// the embedded one.
func newTestSessionWith(t *testing.T, custom *rules.Ruleset) *testSession {
	t.Helper()

	clientProxy, clientApp := net.Pipe()
	upstreamProxy, backendApp := net.Pipe()

	rs := custom
	if rs == nil {
		var err error
		rs, err = rules.Default()
		if err != nil {
			t.Fatalf("load ruleset: %v", err)
		}
	}

	s := &session{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		client:     clientProxy,
		upstream:   upstreamProxy,
		classifier: classify.New(rs),
		tracker: txn.New(txn.Limits{
			MaxAge: time.Duration(rs.Limits.TxnAgeSeconds) * time.Second,
		}),
		serverVersion: DefaultServerVersion,
		statements:    make(map[string]statementInfo),
		txStatus:      'I',
	}

	ts := &testSession{
		s:       s,
		client:  clientApp,
		backend: backendApp,
		fe:      pgproto3.NewFrontend(clientApp, clientApp),
		be:      pgproto3.NewBackend(backendApp, backendApp),
	}
	t.Cleanup(func() {
		_ = clientProxy.Close()
		_ = clientApp.Close()
		_ = upstreamProxy.Close()
		_ = backendApp.Close()
	})

	go func() {
		intercept, err := s.handshake(context.Background())
		if err == nil && intercept {
			s.run()
		}
	}()

	ts.send(t, &pgproto3.StartupMessage{
		ProtocolVersion: wire.ProtocolVersion3,
		Parameters:      map[string]string{"user": "postgres", "database": "postgres"},
	})
	msg, err := ts.be.ReceiveStartupMessage()
	if err != nil {
		t.Fatalf("backend receive startup: %v", err)
	}
	startup, ok := msg.(*pgproto3.StartupMessage)
	if !ok {
		t.Fatalf("expected a startup message, got %T", msg)
	}
	ts.startup = startup
	return ts
}

func (ts *testSession) send(t *testing.T, msg pgproto3.FrontendMessage) {
	t.Helper()
	ts.fe.Send(msg)
	if err := ts.fe.Flush(); err != nil {
		t.Fatalf("flush frontend: %v", err)
	}
}

func (ts *testSession) sendBackend(t *testing.T, msg pgproto3.BackendMessage) {
	t.Helper()
	ts.be.Send(msg)
	if err := ts.be.Flush(); err != nil {
		t.Fatalf("flush backend: %v", err)
	}
}

// serviceSavepoint answers the savepoint the adjudicator establishes when a
// transaction opens. The client's own ReadyForQuery is withheld until it is in
// place, so a test that opens a transaction has to answer it.
func (ts *testSession) serviceSavepoint(t *testing.T, status byte) {
	t.Helper()
	if status == 'I' {
		ts.savepointDone = false
		return
	}
	if status != 'T' || ts.savepointDone {
		return
	}
	ts.savepointDone = true

	if got := ts.expectBackendQuery(t); got != "SAVEPOINT "+savepointName {
		t.Fatalf("backend received %q want the adjudicator's savepoint", got)
	}
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("SAVEPOINT")})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'T'})
}

func (ts *testSession) receive(t *testing.T) pgproto3.BackendMessage {
	t.Helper()
	_ = ts.client.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := ts.fe.Receive()
	if err != nil {
		t.Fatalf("client receive: %v", err)
	}
	_ = ts.client.SetReadDeadline(time.Time{})
	return msg
}

func (ts *testSession) receiveBackend(t *testing.T) pgproto3.FrontendMessage {
	t.Helper()
	_ = ts.backend.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := ts.be.Receive()
	if err != nil {
		t.Fatalf("backend receive: %v", err)
	}
	_ = ts.backend.SetReadDeadline(time.Time{})
	return msg
}

func TestSessionRejectsUnsupportedSimpleQuery(t *testing.T) {
	ts := newTestSession(t)

	ts.send(t, &pgproto3.Query{String: "TRUNCATE widget"})

	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected ErrorResponse, got different message")
	}
	if er.Code != "0A000" {
		t.Fatalf("got SQLSTATE %q want 0A000", er.Code)
	}

	rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatal("expected ReadyForQuery after rejected simple query")
	}
	if rfq.TxStatus != 'I' {
		t.Fatalf("got tx status %q want I", rfq.TxStatus)
	}

	_ = ts.backend.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, err := ts.backend.Read(make([]byte, 1)); err == nil {
		t.Fatalf("rejected query was forwarded to backend (%d bytes)", n)
	}
}

func TestSessionRejectsUnsupportedParseAndSkipsToSync(t *testing.T) {
	ts := newTestSession(t)

	ts.send(t, &pgproto3.Parse{Name: "s1", Query: "CREATE TRIGGER tr AFTER INSERT ON t EXECUTE FUNCTION f()"})

	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected ErrorResponse for rejected Parse")
	}
	if er.Code != "0A000" {
		t.Fatalf("got SQLSTATE %q want 0A000", er.Code)
	}

	ts.send(t, &pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "s1"})
	ts.send(t, &pgproto3.Execute{Portal: "p1"})
	ts.send(t, &pgproto3.Sync{})

	if msg := ts.receiveBackend(t); msg == nil {
		t.Fatal("expected Sync forwarded to backend")
	} else if _, ok := msg.(*pgproto3.Sync); !ok {
		t.Fatalf("expected Sync forwarded to backend, got %T", msg)
	}

	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected ReadyForQuery from backend to reach the client")
	}
}

func TestSessionForwardsSupportedStatements(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"select", "SELECT 1"},
		{"foreign key", "CREATE TABLE t (id int PRIMARY KEY, parent_id int REFERENCES parent(id))"},
		{"deferrable foreign key", "CREATE TABLE t (id int, parent_id int REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)"},
		{"identity column with cache", "CREATE TABLE t (id bigint GENERATED ALWAYS AS IDENTITY (CACHE 65536) PRIMARY KEY)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestSession(t)
			ts.send(t, &pgproto3.Query{String: tc.sql})

			msg := ts.receiveBackend(t)
			q, ok := msg.(*pgproto3.Query)
			if !ok {
				t.Fatalf("expected Query forwarded, got %T", msg)
			}
			if q.String != tc.sql {
				t.Fatalf("forwarded %q want %q", q.String, tc.sql)
			}
		})
	}
}

func TestSessionRejectsSerialColumn(t *testing.T) {
	ts := newTestSession(t)

	ts.send(t, &pgproto3.Query{String: "CREATE TABLE t (id serial PRIMARY KEY)"})

	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected ErrorResponse for serial column")
	}
	if er.Code != "42704" {
		t.Fatalf("got SQLSTATE %q want 42704", er.Code)
	}
	if !strings.Contains(er.Message, "serial") {
		t.Fatalf("message %q does not mention serial", er.Message)
	}
}

// roundTrip sends a simple query, answers it as the backend would, and drains
// the forwarded response from the client side. Backend messages are sent one at
// a time because the session forwards each one synchronously.
func (ts *testSession) roundTrip(t *testing.T, sql, tag string, status byte) {
	t.Helper()

	ts.send(t, &pgproto3.Query{String: sql})
	if got := ts.expectBackendQuery(t); got != sql {
		t.Fatalf("backend received %q want %q", got, sql)
	}

	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte(tag)})
	if _, ok := ts.receive(t).(*pgproto3.CommandComplete); !ok {
		t.Fatal("expected CommandComplete to reach the client")
	}

	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: status})
	ts.serviceSavepoint(t, status)
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected ReadyForQuery to reach the client")
	}
}

// expectRejectedQuery asserts the query is refused with code and returns the
// transaction status reported alongside the error.
func (ts *testSession) expectRejectedQuery(t *testing.T, sql, code string) byte {
	t.Helper()

	ts.send(t, &pgproto3.Query{String: sql})
	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("%q: expected ErrorResponse", sql)
	}
	if er.Code != code {
		t.Fatalf("%q: got SQLSTATE %q want %q (%s)", sql, er.Code, code, er.Message)
	}
	rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("%q: expected ReadyForQuery after rejection", sql)
	}
	return rfq.TxStatus
}

func (ts *testSession) expectBackendQuery(t *testing.T) string {
	t.Helper()
	msg := ts.receiveBackend(t)
	q, ok := msg.(*pgproto3.Query)
	if !ok {
		t.Fatalf("expected Query at the backend, got %T", msg)
	}
	return q.String
}

// expectRejectedInTxn asserts a statement is refused inside a transaction, and
// then plays the upstream side of the abort the session injects.
func (ts *testSession) expectRejectedInTxn(t *testing.T, sql, code string) {
	t.Helper()

	ts.send(t, &pgproto3.Query{String: sql})
	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("%q: expected ErrorResponse", sql)
	}
	if er.Code != code {
		t.Fatalf("%q: got SQLSTATE %q want %q (%s)", sql, er.Code, code, er.Message)
	}
	ts.expectAbort(t)
}

// expectAbort reads the deliberate failing statement the session sends to fail
// the upstream transaction, answers it, and checks the client sees a
// failed-transaction ReadyForQuery.
func (ts *testSession) expectAbort(t *testing.T) {
	t.Helper()

	msg := ts.receiveBackend(t)
	q, ok := msg.(*pgproto3.Query)
	if !ok || q.String != "SELECT 1/0" {
		t.Fatalf("expected the abort statement at the backend, got %T", msg)
	}
	ts.sendBackend(t, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "25P02", Message: "aborted"})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'E'})

	rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatal("expected ReadyForQuery to complete the rejected statement")
	}
	if rfq.TxStatus != 'E' {
		t.Fatalf("got tx status %q want E", rfq.TxStatus)
	}
}

// expectFailedQuery asserts the client is told the transaction is already
// failed.
func (ts *testSession) expectFailedQuery(t *testing.T, sql string) {
	t.Helper()

	ts.send(t, &pgproto3.Query{String: sql})
	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("%q: expected ErrorResponse", sql)
	}
	if er.Code != "25P02" {
		t.Fatalf("%q: got SQLSTATE %q want 25P02", sql, er.Code)
	}
	rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
	if !ok {
		t.Fatalf("%q: expected ReadyForQuery", sql)
	}
	if rfq.TxStatus != 'E' {
		t.Fatalf("%q: got tx status %q want E", sql, rfq.TxStatus)
	}
}

func TestSessionEnforcesOneDDLPerTransaction(t *testing.T) {
	ts := newTestSession(t)

	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.roundTrip(t, "CREATE TABLE a (id int)", "CREATE TABLE", 'T')

	ts.expectRejectedInTxn(t, "CREATE TABLE b (id int)", "0A000")
}

func TestSessionEnforcesDDLandDMLSeparation(t *testing.T) {
	t.Run("ddl then dml", func(t *testing.T) {
		ts := newTestSession(t)
		ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
		ts.roundTrip(t, "CREATE TABLE a (id int)", "CREATE TABLE", 'T')
		ts.expectRejectedInTxn(t, "INSERT INTO a VALUES (1)", "0A000")
	})

	t.Run("dml then ddl", func(t *testing.T) {
		ts := newTestSession(t)
		ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
		ts.roundTrip(t, "INSERT INTO a VALUES (1)", "INSERT 0 1", 'T')
		ts.expectRejectedInTxn(t, "CREATE TABLE b (id int)", "0A000")
	})
}

func TestSessionAllowsDDLInSeparateTransactions(t *testing.T) {
	ts := newTestSession(t)

	ts.roundTrip(t, "CREATE TABLE a (id int)", "CREATE TABLE", 'I')
	ts.roundTrip(t, "CREATE TABLE b (id int)", "CREATE TABLE", 'I')
}

func TestSessionRejectsDDLAndDMLInOneImplicitTransaction(t *testing.T) {
	ts := newTestSession(t)

	// No explicit transaction, so a refusal does not fail a transaction.
	ts.expectRejectedQuery(t, "CREATE TABLE a (id int); INSERT INTO a VALUES (1)", "0A000")
}

func TestSessionCommitOnFailedTransactionRollsBack(t *testing.T) {
	ts := newTestSession(t)

	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.expectRejectedInTxn(t, "SET TRANSACTION READ ONLY", "0A000")
	ts.expectFailedQuery(t, "SELECT 1")

	// The upstream transaction is already failed, so forwarding COMMIT makes
	// PostgreSQL report a ROLLBACK, which is what DSQL does too.
	ts.send(t, &pgproto3.Query{String: "COMMIT"})
	if got := ts.expectBackendQuery(t); got != "COMMIT" {
		t.Fatalf("backend received %q", got)
	}
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
	cc, ok := ts.receive(t).(*pgproto3.CommandComplete)
	if !ok || string(cc.CommandTag) != "ROLLBACK" {
		t.Fatalf("got %v want ROLLBACK command tag", cc)
	}
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected ReadyForQuery at the client")
	}

	// The transaction is over; the session is usable again.
	ts.roundTrip(t, "SELECT 1", "SELECT 1", 'I')
}

const occRuleset = `
dsql_version: "test"
isolation:
  supported: ["repeatable read"]
limits:
  dml_rows_per_txn: 3000
  txn_age_seconds: 1800
occ:
  error: "change conflicts with another transaction (OC000)"
  sqlstate: "40001"
  inject:
    - id: inject_occ
      tables: ["occ_t"]
      every: 1
`

func TestSessionAdvertisesRowCapUpstream(t *testing.T) {
	ts := newTestSession(t)

	options := ts.startup.Parameters["options"]
	if !strings.Contains(options, "dsql.row_cap=3000") {
		t.Fatalf("startup options %q do not carry the row cap", options)
	}
	if got := ts.startup.Parameters["default_transaction_isolation"]; got != "repeatable read" {
		t.Fatalf("got isolation %q want repeatable read", got)
	}
}

func TestSessionInjectsOccConflictAtCommit(t *testing.T) {
	rs, err := rules.Load(strings.NewReader(occRuleset))
	if err != nil {
		t.Fatalf("load ruleset: %v", err)
	}
	ts := newTestSessionWith(t, rs)

	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.roundTrip(t, "INSERT INTO occ_t VALUES (1)", "INSERT 0 1", 'T')

	// The commit is refused, the upstream transaction is rolled back, and the
	// transaction ends cleanly.
	ts.send(t, &pgproto3.Query{String: "COMMIT"})
	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected a conflict error")
	}
	if er.Code != "40001" || !strings.Contains(er.Message, "OC000") {
		t.Fatalf("got %s %q want 40001 with OC000", er.Code, er.Message)
	}

	msg := ts.receiveBackend(t)
	q, ok := msg.(*pgproto3.Query)
	if !ok || q.String != "ROLLBACK" {
		t.Fatalf("expected ROLLBACK at the backend, got %T", msg)
	}
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'I'})

	rfq, ok := ts.receive(t).(*pgproto3.ReadyForQuery)
	if !ok || rfq.TxStatus != 'I' {
		t.Fatalf("expected idle ReadyForQuery, got %v", rfq)
	}
}

// Two injection rules that match the same commit must each keep their own
// count, or one rule's cadence is advanced by the other's matches.
const twoRuleOccRuleset = `
dsql_version: "test"
isolation:
  supported: ["repeatable read"]
limits:
  dml_rows_per_txn: 3000
  txn_age_seconds: 1800
occ:
  error: "change conflicts with another transaction (OC000)"
  sqlstate: "40001"
  inject:
    - id: every_third
      tables: ["occ_t"]
      every: 3
    - id: every_other
      tables: ["occ_t"]
      every: 2
`

func TestSessionCountsEachOccInjectionSeparately(t *testing.T) {
	rs, err := rules.Load(strings.NewReader(twoRuleOccRuleset))
	if err != nil {
		t.Fatalf("load ruleset: %v", err)
	}
	ts := newTestSessionWith(t, rs)

	// Neither rule fires on the first commit: one is on its first of three, the
	// other on its first of two.
	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.roundTrip(t, "INSERT INTO occ_t VALUES (1)", "INSERT 0 1", 'T')
	ts.roundTrip(t, "COMMIT", "COMMIT", 'I')

	// The second commit is the second match for every_other, so it conflicts.
	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.roundTrip(t, "INSERT INTO occ_t VALUES (2)", "INSERT 0 1", 'T')
	ts.send(t, &pgproto3.Query{String: "COMMIT"})
	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected a conflict on the second commit")
	}
	if er.Code != "40001" {
		t.Fatalf("got %s want 40001", er.Code)
	}
}

func TestSessionInjectsOccOnlyForMatchingTables(t *testing.T) {
	rs, err := rules.Load(strings.NewReader(occRuleset))
	if err != nil {
		t.Fatalf("load ruleset: %v", err)
	}
	ts := newTestSessionWith(t, rs)

	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.roundTrip(t, "INSERT INTO other_t VALUES (1)", "INSERT 0 1", 'T')
	ts.roundTrip(t, "COMMIT", "COMMIT", 'I')
}

func TestSessionRewritesSerializationFailure(t *testing.T) {
	ts := newTestSession(t)

	ts.sendBackend(t, &pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     "40001",
		Message:  "could not serialize access due to concurrent update",
	})

	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected an error response")
	}
	if er.Code != "40001" {
		t.Fatalf("got SQLSTATE %q want 40001", er.Code)
	}
	if !strings.Contains(er.Message, "OC000") {
		t.Fatalf("message %q does not carry the OCC code", er.Message)
	}
}

// TestSessionReturnsDerivedJobIDForAsyncIndex covers the whole Create Index
// Async path: the statement is rewritten before it reaches the backend, and the
// job id handed to the client is the one the backing database derives.
func TestSessionReturnsJobIDForAsyncIndex(t *testing.T) {
	ts := newTestSession(t)

	const sql = "CREATE INDEX ASYNC idx ON t (a)"

	ts.send(t, &pgproto3.Parse{Name: "s1", Query: sql})
	parsed, ok := ts.receiveBackend(t).(*pgproto3.Parse)
	if !ok {
		t.Fatal("expected Parse at the backend")
	}
	forwardedID := markedJobID(t, parsed.Query)
	if want := jobMarker(forwardedID) + "CREATE INDEX idx ON t (a)"; parsed.Query != want {
		t.Fatalf("backend received %q want %q", parsed.Query, want)
	}
	ts.sendBackend(t, &pgproto3.ParseComplete{})
	if _, ok := ts.receive(t).(*pgproto3.ParseComplete); !ok {
		t.Fatal("expected ParseComplete at the client")
	}

	ts.bind(t, "p1", "s1")
	ts.execute(t, "p1")

	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("CREATE INDEX")})

	desc, ok := ts.receive(t).(*pgproto3.RowDescription)
	if !ok {
		t.Fatal("expected a RowDescription for the job id")
	}
	if len(desc.Fields) != 1 || string(desc.Fields[0].Name) != "job_id" {
		t.Fatalf("got fields %+v want a single job_id column", desc.Fields)
	}

	row, ok := ts.receive(t).(*pgproto3.DataRow)
	if !ok {
		t.Fatal("expected a DataRow carrying the job id")
	}
	// The id handed to the client is the one the backing database was told to
	// record the job under.
	if got := string(row.Values[0]); got != forwardedID {
		t.Fatalf("client got job id %q, but the statement carried %q", got, forwardedID)
	}

	if _, ok := ts.receive(t).(*pgproto3.CommandComplete); !ok {
		t.Fatal("expected the CommandComplete to reach the client")
	}
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected the ReadyForQuery to reach the client")
	}
}

func TestSessionForwardsPartialAsyncIndex(t *testing.T) {
	ts := newTestSession(t)

	ts.send(t, &pgproto3.Query{String: "CREATE INDEX ASYNC idx ON t (a) WHERE a IS NOT NULL"})

	query, ok := ts.receiveBackend(t).(*pgproto3.Query)
	if !ok {
		t.Fatal("expected a Query at the backend")
	}
	want := jobMarker(markedJobID(t, query.String)) + "CREATE INDEX idx ON t (a) WHERE a IS NOT NULL"
	if query.String != want {
		t.Fatalf("backend received %q want %q", query.String, want)
	}
}

// Aurora DSQL puts the index in the table's schema and its grammar does not
// accept a qualified name. PostgreSQL's grammar does not either, so the
// statement is forwarded with the name intact and the backend supplies the
// syntax error rather than the emulator hard-coding it.
func TestSessionForwardsQualifiedIndexNameForTheBackendToRefuse(t *testing.T) {
	ts := newTestSession(t)

	ts.send(t, &pgproto3.Query{String: "CREATE INDEX ASYNC public.idx ON t (a)"})

	query, ok := ts.receiveBackend(t).(*pgproto3.Query)
	if !ok {
		t.Fatal("expected the statement forwarded to the backend")
	}
	if !strings.HasSuffix(query.String, "CREATE INDEX public.idx ON t (a)") {
		t.Fatalf("backend received %q, expected the qualified name left intact", query.String)
	}

	go func() {
		ts.sendBackend(t, &pgproto3.ErrorResponse{
			Severity: "ERROR", Code: "42601", Message: `syntax error at or near "."`,
		})
		ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	}()

	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected the backend's syntax error to reach the client")
	}
	if er.Code != "42601" {
		t.Fatalf("got SQLSTATE %q want 42601 (%s)", er.Code, er.Message)
	}
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected a ReadyForQuery after the error")
	}
}

// markedJobID reads the job id the emulator attached to a forwarded statement.
func markedJobID(t *testing.T, sql string) string {
	t.Helper()
	m := jobMarkerPattern.FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("statement carries no job marker: %q", sql)
	}
	return m[1]
}

var jobMarkerPattern = regexp.MustCompile(`dsql_job=([0-9a-f-]{36})`)

func TestSessionReturnsJobIDForAsyncValidateConstraint(t *testing.T) {
	ts := newTestSession(t)

	ts.send(t, &pgproto3.Query{String: "ALTER TABLE ASYNC t VALIDATE CONSTRAINT c"})

	query, ok := ts.receiveBackend(t).(*pgproto3.Query)
	if !ok {
		t.Fatal("expected a Query at the backend")
	}
	forwardedID := markedJobID(t, query.String)
	if want := jobMarker(forwardedID) + "ALTER TABLE t VALIDATE CONSTRAINT c"; query.String != want {
		t.Fatalf("backend received %q want %q", query.String, want)
	}

	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("ALTER TABLE")})
	if _, ok := ts.receive(t).(*pgproto3.RowDescription); !ok {
		t.Fatal("expected a RowDescription for the job id")
	}
	row, ok := ts.receive(t).(*pgproto3.DataRow)
	if !ok {
		t.Fatal("expected a DataRow carrying the job id")
	}
	if got := string(row.Values[0]); got != forwardedID {
		t.Fatalf("client got job id %q, but the statement carried %q", got, forwardedID)
	}
}

func TestSessionRewritesServerVersion(t *testing.T) {
	ts := newTestSession(t)

	ts.sendBackend(t, &pgproto3.ParameterStatus{Name: "server_version", Value: "17.2"})
	msg := ts.receive(t)
	ps, ok := msg.(*pgproto3.ParameterStatus)
	if !ok {
		t.Fatalf("expected ParameterStatus, got %T", msg)
	}
	if ps.Value != DefaultServerVersion {
		t.Fatalf("got server_version %q want %q", ps.Value, DefaultServerVersion)
	}
}

func TestSessionPassesOtherParametersThrough(t *testing.T) {
	ts := newTestSession(t)

	ts.sendBackend(t, &pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	ps, ok := ts.receive(t).(*pgproto3.ParameterStatus)
	if !ok || ps.Value != "UTF8" {
		t.Fatalf("got %v want client_encoding UTF8", ps)
	}
}

func TestSessionRejectsUnsupportedIsolation(t *testing.T) {
	ts := newTestSession(t)

	ts.expectRejectedQuery(t, "BEGIN ISOLATION LEVEL SERIALIZABLE", "0A000")
}

// parse prepares a statement and relays the backend's ParseComplete.
func (ts *testSession) parse(t *testing.T, name, sql string) {
	t.Helper()
	ts.send(t, &pgproto3.Parse{Name: name, Query: sql})
	if _, ok := ts.receiveBackend(t).(*pgproto3.Parse); !ok {
		t.Fatal("expected Parse at the backend")
	}
	ts.sendBackend(t, &pgproto3.ParseComplete{})
	if _, ok := ts.receive(t).(*pgproto3.ParseComplete); !ok {
		t.Fatal("expected ParseComplete at the client")
	}
}

// bind runs a prepared statement's Bind, expecting it to be forwarded.
func (ts *testSession) bind(t *testing.T, portal, statement string) {
	t.Helper()
	ts.send(t, &pgproto3.Bind{DestinationPortal: portal, PreparedStatement: statement})
	if _, ok := ts.receiveBackend(t).(*pgproto3.Bind); !ok {
		t.Fatal("expected Bind at the backend")
	}
	ts.sendBackend(t, &pgproto3.BindComplete{})
	if _, ok := ts.receive(t).(*pgproto3.BindComplete); !ok {
		t.Fatal("expected BindComplete at the client")
	}
}

func (ts *testSession) execute(t *testing.T, portal string) {
	t.Helper()
	ts.send(t, &pgproto3.Execute{Portal: portal})
	if _, ok := ts.receiveBackend(t).(*pgproto3.Execute); !ok {
		t.Fatal("expected Execute at the backend")
	}
}

func (ts *testSession) complete(t *testing.T, tag string, status byte) {
	t.Helper()
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte(tag)})
	if _, ok := ts.receive(t).(*pgproto3.CommandComplete); !ok {
		t.Fatal("expected CommandComplete at the client")
	}
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: status})
	ts.serviceSavepoint(t, status)
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected ReadyForQuery at the client")
	}
}

// TestSessionTracksReusedPreparedStatements covers clients that cache prepared
// statements: after the first Parse, transaction control arrives as Bind with no
// Parse, and the rules must still see it.
func TestSessionTracksReusedPreparedStatements(t *testing.T) {
	ts := newTestSession(t)

	ts.parse(t, "b", "BEGIN")
	ts.bind(t, "pb", "b")
	ts.execute(t, "pb")
	ts.complete(t, "BEGIN", 'T')

	ts.parse(t, "c", "CREATE TABLE cached_t (id int)")
	ts.bind(t, "pc", "c")
	ts.execute(t, "pc")
	ts.complete(t, "CREATE TABLE", 'T')

	ts.parse(t, "m", "COMMIT")
	ts.bind(t, "pm", "m")
	ts.execute(t, "pm")
	ts.complete(t, "COMMIT", 'I')

	// Cache hits: no Parse follows for either statement.
	ts.bind(t, "pb2", "b")
	ts.execute(t, "pb2")
	ts.complete(t, "BEGIN", 'T')

	ts.bind(t, "pc2", "c")
	ts.execute(t, "pc2")
	ts.complete(t, "CREATE TABLE", 'T')

	// The cached BEGIN opened a transaction, so this DML after DDL must fail.
	ts.parse(t, "i", "INSERT INTO cached_t (id) VALUES (1)")
	ts.send(t, &pgproto3.Bind{DestinationPortal: "pi", PreparedStatement: "i"})
	er, ok := ts.receive(t).(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatal("expected the cached DML after DDL to be refused")
	}
	if er.Code != "0A000" {
		t.Fatalf("got SQLSTATE %q want 0A000", er.Code)
	}
}

// An asynchronous DDL is admitted where every other prepared statement is, at
// Bind, so a transaction's single DDL is not spent twice.
func TestSessionAllowsPreparedAsyncIndexInTransaction(t *testing.T) {
	ts := newTestSession(t)

	ts.send(t, &pgproto3.Query{String: "BEGIN"})
	if _, ok := ts.receiveBackend(t).(*pgproto3.Query); !ok {
		t.Fatal("expected BEGIN forwarded to the backend")
	}
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'T'})
	ts.serviceSavepoint(t, 'T')
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected ReadyForQuery after BEGIN")
	}

	ts.send(t, &pgproto3.Parse{Name: "s1", Query: "CREATE INDEX ASYNC idx ON t (a)"})
	parse, ok := ts.receiveBackend(t).(*pgproto3.Parse)
	if !ok {
		t.Fatal("expected Parse forwarded to the backend")
	}
	if want := jobMarker(markedJobID(t, parse.Query)) + "CREATE INDEX idx ON t (a)"; parse.Query != want {
		t.Fatalf("forwarded %q want %q", parse.Query, want)
	}

	ts.send(t, &pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "s1"})
	msg := ts.receiveBackend(t)
	if _, ok := msg.(*pgproto3.Bind); !ok {
		t.Fatalf("expected Bind forwarded to the backend, got %T", msg)
	}
}

// A failed asynchronous DDL has no job to report, so no job_id row may be
// spliced into whatever runs next.
func TestSessionDropsJobWhenAsyncIndexFails(t *testing.T) {
	ts := newTestSession(t)

	// One goroutine owns the backend side for the whole exchange, because a
	// pgproto3.Backend cannot be shared. It refuses the index build, then
	// answers the statement that follows it.
	backend := make(chan error, 1)
	go func() {
		backend <- func() error {
			if err := ts.backend.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			if _, err := ts.be.Receive(); err != nil {
				return fmt.Errorf("receive the index statement: %w", err)
			}
			ts.be.Send(&pgproto3.ErrorResponse{
				Severity: "ERROR", Code: "42P01", Message: `relation "missing" does not exist`,
			})
			ts.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := ts.be.Flush(); err != nil {
				return fmt.Errorf("refuse the index build: %w", err)
			}

			if _, err := ts.be.Receive(); err != nil {
				return fmt.Errorf("receive the select: %w", err)
			}
			ts.be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{
				Name: []byte("?column?"), DataTypeOID: 23,
			}}})
			ts.be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
			ts.be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			ts.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			return ts.be.Flush()
		}()
	}()

	ts.send(t, &pgproto3.Query{String: "CREATE INDEX ASYNC idx ON missing (a)"})
	if _, ok := ts.receive(t).(*pgproto3.ErrorResponse); !ok {
		t.Fatal("expected the backend error to reach the client")
	}
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected ReadyForQuery after the failed index build")
	}

	ts.send(t, &pgproto3.Query{String: "SELECT 1"})
	var got []string
	for {
		msg := ts.receive(t)
		got = append(got, fmt.Sprintf("%T", msg))
		if _, done := msg.(*pgproto3.ReadyForQuery); done {
			break
		}
	}
	if err := <-backend; err != nil {
		t.Fatalf("backend script: %v", err)
	}

	want := []string{
		"*pgproto3.RowDescription",
		"*pgproto3.DataRow",
		"*pgproto3.CommandComplete",
		"*pgproto3.ReadyForQuery",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("client saw %v, want %v", got, want)
	}
}

// A multi-statement simple query is what `psql -c 'a; b'` sends. The dialect's
// own rule decides it, rather than PostgreSQL's parser refusing the ASYNC
// keyword it has never heard of.
func TestSessionAppliesTheRulesToAMultiStatementAsyncQuery(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		code string
	}{
		{
			name: "two DDL in one implicit transaction",
			sql:  "CREATE TABLE t (id int); CREATE INDEX ASYNC idx ON t (id)",
			code: "0A000",
		},
		{
			name: "DDL mixed with DML",
			sql:  "INSERT INTO t (id) VALUES (1); CREATE INDEX ASYNC idx ON t (id)",
			code: "0A000",
		},
		{
			name: "two asynchronous index builds",
			sql:  "CREATE INDEX ASYNC a ON t (x); CREATE INDEX ASYNC b ON t (y)",
			code: "0A000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestSession(t)
			ts.expectRejectedQuery(t, tt.sql, tt.code)

			_ = ts.backend.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if n, err := ts.backend.Read(make([]byte, 1)); err == nil {
				t.Fatalf("the refused query reached the backend (%d bytes)", n)
			}
		})
	}
}

// One DDL in an explicit transaction is allowed, so the whole query is
// forwarded and the job id belongs to the statement that carried ASYNC, not to
// the BEGIN whose CommandComplete arrives first.
func TestSessionSplicesTheJobOntoItsOwnStatement(t *testing.T) {
	ts := newTestSession(t)

	const sql = "BEGIN; CREATE INDEX ASYNC idx ON t (a); COMMIT"
	ts.send(t, &pgproto3.Query{String: sql})

	forwarded := ts.expectBackendQuery(t)
	forwardedID := markedJobID(t, forwarded)
	if want := jobMarker(forwardedID) + "BEGIN; CREATE INDEX idx ON t (a); COMMIT"; forwarded != want {
		t.Fatalf("backend received %q want %q", forwarded, want)
	}

	// The BEGIN's own result passes through untouched.
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
	begin, ok := ts.receive(t).(*pgproto3.CommandComplete)
	if !ok {
		t.Fatal("expected the BEGIN CommandComplete at the client")
	}
	if got := string(begin.CommandTag); got != "BEGIN" {
		t.Fatalf("got tag %q want BEGIN", got)
	}

	// The index build is answered with the job id.
	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("CREATE INDEX")})
	if _, ok := ts.receive(t).(*pgproto3.RowDescription); !ok {
		t.Fatal("expected a RowDescription for the job id")
	}
	row, ok := ts.receive(t).(*pgproto3.DataRow)
	if !ok {
		t.Fatal("expected a DataRow carrying the job id")
	}
	if got := string(row.Values[0]); got != forwardedID {
		t.Fatalf("client got job id %q, but the statement carried %q", got, forwardedID)
	}
	if _, ok := ts.receive(t).(*pgproto3.CommandComplete); !ok {
		t.Fatal("expected the CREATE INDEX CommandComplete at the client")
	}

	ts.sendBackend(t, &pgproto3.CommandComplete{CommandTag: []byte("COMMIT")})
	if _, ok := ts.receive(t).(*pgproto3.CommandComplete); !ok {
		t.Fatal("expected the COMMIT CommandComplete at the client")
	}
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected the ReadyForQuery at the client")
	}
}
