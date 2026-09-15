package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
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
func TestSessionReturnsDerivedJobIDForAsyncIndex(t *testing.T) {
	ts := newTestSession(t)

	const sql = "CREATE INDEX ASYNC idx ON t (a)"

	ts.send(t, &pgproto3.Parse{Name: "s1", Query: sql})
	parsed, ok := ts.receiveBackend(t).(*pgproto3.Parse)
	if !ok {
		t.Fatal("expected Parse at the backend")
	}
	if parsed.Query != "CREATE INDEX idx ON t (a)" {
		t.Fatalf("backend received %q, expected the ASYNC keyword removed", parsed.Query)
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
	if got, want := string(row.Values[0]), jobIDForIndex("idx"); got != want {
		t.Fatalf("job id %q want %q", got, want)
	}

	if _, ok := ts.receive(t).(*pgproto3.CommandComplete); !ok {
		t.Fatal("expected the CommandComplete to reach the client")
	}
	ts.sendBackend(t, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	if _, ok := ts.receive(t).(*pgproto3.ReadyForQuery); !ok {
		t.Fatal("expected the ReadyForQuery to reach the client")
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
