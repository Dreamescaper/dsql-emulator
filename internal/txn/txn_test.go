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
	return txn.New(txn.Limits{MaxAge: 30 * time.Minute})
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
	tr := txn.New(txn.Limits{MaxAge: time.Minute})
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
