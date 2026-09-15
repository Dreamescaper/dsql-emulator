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
		{"identity column", "CREATE TABLE t (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY)"},
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
	if er.Code != "0A000" {
		t.Fatalf("got SQLSTATE %q want 0A000", er.Code)
	}
	if !strings.Contains(er.Message, "serial") {
		t.Fatalf("message %q does not mention serial", er.Message)
	}
}
