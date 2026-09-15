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

// rewriteAsyncIndex removes the ASYNC keyword from a CREATE INDEX statement and
// reports the index name. Both the emulator and the backing database derive the
// job id from that name, so the id returned to the client matches the row the
// database records.
func rewriteAsyncIndex(sql string) (rewritten, indexName string, ok bool) {
	if !asyncIndexPattern.MatchString(sql) {
		return sql, "", false
	}
	rewritten = asyncIndexPattern.ReplaceAllString(sql, "${1}")
	if strings.EqualFold(rewritten, sql) {
		return sql, "", false
	}
	return rewritten, indexNameOf(sql), true
}

// indexNameOf returns the unqualified index name, unquoted and folded the way
// PostgreSQL stores it, or "" when it cannot be read.
func indexNameOf(sql string) string {
	m := asyncIndexNamePattern.FindStringSubmatch(sql)
	if m == nil {
		return ""
	}
	name := m[2]
	if name == "" {
		name = m[1]
	}
	return unquoteIdent(name)
}

// unquoteIdent folds an identifier the way PostgreSQL would: a quoted name keeps
// its case and escapes, an unquoted one is lowercased.
func unquoteIdent(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return strings.ToLower(s)
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

// jobIDForIndex derives the job id both sides use. Aurora DSQL issues random
// ids; deriving it from the index name keeps the id handed to the client
// findable in sys.jobs without a round trip to read it back. It returns "" when
// the name could not be read.
func jobIDForIndex(name string) string {
	if name == "" {
		return ""
	}
	sum := md5.Sum([]byte(name))
	return hex.EncodeToString(sum[:])
}
