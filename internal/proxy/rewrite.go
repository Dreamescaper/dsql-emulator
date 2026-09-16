package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

var (
	// asyncIndexPattern matches the ASYNC keyword that Aurora DSQL requires on
	// CREATE INDEX but PostgreSQL's parser does not understand, and
	// asyncAlterPattern the one on the ALTER TABLE form the dialect uses to
	// validate a constraint in the background.
	//
	// Stripping the keyword is textual because libpg_query rejects the
	// statement outright; everything after that is decided on a real parse tree
	// of what is left, so these are the only patterns the dialect needs.
	asyncIndexPattern = regexp.MustCompile(`(?is)^(\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+)ASYNC\s+`)
	asyncAlterPattern = regexp.MustCompile(`(?is)^(\s*ALTER\s+TABLE\s+)ASYNC\s+`)
)

// stripAsync removes the ASYNC keyword matched by pattern, reporting whether
// the statement carried it.
func stripAsync(pattern *regexp.Regexp, sql string) (string, bool) {
	if !pattern.MatchString(sql) {
		return "", false
	}
	rewritten := pattern.ReplaceAllString(sql, "${1}")
	if strings.EqualFold(rewritten, sql) {
		return "", false
	}
	return rewritten, true
}

// parseAsyncIndex recognises CREATE INDEX ASYNC and strips the keyword, leaving
// a statement PostgreSQL can parse and the ruleset can be applied to.
//
// A schema-qualified index name is left in place on purpose. Aurora DSQL's
// grammar does not accept one — the index always lands in the table's schema —
// and neither does PostgreSQL's, so forwarding the stripped statement produces
// the same `42601 syntax error at or near "."` the dialect reports.
func parseAsyncIndex(sql string) (string, bool) {
	return stripAsync(asyncIndexPattern, sql)
}

// parseAsyncAlterTable recognises ALTER TABLE ASYNC and strips the keyword.
func parseAsyncAlterTable(sql string) (string, bool) {
	return stripAsync(asyncAlterPattern, sql)
}

// jobMarker returns the comment that carries a job id down to the backing
// database, which records it in sys.jobs under that id. Passing the id costs no
// extra round trip and works for a statement that names no object, such as an
// unnamed index.
func jobMarker(jobID string) string {
	return "/* dsql_job=" + jobID + " */ "
}

// newJobID returns the identifier for one asynchronous statement. It is a UUID
// because sys.wait_for_job converts an id to one, and reports anything else as
// malformed rather than unknown.
func newJobID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	return uuidAsText(buf)
}

// uuidAsText shapes sixteen random bytes as a UUID.
func uuidAsText(buf [16]byte) string {
	h := hex.EncodeToString(buf[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
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
