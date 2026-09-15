package proxy

import (
	"regexp"
	"strings"
)

// asyncIndexPattern matches the ASYNC keyword that Aurora DSQL requires on
// CREATE INDEX but PostgreSQL's parser does not understand. The rewrite is
// textual because libpg_query rejects the statement outright; everything except
// the keyword is preserved.
var asyncIndexPattern = regexp.MustCompile(`(?is)^(\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+)ASYNC\s+`)

// rewriteAsyncIndex removes the ASYNC keyword from a CREATE INDEX statement. It
// reports false when the statement is not an async index.
func rewriteAsyncIndex(sql string) (string, bool) {
	if !asyncIndexPattern.MatchString(sql) {
		return sql, false
	}
	rewritten := asyncIndexPattern.ReplaceAllString(sql, "${1}")
	return rewritten, !strings.EqualFold(rewritten, sql)
}
