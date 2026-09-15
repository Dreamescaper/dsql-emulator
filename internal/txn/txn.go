// Package txn tracks Aurora DSQL's transaction rules per session: which
// statements may share a transaction, how long a transaction may live, and how
// many rows it may modify.
package txn

import (
	"fmt"
	"time"

	"github.com/Dreamescaper/dsql-emulator/internal/classify"
)

// SQLSTATE codes used by the transaction rules.
const (
	CodeFeatureUnsupported = "0A000"
	CodeProgramLimit       = "54000"
)

// Limits are the per-transaction caps, sourced from the ruleset. The DML row
// cap is enforced by the backing database rather than here, so it is absent.
type Limits struct {
	MaxAge time.Duration
}

// Violation describes a transaction rule that a batch of statements breaks.
type Violation struct {
	Code    string
	Message string
	Rule    string
}

// Stats is a snapshot of tracker state, for tests and logging.
type Stats struct {
	InTxn    bool
	DDLCount int
	DMLSeen  bool
}

// Tracker accumulates transaction state for one session. A batch of kinds is
// what the client submits in a single exchange: one simple Query (which may
// hold several statements) or one prepared statement.
type Tracker struct {
	limits   Limits
	inTxn    bool
	beganAt  time.Time
	ddlCount int
	dmlSeen  bool
}

// New returns a Tracker enforcing limits.
func New(limits Limits) *Tracker {
	return &Tracker{limits: limits}
}

// Admit decides whether a batch may run given the ruleset, then records it.
// Any statement in the batch is accepted or the whole batch is refused; state
// is never changed for a refused batch.
//
// A ROLLBACK is always admitted, so a client can escape a transaction that has
// already breached a limit.
func (t *Tracker) Admit(kinds []classify.Kind, now time.Time) (Violation, bool) {
	if !t.inTxn {
		t.reset()
	}

	rollback := false
	for _, k := range kinds {
		if k == classify.KindRollback {
			rollback = true
		}
	}

	if !rollback && t.inTxn {
		if t.limits.MaxAge > 0 && !t.beganAt.IsZero() && now.Sub(t.beganAt) > t.limits.MaxAge {
			return Violation{
				Code:    CodeProgramLimit,
				Rule:    "txn_age",
				Message: fmt.Sprintf("transaction age of %s exceeded", t.limits.MaxAge),
			}, true
		}
	}

	ddl, dml := t.ddlCount, t.dmlSeen
	for _, k := range kinds {
		switch k {
		case classify.KindBegin:
			ddl, dml = 0, false
		case classify.KindCommit, classify.KindRollback:
			ddl, dml = 0, false
		case classify.KindDDL:
			if ddl >= 1 {
				return Violation{
					Code:    CodeFeatureUnsupported,
					Rule:    "ddl_count",
					Message: "a transaction can include only one DDL statement",
				}, true
			}
			if dml {
				return Violation{
					Code:    CodeFeatureUnsupported,
					Rule:    "ddl_dml_mix",
					Message: "DDL and DML operations must be in separate transactions",
				}, true
			}
			ddl++
		case classify.KindDML:
			if ddl > 0 {
				return Violation{
					Code:    CodeFeatureUnsupported,
					Rule:    "ddl_dml_mix",
					Message: "DDL and DML operations must be in separate transactions",
				}, true
			}
			dml = true
		}
	}

	if !t.inTxn {
		t.ddlCount, t.dmlSeen = 0, false
	}
	for _, k := range kinds {
		switch k {
		case classify.KindBegin:
			t.inTxn = true
			t.beganAt = now
			t.ddlCount, t.dmlSeen = 0, false
		case classify.KindCommit, classify.KindRollback:
			t.inTxn = false
			t.reset()
		case classify.KindDDL:
			t.ddlCount++
		case classify.KindDML:
			t.dmlSeen = true
		}
	}

	return Violation{}, false
}

// Stats returns a snapshot of the tracker state.
func (t *Tracker) Stats() Stats {
	return Stats{
		InTxn:    t.inTxn,
		DDLCount: t.ddlCount,
		DMLSeen:  t.dmlSeen,
	}
}

func (t *Tracker) reset() {
	t.ddlCount = 0
	t.dmlSeen = false
}
