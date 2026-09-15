package classify

import (
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// unsupportedIsolation reports the requested isolation level when a statement
// asks for one that is not supported. The returned level is upper-cased for the
// error message.
func unsupportedIsolation(node *pg_query.Node, supported []string) (string, bool) {
	level := isolationLevel(node)
	if level == "" {
		return "", false
	}
	for _, s := range supported {
		if strings.EqualFold(s, level) {
			return "", false
		}
	}
	return strings.ToUpper(level), true
}

// isolationLevel extracts a requested isolation level from the statements that
// can carry one:
//
//	BEGIN / START TRANSACTION ISOLATION LEVEL ...
//	SET TRANSACTION ISOLATION LEVEL ...
//	SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL ...
//	SET [default_]transaction_isolation = ...
func isolationLevel(node *pg_query.Node) string {
	if ts := node.GetTransactionStmt(); ts != nil {
		return defElemValue(ts.GetOptions(), "transaction_isolation")
	}

	vs := node.GetVariableSetStmt()
	if vs == nil {
		return ""
	}
	switch vs.GetName() {
	case "TRANSACTION", "SESSION CHARACTERISTICS":
		return defElemValue(vs.GetArgs(), "transaction_isolation")
	case "transaction_isolation", "default_transaction_isolation":
		args := vs.GetArgs()
		if len(args) == 0 {
			return ""
		}
		if c := args[0].GetAConst(); c != nil && c.GetSval() != nil {
			return c.GetSval().GetSval()
		}
	}
	return ""
}

// defElemValue returns the string argument of the named DefElem, if present.
func defElemValue(nodes []*pg_query.Node, name string) string {
	for _, node := range nodes {
		de := node.GetDefElem()
		if de == nil || de.GetDefname() != name {
			continue
		}
		if c := de.GetArg().GetAConst(); c != nil && c.GetSval() != nil {
			return c.GetSval().GetSval()
		}
	}
	return ""
}
