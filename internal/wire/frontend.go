package wire

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgproto3"
)

// DecodeFrontend decodes a tagged frontend message body into its pgproto3
// representation. It returns nil, nil for message types it does not model, so
// callers can forward unknown traffic untouched.
func DecodeFrontend(t byte, body []byte) (pgproto3.FrontendMessage, error) {
	var msg pgproto3.FrontendMessage
	switch t {
	case 'B':
		msg = &pgproto3.Bind{}
	case 'C':
		msg = &pgproto3.Close{}
	case 'D':
		msg = &pgproto3.Describe{}
	case 'E':
		msg = &pgproto3.Execute{}
	case 'F':
		msg = &pgproto3.FunctionCall{}
	case 'H':
		msg = &pgproto3.Flush{}
	case 'P':
		msg = &pgproto3.Parse{}
	case 'Q':
		msg = &pgproto3.Query{}
	case 'S':
		msg = &pgproto3.Sync{}
	case 'X':
		msg = &pgproto3.Terminate{}
	case 'd':
		msg = &pgproto3.CopyData{}
	case 'c':
		msg = &pgproto3.CopyDone{}
	case 'f':
		msg = &pgproto3.CopyFail{}
	case 'p':
		msg = &pgproto3.PasswordMessage{}
	default:
		return nil, nil
	}

	if err := msg.Decode(body); err != nil {
		return nil, fmt.Errorf("wire: decoding message type %q: %w", t, err)
	}
	return msg, nil
}
