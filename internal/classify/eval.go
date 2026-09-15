package classify

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/Dreamescaper/dsql-emulator/rules"
)

// matches reports whether every predicate the rule sets holds for node.
func matches(r rules.Rule, node *pg_query.Node) bool {
	if nodeType(node) != r.Stmt {
		return false
	}
	if len(r.Relpersistence) > 0 && !contains(r.Relpersistence, relpersistence(node)) {
		return false
	}
	if len(r.ColumnType) > 0 && !intersects(r.ColumnType, columnTypes(node)) {
		return false
	}
	if len(r.Objtype) > 0 && !contains(r.Objtype, objtype(node)) {
		return false
	}
	if len(r.TxnKind) > 0 && !contains(r.TxnKind, txnKind(node)) {
		return false
	}
	return true
}

// nodeType returns the libpg_query oneof field name for a statement node, such
// as "truncate_stmt" or "create_stmt".
func nodeType(node *pg_query.Node) string {
	m := node.ProtoReflect()
	oneofs := m.Descriptor().Oneofs()
	if oneofs.Len() == 0 {
		return ""
	}
	fd := m.WhichOneof(oneofs.Get(0))
	if fd == nil {
		return ""
	}
	return string(fd.Name())
}

func relpersistence(node *pg_query.Node) string {
	if cs := node.GetCreateStmt(); cs != nil && cs.GetRelation() != nil {
		return cs.GetRelation().GetRelpersistence()
	}
	if cta := node.GetCreateTableAsStmt(); cta != nil {
		if into := cta.GetInto(); into != nil && into.GetRel() != nil {
			return into.GetRel().GetRelpersistence()
		}
	}
	return ""
}

func columnTypes(node *pg_query.Node) []string {
	cs := node.GetCreateStmt()
	if cs == nil {
		return nil
	}
	var out []string
	for _, elt := range cs.GetTableElts() {
		col := elt.GetColumnDef()
		if col == nil || col.GetTypeName() == nil {
			continue
		}
		names := col.GetTypeName().GetNames()
		if len(names) == 0 {
			continue
		}
		if s := names[len(names)-1].GetString_(); s != nil {
			out = append(out, s.GetSval())
		}
	}
	return out
}

func objtype(node *pg_query.Node) string {
	if cta := node.GetCreateTableAsStmt(); cta != nil {
		return cta.GetObjtype().String()
	}
	return ""
}

func txnKind(node *pg_query.Node) string {
	if ts := node.GetTransactionStmt(); ts != nil {
		return ts.GetKind().String()
	}
	return ""
}

func contains(want []string, got string) bool {
	for _, w := range want {
		if w == got {
			return true
		}
	}
	return false
}

func intersects(want, got []string) bool {
	for _, g := range got {
		if contains(want, g) {
			return true
		}
	}
	return false
}
