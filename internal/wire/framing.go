// Package wire frames PostgreSQL protocol messages without altering them, so a
// proxy can inspect traffic and forward the original bytes verbatim.
package wire

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"
)

// maxMessageLength bounds a single framed message. PostgreSQL's own limit is
// 1GB; this is a safety bound against corrupt or hostile length prefixes.
const maxMessageLength = 1 << 28

// Protocol codes carried in the first four bytes of an untagged startup message.
const (
	ProtocolVersion3  = 196608
	ProtocolVersion32 = 196610
	SSLRequestCode    = 80877103
	GSSENCRequest     = 80877104
	CancelRequest     = 80877102
)

// IsStartup reports whether code identifies an opening startup message rather
// than a special request.
func IsStartup(code int32) bool {
	return code == ProtocolVersion3 || code == ProtocolVersion32
}

// Message is one framed protocol message. Raw is the complete frame, including
// the type byte and the length prefix, and is what a proxy should forward.
type Message struct {
	Type byte
	Body []byte
	Raw  []byte
}

// ReadTagged reads one tagged message: a type byte, an int32 length that counts
// itself, and the message body.
func ReadTagged(r io.Reader) (Message, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Message{}, err
	}
	t := hdr[0]
	length := int(binary.BigEndian.Uint32(hdr[1:5]))
	if length < 4 || length > maxMessageLength {
		return Message{}, fmt.Errorf("wire: invalid length %d for message type %q", length, t)
	}

	body := make([]byte, length-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return Message{}, fmt.Errorf("wire: reading body of message type %q: %w", t, err)
	}

	raw := make([]byte, 0, length+1)
	raw = append(raw, hdr[:]...)
	raw = append(raw, body...)
	return Message{Type: t, Body: body, Raw: raw}, nil
}

// Startup is an untagged message that opens a connection: a length prefix
// followed by a protocol version or a special request code.
type Startup struct {
	Code int32
	Body []byte
	Raw  []byte
}

// ReadStartup reads one untagged startup message.
func ReadStartup(r io.Reader) (Startup, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return Startup{}, err
	}
	length := int(binary.BigEndian.Uint32(lenBuf[:]))
	if length < 8 || length > maxMessageLength {
		return Startup{}, fmt.Errorf("wire: invalid startup length %d", length)
	}

	rest := make([]byte, length-4)
	if _, err := io.ReadFull(r, rest); err != nil {
		return Startup{}, fmt.Errorf("wire: reading startup message: %w", err)
	}

	raw := make([]byte, 0, length)
	raw = append(raw, lenBuf[:]...)
	raw = append(raw, rest...)
	return Startup{Code: int32(binary.BigEndian.Uint32(rest[:4])), Body: rest, Raw: raw}, nil
}

// RewriteStartup re-encodes a startup message with parameters set and options
// appended to any the client already sent. If the message cannot be decoded it
// is returned unchanged, so malformed input still reaches the server.
func RewriteStartup(startup Startup, params map[string]string, options ...string) []byte {
	var msg pgproto3.StartupMessage
	if err := msg.Decode(startup.Body); err != nil {
		return startup.Raw
	}
	if msg.Parameters == nil {
		msg.Parameters = make(map[string]string)
	}
	for key, value := range params {
		msg.Parameters[key] = value
	}
	if len(options) > 0 {
		all := options
		if existing := msg.Parameters["options"]; existing != "" {
			all = append([]string{existing}, options...)
		}
		msg.Parameters["options"] = strings.Join(all, " ")
	}

	encoded, err := msg.Encode(nil)
	if err != nil {
		return startup.Raw
	}
	return encoded
}
