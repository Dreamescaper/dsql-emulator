package rules_test

import (
	"strings"
	"testing"

	"github.com/Dreamescaper/dsql-emulator/rules"
)

// minimal is the smallest ruleset that loads, so a test can add just the part
// it is about.
const minimal = `
dsql_version: "test"
isolation:
  supported: ["repeatable read"]
`

func TestDefaultRulesetIsValid(t *testing.T) {
	rs, err := rules.Default()
	if err != nil {
		t.Fatalf("the embedded ruleset does not load: %v", err)
	}
	if rs.DSQLVersion == "" {
		t.Error("the embedded ruleset has no dsql_version")
	}
	if len(rs.Unsupported) == 0 {
		t.Error("the embedded ruleset refuses nothing")
	}
	if rs.OCC.LockTimeoutMS <= 0 {
		t.Errorf("lock_timeout_ms is %d; the adjudicator needs the backend to stop waiting", rs.OCC.LockTimeoutMS)
	}
}

// An injection rule that is quietly ignored is worse than one that is refused:
// the transaction it was meant to fail commits, and a retry loop under test
// never runs.
func TestLoadRefusesInjectionRulesThatWouldNeverFire(t *testing.T) {
	tests := []struct {
		name string
		occ  string
		want string
	}{
		{
			name: "no id",
			occ:  "  inject:\n    - tables: [\"t\"]\n",
			want: "has no id",
		},
		{
			name: "duplicate id",
			occ:  "  inject:\n    - id: dup\n      tables: [\"a\"]\n    - id: dup\n      tables: [\"b\"]\n",
			want: `duplicate occ injection id "dup"`,
		},
		{
			name: "negative every",
			occ:  "  inject:\n    - id: backwards\n      every: -1\n",
			want: "use 1 for every commit",
		},
		{
			name: "empty table name",
			occ:  "  inject:\n    - id: blank\n      tables: [\"\"]\n",
			want: "names an empty table",
		},
		{
			name: "negative lock timeout",
			occ:  "  lock_timeout_ms: -5\n",
			want: "use 0 to let the backend wait",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rules.Load(strings.NewReader(minimal + "occ:\n" + tt.occ))
			if err == nil {
				t.Fatal("the ruleset loaded, so the rule would have been ignored at runtime")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadAcceptsUsableInjectionRules(t *testing.T) {
	rs, err := rules.Load(strings.NewReader(minimal + `
occ:
  lock_timeout_ms: 50
  inject:
    - id: every_commit
      tables: ["orders"]
    - id: every_third
      tables: ["orders", "items"]
      every: 3
    - id: any_table
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rs.OCC.Inject) != 3 {
		t.Fatalf("got %d injection rules want 3", len(rs.OCC.Inject))
	}
	// No Every means every matching commit, and no tables means any transaction.
	if rs.OCC.Inject[0].Every != 0 {
		t.Errorf("got every %d want 0", rs.OCC.Inject[0].Every)
	}
	if len(rs.OCC.Inject[2].Tables) != 0 {
		t.Errorf("got tables %v want none", rs.OCC.Inject[2].Tables)
	}
}

func TestLoadRefusesIncompleteRules(t *testing.T) {
	tests := []struct {
		name    string
		ruleset string
		want    string
	}{
		{name: "no version", ruleset: "isolation:\n  supported: [\"repeatable read\"]\n", want: "dsql_version is required"},
		{name: "no isolation", ruleset: "dsql_version: \"test\"\n", want: "at least one level"},
		{
			name:    "rule without a code",
			ruleset: minimal + "unsupported:\n  - id: r\n    stmt: create_stmt\n    message: \"no\"\n",
			want:    "has no code",
		},
		{
			name:    "duplicate rule id",
			ruleset: minimal + "unsupported:\n  - id: r\n    stmt: create_stmt\n    code: \"0A000\"\n    message: \"no\"\n  - id: r\n    stmt: drop_stmt\n    code: \"0A000\"\n    message: \"no\"\n",
			want:    "duplicate rule id",
		},
		{
			name:    "a key the ruleset does not define",
			ruleset: minimal + "occ:\n  lock_timeout_msec: 50\n",
			want:    "decode ruleset",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rules.Load(strings.NewReader(tt.ruleset))
			if err == nil {
				t.Fatal("expected the ruleset to be refused")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %q, want it to mention %q", err, tt.want)
			}
		})
	}
}
