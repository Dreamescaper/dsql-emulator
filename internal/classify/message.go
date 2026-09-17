package classify

import (
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/Dreamescaper/dsql-emulator/rules"
)

// Placeholders a rule's message may carry. Aurora DSQL names the offending
// thing in some of its refusals, so the rule states the sentence and the
// statement supplies the noun.
const (
	typePlaceholder     = "{type}"
	languagePlaceholder = "{language}"
)

// fill renders a rule's message for the statement it refused.
func fill(r rules.Rule, node *pg_query.Node) string {
	message := r.Message
	if !strings.Contains(message, "{") {
		return message
	}
	if strings.Contains(message, typePlaceholder) {
		message = strings.ReplaceAll(message, typePlaceholder, refusedType(r, node))
	}
	if strings.Contains(message, languagePlaceholder) {
		message = strings.ReplaceAll(message, languagePlaceholder, functionLanguage(node))
	}
	return message
}

// refusedType names the type the rule refused the statement for, as PostgreSQL
// spells it rather than as the statement wrote it: Aurora DSQL reports
// `bit varying` for a `varbit` column and `integer[]` for an `int[]` one.
func refusedType(r rules.Rule, node *pg_query.Node) string {
	if r.ColumnArray {
		for _, col := range columnDefs(node) {
			name := col.GetTypeName()
			if name == nil || len(name.GetArrayBounds()) == 0 {
				continue
			}
			return canonicalType(lastTypeName(name)) + "[]"
		}
	}
	if len(r.IdentityTypeNot) > 0 {
		if written, found := identityColumnType(node, r.IdentityTypeNot); found {
			return canonicalType(written)
		}
	}
	if len(r.ColumnType) > 0 {
		for _, col := range columnDefs(node) {
			name := col.GetTypeName()
			if name == nil {
				continue
			}
			if written := lastTypeName(name); contains(r.ColumnType, written) {
				return canonicalType(written)
			}
		}
	}
	// A geometric type is refused through the function that builds one, which
	// is named for the type itself.
	for _, called := range functionNames(node) {
		if contains(r.Function, called) {
			return canonicalType(called)
		}
	}
	return ""
}

// canonical maps the names the parser uses internally to the ones PostgreSQL
// displays, which is what Aurora DSQL's refusals carry. Only the aliases the
// parser always rewrites are listed; a type written as its displayed name comes
// through the parser unchanged and needs no entry.
var canonical = map[string]string{
	"int2":    "smallint",
	"int4":    "integer",
	"int8":    "bigint",
	"float4":  "real",
	"float8":  "double precision",
	"bool":    "boolean",
	"varbit":  "bit varying",
	"bpchar":  "character",
	"varchar": "character varying",
}

func canonicalType(written string) string {
	if name, ok := canonical[written]; ok {
		return name
	}
	return written
}
