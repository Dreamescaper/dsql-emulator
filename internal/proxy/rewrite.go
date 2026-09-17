package proxy

import (
	"crypto/rand"
	"encoding/base32"
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// stripAsync removes the ASYNC keyword Aurora DSQL requires on CREATE INDEX and
// on the ALTER TABLE form that validates a constraint in the background, and
// reports which statement carried it.
//
// PostgreSQL's grammar rejects the keyword, but its lexer does not: to the
// scanner ASYNC is an ordinary identifier, so the token stream gives the
// keyword's exact bounds even though the statement will not parse. Working from
// tokens rather than from the text is what lets a simple query holding several
// statements be rewritten -- the scanner knows where each one ends -- and it
// cannot mistake the word for one inside a string literal, a comment, or a
// quoted identifier such as an index actually named "ASYNC".
//
// The ordinal it returns is the statement the job belongs to, counted in
// CommandComplete messages, so the synthesized job id is spliced onto the
// asynchronous statement's own result rather than onto the first one's.
func stripAsync(sql string) (rewritten string, statement int, ok bool) {
	scanned, err := pg_query.Scan(sql)
	if err != nil {
		return "", 0, false
	}

	var cuts [][2]int32
	found := -1
	for ordinal, stmt := range statements(scanned.GetTokens()) {
		at := asyncKeyword(sql, stmt.tokens)
		if at < 0 {
			continue
		}
		if found < 0 {
			found = ordinal
		}
		// Cut through to the next token, so the space the keyword sat in goes
		// with it and the statement reads as though it had been written
		// without it.
		through := stmt.tokens[at].GetEnd()
		if at+1 < len(stmt.tokens) {
			through = stmt.tokens[at+1].GetStart()
		}
		cuts = append(cuts, [2]int32{stmt.tokens[at].GetStart(), through})
	}
	if found < 0 {
		return "", 0, false
	}

	var out strings.Builder
	prev := int32(0)
	for _, cut := range cuts {
		out.WriteString(sql[prev:cut[0]])
		prev = cut[1]
	}
	out.WriteString(sql[prev:])
	return out.String(), found, true
}

// statement is one statement's tokens within a simple query that may hold
// several.
type statement struct {
	tokens []*pg_query.ScanToken
}

// statements splits a token stream on the semicolons that separate statements.
func statements(tokens []*pg_query.ScanToken) []statement {
	var out []statement
	start := 0
	for i, tok := range tokens {
		if tok.GetToken() != pg_query.Token_ASCII_59 {
			continue
		}
		out = append(out, statement{tokens: tokens[start:i]})
		start = i + 1
	}
	return append(out, statement{tokens: tokens[start:]})
}

// asyncKeyword returns the index of the ASYNC keyword in a statement that opens
// with a form the dialect spells with it, and -1 for anything else.
//
// What follows the word decides it, because ASYNC sits exactly where an object
// name is otherwise written. In `ALTER TABLE async ADD COLUMN b int` the word is
// a table called async, and every ALTER TABLE action that could follow a name
// begins with a keyword; the dialect's form is followed by the table's name
// instead. A CREATE INDEX is read the same way, against the three things that
// can follow the keyword there.
func asyncKeyword(sql string, tokens []*pg_query.ScanToken) int {
	switch tokenAt(tokens, 0) {
	case pg_query.Token_CREATE:
		at := 1
		if tokenAt(tokens, at) == pg_query.Token_UNIQUE {
			at++
		}
		if tokenAt(tokens, at) != pg_query.Token_INDEX || !isAsync(sql, tokens, at+1) {
			return -1
		}
		switch tokenAt(tokens, at+2) {
		// The index's name, IF NOT EXISTS, or an index with no name at all.
		case pg_query.Token_IDENT, pg_query.Token_IF_P, pg_query.Token_ON:
			return at + 1
		}
	case pg_query.Token_ALTER:
		if tokenAt(tokens, 1) != pg_query.Token_TABLE || !isAsync(sql, tokens, 2) {
			return -1
		}
		if tokenAt(tokens, 3) == pg_query.Token_IDENT {
			return 2
		}
	}
	return -1
}

func tokenAt(tokens []*pg_query.ScanToken, i int) pg_query.Token {
	if i < 0 || i >= len(tokens) {
		return pg_query.Token_NUL
	}
	return tokens[i].GetToken()
}

// isAsync reports whether the token is the bare word ASYNC. A quoted identifier
// carries its quotes in the scanned text, so an index named "ASYNC" is left
// alone.
func isAsync(sql string, tokens []*pg_query.ScanToken, i int) bool {
	if tokenAt(tokens, i) != pg_query.Token_IDENT {
		return false
	}
	tok := tokens[i]
	if tok.GetStart() < 0 || int(tok.GetEnd()) > len(sql) {
		return false
	}
	return strings.EqualFold(sql[tok.GetStart():tok.GetEnd()], "async")
}

// jobMarker returns the comment that carries a job id down to the backing
// database, which records it in sys.jobs under that id. Passing the id costs no
// extra round trip and works for a statement that names no object, such as an
// unnamed index.
func jobMarker(jobID string) string {
	return "/* dsql_job=" + jobID + " */ "
}

// jobIDEncoding renders sixteen bytes as the 26 characters Aurora DSQL uses for
// a job id. Every one of base32's 32 lowercase characters appears across the
// ids in the golden record and none of 0, 1, 8 or 9 ever does, which is the
// RFC 4648 alphabet rather than Crockford's; unpadded, sixteen bytes come to
// exactly 26 characters, the same 128 bits a UUID carries.
var jobIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// jobIDPattern is the shape of a job id. The backing database matches the same
// shape out of the marker comment, and sys.wait_for_job refuses anything else
// the way Aurora DSQL refuses it, so the three are kept in step by this one
// description of it.
const jobIDPattern = `[a-z2-7]{26}`

// JobIDLength is how many characters a job id has.
const JobIDLength = 26

// newJobID returns the identifier for one asynchronous statement, shaped the
// way Aurora DSQL shapes one so that a client storing it in a column of its own
// finds the same width against either.
func newJobID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strings.Repeat("a", JobIDLength)
	}
	return strings.ToLower(jobIDEncoding.EncodeToString(buf[:]))
}

// serverVersionNum encodes a version the way PostgreSQL's server_version_num
// does: major*10000 + minor*100 + patch. Aurora DSQL reports 16.15 as 160015,
// so a two-part version is read as major.patch.
func serverVersionNum(version string) string {
	parts := strings.SplitN(strings.TrimSpace(version), ".", 3)
	atoi := func(s string) int {
		n := 0
		for _, r := range s {
			if r < '0' || r > '9' {
				return 0
			}
			n = n*10 + int(r-'0')
		}
		return n
	}
	major := atoi(parts[0])
	minor, patch := 0, 0
	switch len(parts) {
	case 2:
		patch = atoi(parts[1])
	case 3:
		minor, patch = atoi(parts[1]), atoi(parts[2])
	}
	return strconv.Itoa(major*10000 + minor*100 + patch)
}
