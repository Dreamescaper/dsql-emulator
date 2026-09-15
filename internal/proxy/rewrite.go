package proxy

import (
	"crypto/md5"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// ident matches a SQL identifier, quoted or plain.
const ident = `"(?:[^"]|"")*"|[A-Za-z_][A-Za-z0-9_$]*`

var (
	// asyncIndexPattern matches the ASYNC keyword that Aurora DSQL requires on
	// CREATE INDEX but PostgreSQL's parser does not understand. The rewrite is
	// textual because libpg_query rejects the statement outright; everything
	// except the keyword is preserved.
	asyncIndexPattern = regexp.MustCompile(`(?is)^(\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+)ASYNC\s+`)
	// asyncIndexNamePattern captures the index name, optionally qualified.
	asyncIndexNamePattern = regexp.MustCompile(
		`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+ASYNC\s+(?:IF\s+NOT\s+EXISTS\s+)?(` + ident + `)(?:\s*\.\s*(` + ident + `))?\s+ON\b`)
)

// asyncIndex is a parsed CREATE INDEX ASYNC statement.
type asyncIndex struct {
	rewritten string
	// name is the unqualified index name, or "" when the statement omits one
	// and lets the server choose.
	name string
	// qualified reports that the index name carried a schema. Aurora DSQL does
	// not allow that: the index always lands in the table's schema.
	qualified bool
}

// parseAsyncIndex recognises a CREATE INDEX ASYNC statement, strips the keyword
// that PostgreSQL's parser does not understand, and reads the index name. Both
// the emulator and the backing database derive the job id from that name, so
// the id returned to the client matches the row the database records.
func parseAsyncIndex(sql string) (asyncIndex, bool) {
	if !asyncIndexPattern.MatchString(sql) {
		return asyncIndex{}, false
	}
	rewritten := asyncIndexPattern.ReplaceAllString(sql, "${1}")
	if strings.EqualFold(rewritten, sql) {
		return asyncIndex{}, false
	}

	out := asyncIndex{rewritten: rewritten}
	if m := asyncIndexNamePattern.FindStringSubmatch(sql); m != nil {
		out.name = unquoteIdent(m[2])
		if out.name == "" {
			out.name = unquoteIdent(m[1])
		}
		out.qualified = m[2] != ""
	}
	return out, true
}

// unquoteIdent folds an identifier the way PostgreSQL would: a quoted name keeps
// its case and escapes, an unquoted one is lowercased.
func unquoteIdent(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return strings.ToLower(s)
}

// asyncAlter is a parsed ALTER TABLE ASYNC statement. Aurora DSQL uses the ASYNC
// form to validate a constraint asynchronously, which PostgreSQL's parser does
// not understand.
type asyncAlter struct {
	rewritten string
	// table is the unqualified table name, or "" when it could not be read.
	table string
}

var (
	asyncAlterPattern     = regexp.MustCompile(`(?is)^(\s*ALTER\s+TABLE\s+)ASYNC\s+`)
	asyncAlterNamePattern = regexp.MustCompile(
		`(?is)^\s*ALTER\s+TABLE\s+ASYNC\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?(` + ident + `)(?:\s*\.\s*(` + ident + `))?`)
)

// parseAsyncAlterTable recognises ALTER TABLE ASYNC and strips the keyword.
func parseAsyncAlterTable(sql string) (asyncAlter, bool) {
	if !asyncAlterPattern.MatchString(sql) {
		return asyncAlter{}, false
	}
	rewritten := asyncAlterPattern.ReplaceAllString(sql, "${1}")
	if strings.EqualFold(rewritten, sql) {
		return asyncAlter{}, false
	}

	out := asyncAlter{rewritten: rewritten}
	if m := asyncAlterNamePattern.FindStringSubmatch(sql); m != nil {
		out.table = unquoteIdent(m[2])
		if out.table == "" {
			out.table = unquoteIdent(m[1])
		}
	}
	return out, true
}

// jobIDForValidation derives the id of a constraint validation job from the
// table it runs on, the same way the backing database does. It returns "" when
// the table name could not be read.
func jobIDForValidation(table string) string {
	if table == "" {
		return ""
	}
	sum := md5.Sum([]byte("validate:" + table))
	return uuidFromMD5(sum)
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

// jobIDForIndex derives the job id both sides use. Aurora DSQL issues a random
// id; deriving one from the index name keeps the id handed to the client
// findable in sys.jobs without a round trip to read it back, and shapes it as a
// UUID because wait_for_job converts the id to one. It returns "" when the name
// could not be read.
func jobIDForIndex(name string) string {
	if name == "" {
		return ""
	}
	sum := md5.Sum([]byte(name))
	return uuidFromMD5(sum)
}

// uuidFromMD5 shapes a digest as a UUID, because wait_for_job converts ids to
// one.
func uuidFromMD5(sum [md5.Size]byte) string {
	h := hex.EncodeToString(sum[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
