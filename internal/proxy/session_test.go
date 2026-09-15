package proxy

import (
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
}

// newTestSession wires a session between two in-memory pipes and completes the
// startup exchange, leaving it ready to classify frontend messages.
func newTestSession(t *testing.T) *testSession {
	t.Helper()

	clientProxy, clientApp := net.Pipe()
	upstreamProxy, backendApp := net.Pipe()

	rs, err := rules.Default()
	if err != nil {
		t.Fatalf("load ruleset: %v", err)
	}

	s := &session{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		client:     clientProxy,
		upstream:   upstreamProxy,
		classifier: classify.New(rs),
		tracker: txn.New(txn.Limits{
			DMLRows: rs.Limits.DMLRowsPerTxn,
			MaxAge:  time.Duration(rs.Limits.TxnAgeSeconds) * time.Second,
		}),
		statements: make(map[string][]classify.Kind),
		txStatus:   'I',
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
		intercept, err := s.handshake()
		if err == nil && intercept {
			s.run()
		}
	}()

	ts.send(t, &pgproto3.StartupMessage{
		ProtocolVersion: wire.ProtocolVersion3,
		Parameters:      map[string]string{"user": "postgres", "database": "postgres"},
	})
	if _, err := ts.be.ReceiveStartupMessage(); err != nil {
		t.Fatalf("backend receive startup: %v", err)
	}
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

func TestSessionEnforcesOneDDLPerTransaction(t *testing.T) {
	ts := newTestSession(t)

	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.roundTrip(t, "CREATE TABLE a (id int)", "CREATE TABLE", 'T')

	if status := ts.expectRejectedQuery(t, "CREATE TABLE b (id int)", "0A000"); status != 'T' {
		t.Fatalf("got tx status %q want T", status)
	}
}

func TestSessionEnforcesDDLandDMLSeparation(t *testing.T) {
	t.Run("ddl then dml", func(t *testing.T) {
		ts := newTestSession(t)
		ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
		ts.roundTrip(t, "CREATE TABLE a (id int)", "CREATE TABLE", 'T')
		ts.expectRejectedQuery(t, "INSERT INTO a VALUES (1)", "0A000")
	})

	t.Run("dml then ddl", func(t *testing.T) {
		ts := newTestSession(t)
		ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
		ts.roundTrip(t, "INSERT INTO a VALUES (1)", "INSERT 0 1", 'T')
		ts.expectRejectedQuery(t, "CREATE TABLE b (id int)", "0A000")
	})
}

func TestSessionAllowsDDLInSeparateTransactions(t *testing.T) {
	ts := newTestSession(t)

	ts.roundTrip(t, "CREATE TABLE a (id int)", "CREATE TABLE", 'I')
	ts.roundTrip(t, "CREATE TABLE b (id int)", "CREATE TABLE", 'I')
}

func TestSessionRejectsDDLAndDMLInOneImplicitTransaction(t *testing.T) {
	ts := newTestSession(t)

	ts.expectRejectedQuery(t, "CREATE TABLE a (id int); INSERT INTO a VALUES (1)", "0A000")
}

func TestSessionEnforcesRowCapAndAllowsRollback(t *testing.T) {
	ts := newTestSession(t)

	ts.roundTrip(t, "BEGIN", "BEGIN", 'T')
	ts.roundTrip(t, "DELETE FROM big", "DELETE 5000", 'T')

	if status := ts.expectRejectedQuery(t, "SELECT 1", "54000"); status != 'T' {
		t.Fatalf("got tx status %q want T", status)
	}

	// The client must still be able to escape the transaction.
	ts.roundTrip(t, "ROLLBACK", "ROLLBACK", 'I')
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
