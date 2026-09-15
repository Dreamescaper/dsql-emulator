package classify

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// ddlNodes are the statement nodes that count as DDL. Aurora DSQL rejects most
// DDL outright, so this covers the statements that can reach the backend.
var ddlNodes = map[string]bool{
	"create_stmt":              true,
	"alter_table_stmt":         true,
	"drop_stmt":                true,
	"index_stmt":               true,
	"view_stmt":                true,
	"create_seq_stmt":          true,
	"alter_seq_stmt":           true,
	"create_schema_stmt":       true,
	"grant_stmt":               true,
	"revoke_stmt":              true,
	"comment_stmt":             true,
	"create_table_as_stmt":     true,
	"rename_stmt":              true,
	"alter_owner_stmt":         true,
	"alter_object_schema_stmt": true,
}

// statementTables returns the relations a DML statement reads or writes.
func statementTables(node *pg_query.Node) []string {
	switch {
	case node.GetInsertStmt() != nil:
		return relationName(node.GetInsertStmt().GetRelation())
	case node.GetUpdateStmt() != nil:
		return relationName(node.GetUpdateStmt().GetRelation())
	case node.GetDeleteStmt() != nil:
		return relationName(node.GetDeleteStmt().GetRelation())
	case node.GetMergeStmt() != nil:
		return relationName(node.GetMergeStmt().GetRelation())
	}
	return nil
}

func relationName(rel *pg_query.RangeVar) []string {
	if rel == nil || rel.GetRelname() == "" {
		return nil
	}
	return []string{rel.GetRelname()}
}

// kindOf classifies a statement node for transaction rule purposes.
func kindOf(node *pg_query.Node) Kind {
	switch nodeType(node) {
	case "insert_stmt", "update_stmt", "delete_stmt", "merge_stmt":
		return KindDML
	case "select_stmt":
		return KindSelect
	case "transaction_stmt":
		switch txnKind(node) {
		case "TRANS_STMT_BEGIN", "TRANS_STMT_START":
			return KindBegin
		case "TRANS_STMT_COMMIT":
			return KindCommit
		case "TRANS_STMT_ROLLBACK":
			return KindRollback
		}
		return KindOther
	}
	if ddlNodes[nodeType(node)] {
		return KindDDL
	}
	return KindOther
}
