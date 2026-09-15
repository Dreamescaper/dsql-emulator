package classify

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// walkNodes visits every Node in a parsed statement. libpg_query exposes no
// walker, so the protobuf tree is walked generically; this avoids missing a
// construct nested inside an expression or subquery.
func walkNodes(msg protoreflect.Message, visit func(*pg_query.Node)) {
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
				walkNodes(list.Get(i).Message(), visit)
			}
			return true
		}
		walkNodes(value.Message(), visit)
		return true
	})
}

// functionNames returns the name of every function called in a statement.
func functionNames(node *pg_query.Node) []string {
	var names []string
	walkNodes(node.ProtoReflect(), func(n *pg_query.Node) {
		if call := n.GetFuncCall(); call != nil {
			if name := functionName(call); name != "" {
				names = append(names, name)
			}
		}
	})
	return names
}

// containsNode reports whether any node in the statement has one of the given
// oneof names, such as "table_sample_clause".
func containsNode(node *pg_query.Node, wanted []string) bool {
	found := false
	walkMessages(node.ProtoReflect(), func(msg protoreflect.Message) {
		if found {
			return
		}
		// A construct may appear as a oneof on Node, or as a plain message
		// field such as RangeVar.tableSample.
		if n, ok := msg.Interface().(*pg_query.Node); ok && contains(wanted, nodeType(n)) {
			found = true
			return
		}
		if contains(wanted, string(msg.Descriptor().Name())) {
			found = true
		}
	})
	return found
}

// walkMessages visits every message in the tree, including those reached
// through plain message fields.
func walkMessages(msg protoreflect.Message, visit func(protoreflect.Message)) {
	visit(msg)

	msg.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Message() == nil {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := 0; i < list.Len(); i++ {
				walkMessages(list.Get(i).Message(), visit)
			}
			return true
		}
		walkMessages(value.Message(), visit)
		return true
	})
}

// vacuumKind distinguishes VACUUM from ANALYZE, which share a node.
func vacuumKind(node *pg_query.Node) string {
	stmt := node.GetVacuumStmt()
	if stmt == nil {
		return ""
	}
	if stmt.GetIsVacuumcmd() {
		return "VACUUM"
	}
	return "ANALYZE"
}

func functionName(call *pg_query.FuncCall) string {
	parts := call.GetFuncname()
	if len(parts) == 0 {
		return ""
	}
	if s := parts[len(parts)-1].GetString_(); s != nil {
		return s.GetSval()
	}
	return ""
}
