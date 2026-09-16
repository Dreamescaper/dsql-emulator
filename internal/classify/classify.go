// Package classify decides whether a SQL string is acceptable to Aurora DSQL,
// using a real PostgreSQL parser rather than pattern matching. It also reports
// the kind of each statement, which the transaction state machine needs.
package classify

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/Dreamescaper/dsql-emulator/rules"
)

// Kind is the category of a statement, used to enforce transaction rules.
type Kind uint8

const (
	KindOther Kind = iota
	KindSelect
	KindDML
	KindDDL
	KindBegin
	KindCommit
	KindRollback
)

func (k Kind) String() string {
	switch k {
	case KindSelect:
		return "select"
	case KindDML:
		return "dml"
	case KindDDL:
		return "ddl"
	case KindBegin:
		return "begin"
	case KindCommit:
		return "commit"
	case KindRollback:
		return "rollback"
	default:
		return "other"
	}
}

// Verdict is the outcome of classifying one SQL string. A zero Verdict means
// the statement may be forwarded.
type Verdict struct {
	RuleID  string
	Code    string
	Message string
}

// Rejected reports whether the statement must be refused.
func (v Verdict) Rejected() bool { return v.Code != "" }

// Result pairs a classification verdict with the kinds of the statements it
// read. Kinds and Tables are populated only when nothing was rejected.
type Result struct {
	Verdict Verdict
	Kinds   []Kind
	// Tables are the relations a DML statement touches, used by OCC injection.
	Tables []string
}

// Classifier evaluates SQL against a ruleset.
type Classifier struct {
	ruleset *rules.Ruleset
}

// New returns a Classifier backed by rs.
func New(rs *rules.Ruleset) *Classifier {
	return &Classifier{ruleset: rs}
}

// Ruleset returns the ruleset the classifier was built with.
func (c *Classifier) Ruleset() *rules.Ruleset { return c.ruleset }

// options are the per-call settings an Option adjusts.
type options struct {
	async bool
}

// Option steers one classification.
type Option func(*options)

// AsyncRewritten marks a statement whose ASYNC keyword was stripped before it
// was parsed, because PostgreSQL's grammar does not accept it. The rules that
// exist only to require the ASYNC form are skipped, since the client supplied
// it; every other rule still applies.
func AsyncRewritten() Option {
	return func(o *options) { o.async = true }
}

// Classify parses sql and returns the first rule it violates. A parse error is
// returned as an error rather than a rejection: unparseable input is not
// evidence of a DSQL incompatibility and is left for the backing server to
// answer.
func (c *Classifier) Classify(sql string, opts ...Option) (Result, error) {
	var o options
	for _, apply := range opts {
		apply(&o)
	}

	res, err := pg_query.Parse(sql)
	if err != nil {
		return Result{}, err
	}

	var result Result
	for _, raw := range res.GetStmts() {
		node := raw.GetStmt()
		if node == nil {
			continue
		}

		// Isolation is checked first: DSQL reports a specific level for an
		// unsupported isolation request even though it refuses the SET itself.
		if level, unsupported := unsupportedIsolation(node, c.ruleset.Isolation.Supported); unsupported {
			return Result{Verdict: Verdict{
				RuleID:  "isolation",
				Code:    "0A000",
				Message: "Unsupported isolation level: " + level,
			}}, nil
		}

		for _, rule := range c.ruleset.Unsupported {
			if rule.UnlessAsync && o.async {
				continue
			}
			if matches(rule, node) {
				return Result{Verdict: Verdict{RuleID: rule.ID, Code: rule.Code, Message: rule.Message}}, nil
			}
		}

		result.Kinds = append(result.Kinds, kindOf(node))
		result.Tables = append(result.Tables, statementTables(node)...)
	}
	return result, nil
}
