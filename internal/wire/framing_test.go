package wire

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

func TestReadTaggedRoundTrips(t *testing.T) {
	encoded, err := (&pgproto3.Query{String: "SELECT 1"}).Encode(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	msg, err := ReadTagged(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if msg.Type != 'Q' {
		t.Fatalf("got type %q want Q", msg.Type)
	}
	if !bytes.Equal(msg.Raw, encoded) {
		t.Fatalf("raw frame changed: got %v want %v", msg.Raw, encoded)
	}

	var query pgproto3.Query
	if err := query.Decode(msg.Body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if query.String != "SELECT 1" {
		t.Fatalf("got %q want %q", query.String, "SELECT 1")
	}
}

func TestReadTaggedRejectsBadLength(t *testing.T) {
	raw := []byte{'Q', 0, 0, 0, 1}
	if _, err := ReadTagged(bytes.NewReader(raw)); err == nil {
		t.Fatal("expected error for length below the header size")
	}
}

func TestReadStartup(t *testing.T) {
	encoded, err := (&pgproto3.StartupMessage{
		ProtocolVersion: ProtocolVersion3,
		Parameters:      map[string]string{"user": "postgres"},
	}).Encode(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	startup, err := ReadStartup(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if startup.Code != ProtocolVersion3 {
		t.Fatalf("got code %d want %d", startup.Code, ProtocolVersion3)
	}
	if !bytes.Equal(startup.Raw, encoded) {
		t.Fatalf("raw frame changed: got %v want %v", startup.Raw, encoded)
	}
}

func TestRewriteStartupSetsParametersAndOptions(t *testing.T) {
	startup, err := ReadStartup(bytes.NewReader(func() []byte {
		encoded, _ := (&pgproto3.StartupMessage{
			ProtocolVersion: ProtocolVersion3,
			Parameters:      map[string]string{"user": "admin", "options": "-c foo=1"},
		}).Encode(nil)
		return encoded
	}()))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	raw := RewriteStartup(startup,
		map[string]string{"default_transaction_isolation": "repeatable read"},
		"-c dsql.row_cap=3000")

	var rewritten pgproto3.StartupMessage
	if err := rewritten.Decode(raw[4:]); err != nil {
		t.Fatalf("decode rewritten: %v", err)
	}
	if rewritten.Parameters["default_transaction_isolation"] != "repeatable read" {
		t.Fatalf("parameter not set: %v", rewritten.Parameters)
	}
	want := "-c foo=1 -c dsql.row_cap=3000"
	if got := rewritten.Parameters["options"]; got != want {
		t.Fatalf("got options %q want %q", got, want)
	}
}

func TestReadStartupSSLRequest(t *testing.T) {
	raw := make([]byte, 8)
	binary.BigEndian.PutUint32(raw[0:4], 8)
	binary.BigEndian.PutUint32(raw[4:8], SSLRequestCode)

	startup, err := ReadStartup(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if startup.Code != SSLRequestCode {
		t.Fatalf("got code %d want %d", startup.Code, SSLRequestCode)
	}
}
