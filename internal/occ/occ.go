// Package occ models Aurora DSQL's optimistic concurrency control for the
// proxy. PostgreSQL waits for a row lock where DSQL adjudicates at commit, so
// the emulator bounds the wait and treats a refused lock as the evidence that
// two transactions wanted the same rows. This package decides which statements
// that evidence can be reported for, and builds the read-only statement that
// reproduces what the refused one would have reported.
package occ

import (
	"fmt"
	"sort"

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
	// Params are the client's parameter positions the shadow asks for, in the
	// order it asks for them. A shadow keeps only the parameters it still
	// refers to, renumbered from $1: PostgreSQL infers a parameter's type from
	// where it is used, so one the shadow dropped -- everything in the SET list
	// of an UPDATE, say -- cannot be left declared and unmentioned.
	Params []int
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
		return shadowIntent("SELECT", node, true)
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
	return shadowIntent(tag, &pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: sel}}, false)
}

// shadowIntent renders a shadow statement and the parameters it needs.
func shadowIntent(tag string, node *pg_query.Node, rowset bool) *Intent {
	params, ok := renumberParams(node)
	if !ok {
		return nil
	}
	shadow, ok := deparse(node)
	if !ok {
		return nil
	}
	return &Intent{Tag: tag, Rows: -1, Shadow: shadow, Rowset: rowset, Params: params}
}

// renumberParams rewrites the parameters a shadow still refers to so that they
// run from $1, and returns the client's positions in that order, so the values
// it bound can be picked out and sent in the order the shadow asks for them.
func renumberParams(node *pg_query.Node) ([]int, bool) {
	var refs []*pg_query.ParamRef
	walk(node.ProtoReflect(), func(n *pg_query.Node) {
		if ref := n.GetParamRef(); ref != nil {
			refs = append(refs, ref)
		}
	})
	if len(refs) == 0 {
		return nil, true
	}

	var order []int32
	seen := make(map[int32]bool, len(refs))
	for _, ref := range refs {
		number := ref.GetNumber()
		if number < 1 {
			// Not a parameter this can reason about.
			return nil, false
		}
		if !seen[number] {
			seen[number] = true
			order = append(order, number)
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	renumbered := make(map[int32]int32, len(order))
	positions := make([]int, len(order))
	for i, number := range order {
		renumbered[number] = int32(i + 1)
		positions[i] = int(number)
	}
	for _, ref := range refs {
		ref.Number = renumbered[ref.GetNumber()]
	}
	return positions, true
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
