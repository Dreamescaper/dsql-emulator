// Package occ models Aurora DSQL's optimistic concurrency control for the
// proxy. PostgreSQL waits for a row lock where DSQL adjudicates at commit, so
// the emulator bounds the wait and treats a refused lock as the evidence that
// two transactions wanted the same rows. This package decides which statements
// that evidence can be reported for, and builds the read-only statement that
// reproduces what the refused one would have reported.
package occ

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	// CodeLockTimeout is what lock_timeout turns a lock wait into. The other
	// transaction is still open and holds the rows.
	CodeLockTimeout = "55P03"
	// CodeSerialization is what REPEATABLE READ raises when the other
	// transaction committed before the wait began.
	CodeSerialization = "40001"
)

// IsConflict reports whether a SQLSTATE is evidence of a conflict Aurora DSQL
// would have resolved at commit rather than at the statement.
func IsConflict(code string) bool {
	return code == CodeLockTimeout || code == CodeSerialization
}

// Intent is what a statement would have reported had the backend not refused
// it a lock. Aurora DSQL lets the loser's statements succeed and fails it at
// COMMIT, so the emulator has to answer the statement as if it had run.
type Intent struct {
	// Tag is the command tag Aurora DSQL reports, without its row count.
	Tag string
	// Rows is the row count when the statement itself carries it. It is -1
	// when Shadow has to be run to find it.
	Rows int
	// Shadow is a read-only statement that reports what the refused one would
	// have. It takes no locks, so it cannot be refused in turn.
	Shadow string
	// Rowset reports whether the client is waiting for Shadow's rows, rather
	// than only for the count of them.
	Rowset bool
}

// CommandTag renders the tag for a row count.
func (i Intent) CommandTag(rows int) string {
	if i.Tag == "INSERT" {
		return fmt.Sprintf("INSERT 0 %d", rows)
	}
	return fmt.Sprintf("%s %d", i.Tag, rows)
}

// Analyze reports what a statement would have answered if the backend refused
// it a lock. It returns nil for statements that cannot conflict, and for the
// ones whose answer the emulator cannot reproduce exactly; the caller then
// reports the conflict where PostgreSQL raised it instead of at COMMIT.
func Analyze(sql string) *Intent {
	tree, err := pg_query.Parse(sql)
	if err != nil || len(tree.GetStmts()) != 1 {
		return nil
	}
	node := tree.GetStmts()[0].GetStmt()
	if node == nil {
		return nil
	}
	// A shadow is a different statement, so the client's bound values cannot be
	// carried over to it: the parameter numbering it would need belongs to the
	// statement that was refused.
	if hasParameters(node) {
		return nil
	}

	switch {
	case node.GetUpdateStmt() != nil:
		u := node.GetUpdateStmt()
		if len(u.GetReturningList()) > 0 {
			return nil
		}
		return countIntent("UPDATE", u.GetRelation(), u.GetFromClause(), u.GetWhereClause())

	case node.GetDeleteStmt() != nil:
		d := node.GetDeleteStmt()
		if len(d.GetReturningList()) > 0 {
			return nil
		}
		return countIntent("DELETE", d.GetRelation(), d.GetUsingClause(), d.GetWhereClause())

	case node.GetInsertStmt() != nil:
		ins := node.GetInsertStmt()
		if len(ins.GetReturningList()) > 0 || ins.GetOnConflictClause() != nil {
			return nil
		}
		// Only a VALUES list states its own row count. INSERT ... SELECT would
		// need the source counted under the transaction's snapshot, which the
		// rolled-back statement no longer describes.
		values := ins.GetSelectStmt().GetSelectStmt()
		if values == nil || len(values.GetValuesLists()) == 0 {
			return nil
		}
		return &Intent{Tag: "INSERT", Rows: len(values.GetValuesLists())}

	case node.GetSelectStmt() != nil:
		sel := node.GetSelectStmt()
		if len(sel.GetLockingClause()) == 0 {
			return nil
		}
		// The rows are the same either way; only the lock is not taken.
		sel.LockingClause = nil
		shadow, ok := deparse(node)
		if !ok {
			return nil
		}
		return &Intent{Tag: "SELECT", Rows: -1, Shadow: shadow, Rowset: true}
	}

	return nil
}

// countIntent builds the intent for a DML statement whose row count has to be
// counted, because the statement was rolled back before it reported one.
func countIntent(tag string, rel *pg_query.RangeVar, extra []*pg_query.Node, where *pg_query.Node) *Intent {
	if rel == nil {
		return nil
	}
	from := append([]*pg_query.Node{{Node: &pg_query.Node_RangeVar{RangeVar: rel}}}, extra...)
	sel := &pg_query.SelectStmt{
		TargetList:  []*pg_query.Node{resTarget(countStar())},
		FromClause:  from,
		WhereClause: where,
		LimitOption: pg_query.LimitOption_LIMIT_OPTION_DEFAULT,
		Op:          pg_query.SetOperation_SETOP_NONE,
	}
	shadow, ok := deparse(&pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: sel}})
	if !ok {
		return nil
	}
	return &Intent{Tag: tag, Rows: -1, Shadow: shadow}
}

func countStar() *pg_query.Node {
	return &pg_query.Node{Node: &pg_query.Node_FuncCall{FuncCall: &pg_query.FuncCall{
		Funcname: []*pg_query.Node{{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: "count"}}}},
		AggStar:  true,
	}}}
}

func resTarget(val *pg_query.Node) *pg_query.Node {
	return &pg_query.Node{Node: &pg_query.Node_ResTarget{ResTarget: &pg_query.ResTarget{Val: val}}}
}

func deparse(node *pg_query.Node) (string, bool) {
	out, err := pg_query.Deparse(&pg_query.ParseResult{
		Stmts: []*pg_query.RawStmt{{Stmt: node}},
	})
	if err != nil {
		return "", false
	}
	return out, true
}

// hasParameters reports whether a statement carries bind parameters.
func hasParameters(node *pg_query.Node) bool {
	found := false
	walk(node.ProtoReflect(), func(n *pg_query.Node) {
		if n.GetParamRef() != nil {
			found = true
		}
	})
	return found
}

// walk visits every Node in a parsed statement. libpg_query exposes no walker,
// so the protobuf tree is walked generically rather than case by case.
func walk(msg protoreflect.Message, visit func(*pg_query.Node)) {
	if node, ok := msg.Interface().(*pg_query.Node); ok {
		visit(node)
	}

	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Message() == nil {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				walk(list.Get(i).Message(), visit)
			}
			return true
		}
		walk(value.Message(), visit)
		return true
	})
}
