package txn_test

import (
	"testing"
	"time"

	"github.com/Dreamescaper/dsql-emulator/internal/classify"
	"github.com/Dreamescaper/dsql-emulator/internal/txn"
)

var (
	begin    = []classify.Kind{classify.KindBegin}
	commit   = []classify.Kind{classify.KindCommit}
	rollback = []classify.Kind{classify.KindRollback}
	ddl      = []classify.Kind{classify.KindDDL}
	dml      = []classify.Kind{classify.KindDML}
)

func newTracker() *txn.Tracker {
	return txn.New(txn.Limits{DMLRows: 3000, MaxAge: 30 * time.Minute})
}

func TestAdmitAllowsDDLInSeparateTransactions(t *testing.T) {
	tr := newTracker()
	now := time.Now()

	if _, bad := tr.Admit(ddl, now); bad {
		t.Fatal("first implicit DDL rejected")
	}
	if _, bad := tr.Admit(ddl, now); bad {
		t.Fatal("DDL in a new transaction rejected")
	}
}

func TestAdmitRejectsSecondDDLInTransaction(t *testing.T) {
	tr := newTracker()
	now := time.Now()

	tr.Admit(begin, now)
	if _, bad := tr.Admit(ddl, now); bad {
		t.Fatal("first DDL rejected")
	}
	v, bad := tr.Admit(ddl, now)
	if !bad {
		t.Fatal("second DDL in the same transaction was allowed")
	}
	if v.Code != txn.CodeFeatureUnsupported || v.Rule != "ddl_count" {
		t.Fatalf("got %+v", v)
	}
}

func TestAdmitRejectsDDLAndDMLTogether(t *testing.T) {
	tr := newTracker()
	now := time.Now()

	tr.Admit(begin, now)
	tr.Admit(ddl, now)

	if v, bad := tr.Admit(dml, now); !bad {
		t.Fatal("DML after DDL in the same transaction was allowed")
	} else if v.Rule != "ddl_dml_mix" {
		t.Fatalf("got rule %q", v.Rule)
	}

	tr2 := newTracker()
	tr2.Admit(begin, now)
	tr2.Admit(dml, now)
	if v, bad := tr2.Admit(ddl, now); !bad {
		t.Fatal("DDL after DML in the same transaction was allowed")
	} else if v.Rule != "ddl_dml_mix" {
		t.Fatalf("got rule %q", v.Rule)
	}
}

func TestAdmitRejectsMixedBatchInImplicitTransaction(t *testing.T) {
	tr := newTracker()
	mixed := []classify.Kind{classify.KindDDL, classify.KindDML}

	if _, bad := tr.Admit(mixed, time.Now()); !bad {
		t.Fatal("DDL and DML in one implicit transaction was allowed")
	}
}

func TestAdmitAllowsManyDMLStatements(t *testing.T) {
	tr := newTracker()
	now := time.Now()

	tr.Admit(begin, now)
	for i := 0; i < 5; i++ {
		if _, bad := tr.Admit(dml, now); bad {
			t.Fatalf("DML statement %d rejected", i)
		}
	}
}

func TestAdmitResetsAfterCommit(t *testing.T) {
	tr := newTracker()
	now := time.Now()

	tr.Admit(begin, now)
	tr.Admit(ddl, now)
	tr.Admit(commit, now)

	if _, bad := tr.Admit(ddl, now); bad {
		t.Fatal("DDL after COMMIT was rejected")
	}
}

func TestAdmitEnforcesTransactionAge(t *testing.T) {
	tr := txn.New(txn.Limits{DMLRows: 3000, MaxAge: time.Minute})
	start := time.Now()

	tr.Admit(begin, start)
	if _, bad := tr.Admit(dml, start.Add(30*time.Second)); bad {
		t.Fatal("statement within the age limit was rejected")
	}

	v, bad := tr.Admit(dml, start.Add(2*time.Minute))
	if !bad {
		t.Fatal("statement past the age limit was allowed")
	}
	if v.Code != txn.CodeProgramLimit || v.Rule != "txn_age" {
		t.Fatalf("got %+v", v)
	}

	if _, bad := tr.Admit(rollback, start.Add(2*time.Minute)); bad {
		t.Fatal("ROLLBACK was refused after the age limit was exceeded")
	}
}

func TestAdmitEnforcesRowCap(t *testing.T) {
	tr := txn.New(txn.Limits{DMLRows: 3000, MaxAge: time.Hour})
	now := time.Now()

	tr.Admit(begin, now)
	tr.RecordRows(2000)
	if _, bad := tr.Admit(dml, now); bad {
		t.Fatal("statement under the row cap was rejected")
	}

	tr.RecordRows(2000)
	if v, bad := tr.Admit(dml, now); !bad {
		t.Fatal("statement past the row cap was allowed")
	} else if v.Code != txn.CodeProgramLimit || v.Rule != "dml_rows" {
		t.Fatalf("got %+v", v)
	}

	if _, bad := tr.Admit(rollback, now); bad {
		t.Fatal("ROLLBACK was refused after the row cap was exceeded")
	}
	if _, bad := tr.Admit(dml, now); bad {
		t.Fatal("new transaction after ROLLBACK was rejected")
	}
}

func TestAdmitIgnoresRowCapOutsideExplicitTransaction(t *testing.T) {
	tr := newTracker()

	tr.Admit(dml, time.Now())
	tr.RecordRows(10000)

	if _, bad := tr.Admit(dml, time.Now()); bad {
		t.Fatal("implicit transaction must not be blocked by a previous statement's rows")
	}
}

func TestRowsFromCommandTag(t *testing.T) {
	cases := []struct {
		tag  string
		want int64
	}{
		{"INSERT 0 5", 5},
		{"INSERT 0 0", 0},
		{"UPDATE 3", 3},
		{"DELETE 2", 2},
		{"MERGE 7", 7},
		{"SELECT 5", 0},
		{"CREATE TABLE", 0},
		{"BEGIN", 0},
		{"", 0},
	}

	for _, tc := range cases {
		if got := txn.RowsFromCommandTag(tc.tag); got != tc.want {
			t.Fatalf("RowsFromCommandTag(%q) = %d want %d", tc.tag, got, tc.want)
		}
	}
}
