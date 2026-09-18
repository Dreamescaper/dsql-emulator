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
	if r.ColumnArray && !hasArrayColumn(node) {
		return false
	}
	if len(r.RemoveType) > 0 && !contains(r.RemoveType, removeType(node)) {
		return false
	}
	if len(r.RenameType) > 0 && !contains(r.RenameType, renameType(node)) {
		return false
	}
	if len(r.Locking) > 0 && !intersects(r.Locking, lockingStrengths(node)) {
		return false
	}
	if len(r.Function) > 0 && !intersects(r.Function, functionNames(node)) {
		return false
	}
	if len(r.Contains) > 0 && !containsNode(node, r.Contains) {
		return false
	}
	if len(r.VacuumKind) > 0 && !contains(r.VacuumKind, vacuumKind(node)) {
		return false
	}
	if len(r.ShowName) > 0 && !contains(r.ShowName, showName(node)) {
		return false
	}
	if len(r.AlterAction) > 0 && !intersects(r.AlterAction, alterActions(node)) {
		return false
	}
	if r.AddConstraintMissingNotValid && !addsConstraintWithoutValidation(node) {
		return false
	}
	if len(r.AddConstraintType) > 0 && !addsConstraintOfType(node, r.AddConstraintType) {
		return false
	}
	if len(r.AddColumnConstraint) > 0 && !addsColumnWithConstraint(node, r.AddColumnConstraint) {
		return false
	}
	if len(r.Objtype) > 0 && !contains(r.Objtype, objtype(node)) {
		return false
	}
	if len(r.TxnKind) > 0 && !contains(r.TxnKind, txnKind(node)) {
		return false
	}
	if len(r.SetName) > 0 && !contains(r.SetName, setParameter(node)) {
		return false
	}
	if len(r.LanguageNot) > 0 && contains(r.LanguageNot, functionLanguage(node)) {
		return false
	}
	if r.SequenceCacheMin != nil {
		cache, ok := sequenceCache(node)
		if !cacheTooSmall(cache, ok, *r.SequenceCacheMin, r.CacheAllow) {
			return false
		}
	}
	if len(r.IdentityTypeNot) > 0 {
		if _, found := identityColumnType(node, r.IdentityTypeNot); !found {
			return false
		}
	}
	if r.IdentityCacheMin != nil && !hasSmallIdentityCache(node, *r.IdentityCacheMin, r.CacheAllow) {
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

// columnDefs returns the columns a statement declares: the ones a CREATE TABLE
// defines, and the ones an ALTER TABLE adds or retypes. A type is unsupported
// wherever it appears, so both forms are inspected.
func columnDefs(node *pg_query.Node) []*pg_query.ColumnDef {
	if cs := node.GetCreateStmt(); cs != nil {
		var out []*pg_query.ColumnDef
		for _, elt := range cs.GetTableElts() {
			if col := elt.GetColumnDef(); col != nil {
				out = append(out, col)
			}
		}
		return out
	}

	stmt := node.GetAlterTableStmt()
	if stmt == nil {
		return nil
	}
	var out []*pg_query.ColumnDef
	for _, cmd := range stmt.GetCmds() {
		at := cmd.GetAlterTableCmd()
		if at == nil {
			continue
		}
		switch at.GetSubtype() {
		case pg_query.AlterTableType_AT_AddColumn, pg_query.AlterTableType_AT_AlterColumnType:
			if col := at.GetDef().GetColumnDef(); col != nil {
				out = append(out, col)
			}
		}
	}
	return out
}

func columnTypes(node *pg_query.Node) []string {
	var out []string
	for _, col := range columnDefs(node) {
		if col.GetTypeName() == nil {
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

// lockingStrengths returns the row-locking strengths of a SELECT.
func lockingStrengths(node *pg_query.Node) []string {
	sel := node.GetSelectStmt()
	if sel == nil {
		return nil
	}
	var out []string
	for _, clause := range sel.GetLockingClause() {
		if lc := clause.GetLockingClause(); lc != nil {
			out = append(out, lc.GetStrength().String())
		}
	}
	return out
}

// alterActions returns the ALTER TABLE command subtypes of a statement, such as
// "AT_ValidateConstraint".
func alterActions(node *pg_query.Node) []string {
	stmt := node.GetAlterTableStmt()
	if stmt == nil {
		return nil
	}
	var out []string
	for _, node := range stmt.GetCmds() {
		if cmd := node.GetAlterTableCmd(); cmd != nil {
			out = append(out, cmd.GetSubtype().String())
		}
	}
	return out
}

// addsConstraintWithoutValidation reports whether a statement adds a CHECK or
// FOREIGN KEY constraint without NOT VALID, which the dialect requires for a
// constraint added by ALTER TABLE.
// addsColumnWithConstraint reports whether an ALTER TABLE ADD COLUMN gives the
// new column a constraint of one of the given kinds. Aurora DSQL refuses a
// column added with one, saying so about the constraint rather than about the
// column's type: the recorded alter_add_identity_integer probe answers
// `ALTER TABLE ADD COLUMN with constraint not supported`.
func addsColumnWithConstraint(node *pg_query.Node, kinds []string) bool {
	stmt := node.GetAlterTableStmt()
	if stmt == nil {
		return false
	}
	for _, node := range stmt.GetCmds() {
		cmd := node.GetAlterTableCmd()
		if cmd == nil || cmd.GetSubtype() != pg_query.AlterTableType_AT_AddColumn {
			continue
		}
		for _, con := range cmd.GetDef().GetColumnDef().GetConstraints() {
			c := con.GetConstraint()
			if c != nil && contains(kinds, c.GetContype().String()) {
				return true
			}
		}
	}
	return false
}

// addsConstraintOfType reports whether an ALTER TABLE adds a constraint of one
// of the given kinds.
//
// A constraint that adopts an index already built -- ADD CONSTRAINT ... UNIQUE
// USING INDEX -- is never matched. Aurora DSQL accepts that form: the recorded
// alter_unique_using_index probe gets as far as `55000 index is not valid`,
// which is a complaint about the index rather than about the statement.
func addsConstraintOfType(node *pg_query.Node, kinds []string) bool {
	stmt := node.GetAlterTableStmt()
	if stmt == nil {
		return false
	}
	for _, node := range stmt.GetCmds() {
		cmd := node.GetAlterTableCmd()
		if cmd == nil || cmd.GetSubtype() != pg_query.AlterTableType_AT_AddConstraint {
			continue
		}
		constraint := cmd.GetDef().GetConstraint()
		if constraint == nil || constraint.GetIndexname() != "" {
			continue
		}
		if contains(kinds, constraint.GetContype().String()) {
			return true
		}
	}
	return false
}

func addsConstraintWithoutValidation(node *pg_query.Node) bool {
	stmt := node.GetAlterTableStmt()
	if stmt == nil {
		return false
	}
	for _, node := range stmt.GetCmds() {
		cmd := node.GetAlterTableCmd()
		if cmd == nil || cmd.GetSubtype() != pg_query.AlterTableType_AT_AddConstraint {
			continue
		}
		constraint := cmd.GetDef().GetConstraint()
		if constraint == nil || constraint.GetSkipValidation() {
			continue
		}
		switch constraint.GetContype() {
		case pg_query.ConstrType_CONSTR_CHECK, pg_query.ConstrType_CONSTR_FOREIGN:
			return true
		}
	}
	return false
}

func showName(node *pg_query.Node) string {
	if stmt := node.GetVariableShowStmt(); stmt != nil {
		return stmt.GetName()
	}
	return ""
}

func removeType(node *pg_query.Node) string {
	if d := node.GetDropStmt(); d != nil {
		return d.GetRemoveType().String()
	}
	return ""
}

func renameType(node *pg_query.Node) string {
	if r := node.GetRenameStmt(); r != nil {
		return r.GetRenameType().String()
	}
	return ""
}

// hasArrayColumn reports whether any column a statement declares is an array.
func hasArrayColumn(node *pg_query.Node) bool {
	for _, col := range columnDefs(node) {
		if col.GetTypeName() == nil {
			continue
		}
		if len(col.GetTypeName().GetArrayBounds()) > 0 {
			return true
		}
	}
	return false
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

// setParameter returns the parameter named by a SET statement, such as
// "TRANSACTION" or "default_transaction_isolation".
func setParameter(node *pg_query.Node) string {
	if vs := node.GetVariableSetStmt(); vs != nil {
		return vs.GetName()
	}
	return ""
}

// functionLanguage returns the LANGUAGE option of a CREATE FUNCTION, or "" when
// none is given.
func functionLanguage(node *pg_query.Node) string {
	fn := node.GetCreateFunctionStmt()
	if fn == nil {
		return ""
	}
	for _, opt := range fn.GetOptions() {
		de := opt.GetDefElem()
		if de == nil || de.GetDefname() != "language" {
			continue
		}
		if s := de.GetArg().GetString_(); s != nil {
			return s.GetSval()
		}
	}
	return ""
}

// sequenceCache returns the CACHE option of a CREATE SEQUENCE.
func sequenceCache(node *pg_query.Node) (int, bool) {
	seq := node.GetCreateSeqStmt()
	if seq == nil {
		return 0, false
	}
	return defElemInt(seq.GetOptions(), "cache")
}

// identityColumnType returns the type of the first identity column whose type
// is not one the dialect allows, as the parser spells it, and reports whether
// there was one. Aurora DSQL allows only bigint, and names the offending type
// in its refusal.
func identityColumnType(node *pg_query.Node, allowed []string) (string, bool) {
	for _, col := range columnDefs(node) {
		if !isIdentityColumn(col) {
			continue
		}
		written := lastTypeName(col.GetTypeName())
		if written == "" || contains(allowed, written) {
			continue
		}
		return written, true
	}
	return "", false
}

func isIdentityColumn(col *pg_query.ColumnDef) bool {
	for _, con := range col.GetConstraints() {
		if c := con.GetConstraint(); c != nil && c.GetContype() == pg_query.ConstrType_CONSTR_IDENTITY {
			return true
		}
	}
	return false
}

// lastTypeName is the type's own name, without the schema the parser qualifies
// a built-in with.
func lastTypeName(name *pg_query.TypeName) string {
	names := name.GetNames()
	if len(names) == 0 {
		return ""
	}
	if s := names[len(names)-1].GetString_(); s != nil {
		return s.GetSval()
	}
	return ""
}

// hasSmallIdentityCache reports whether any identity column in a CREATE TABLE
// sets a CACHE that is missing or too small.
func hasSmallIdentityCache(node *pg_query.Node, min int, allow []int) bool {
	cs := node.GetCreateStmt()
	if cs == nil {
		return false
	}
	for _, elt := range cs.GetTableElts() {
		col := elt.GetColumnDef()
		if col == nil {
			continue
		}
		for _, con := range col.GetConstraints() {
			c := con.GetConstraint()
			if c == nil || c.GetContype() != pg_query.ConstrType_CONSTR_IDENTITY {
				continue
			}
			cache, ok := defElemInt(c.GetOptions(), "cache")
			if cacheTooSmall(cache, ok, min, allow) {
				return true
			}
		}
	}
	return false
}

// cacheTooSmall reports whether a CACHE value is missing or below min without
// appearing in allow.
func cacheTooSmall(cache int, ok bool, min int, allow []int) bool {
	if !ok {
		return true
	}
	if cache >= min {
		return false
	}
	return !containsInt(allow, cache)
}

func defElemInt(nodes []*pg_query.Node, name string) (int, bool) {
	for _, node := range nodes {
		de := node.GetDefElem()
		if de == nil || de.GetDefname() != name {
			continue
		}
		if i := de.GetArg().GetInteger(); i != nil {
			return int(i.GetIval()), true
		}
	}
	return 0, false
}

func contains(want []string, got string) bool {
	for _, w := range want {
		if w == got {
			return true
		}
	}
	return false
}

func containsInt(want []int, got int) bool {
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
