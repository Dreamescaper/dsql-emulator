package proxy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestStripAsync(t *testing.T) {
	cases := []struct {
		sql       string
		rewritten string
	}{
		{"CREATE INDEX ASYNC idx ON t (a)", "CREATE INDEX idx ON t (a)"},
		{"CREATE UNIQUE INDEX ASYNC idx ON t (a)", "CREATE UNIQUE INDEX idx ON t (a)"},
		{"CREATE INDEX ASYNC IF NOT EXISTS idx ON t (a)", "CREATE INDEX IF NOT EXISTS idx ON t (a)"},
		{"  create index async MyIdx on t (a)", "  create index MyIdx on t (a)"},
		{`CREATE INDEX ASYNC "MyIdx" ON t (a)`, `CREATE INDEX "MyIdx" ON t (a)`},
		{`CREATE INDEX ASYNC "My""Idx" ON t (a)`, `CREATE INDEX "My""Idx" ON t (a)`},

		// Aurora DSQL puts the index in the table's schema and its grammar does
		// not accept a qualified name. The name is left as it was, so
		// PostgreSQL answers with the same syntax error.
		{"CREATE INDEX ASYNC s.idx ON t (a)", "CREATE INDEX s.idx ON t (a)"},
		{`CREATE INDEX ASYNC "s"."Idx" ON t (a)`, `CREATE INDEX "s"."Idx" ON t (a)`},

		// Partial index, the clause this was extended for.
		{"CREATE INDEX ASYNC idx ON t (a) WHERE a > 0", "CREATE INDEX idx ON t (a) WHERE a > 0"},
		{"CREATE UNIQUE INDEX ASYNC idx ON t (a) WHERE a IS NOT NULL AND b = 'x'", "CREATE UNIQUE INDEX idx ON t (a) WHERE a IS NOT NULL AND b = 'x'"},
		{"CREATE INDEX ASYNC idx ON t (a) INCLUDE (b) WHERE b IS NOT NULL", "CREATE INDEX idx ON t (a) INCLUDE (b) WHERE b IS NOT NULL"},
		{"CREATE UNIQUE INDEX ASYNC idx ON t (a) NULLS NOT DISTINCT", "CREATE UNIQUE INDEX idx ON t (a) NULLS NOT DISTINCT"},

		// No name: the server chooses one. The job id does not come from the
		// name, so this is no different from any other.
		{"CREATE INDEX ASYNC ON t ((lower(a)))", "CREATE INDEX ON t ((lower(a)))"},
	}

	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			rewritten, _, ok := stripAsync(tc.sql)
			if !ok {
				t.Fatalf("%q was not recognised as an async index", tc.sql)
			}
			if rewritten != tc.rewritten {
				t.Fatalf("rewrote to %q want %q", rewritten, tc.rewritten)
			}
		})
	}
}

func TestStripAsyncLeavesOtherStatementsAlone(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX idx ON t (a)",
		"CREATE INDEX idx ON t (a) WHERE a > 0",
		"CREATE INDEX CONCURRENTLY idx ON t (a)",
		"SELECT 1",
		"CREATE TABLE t (id int)",
	} {
		rewritten, _, ok := stripAsync(sql)
		if ok {
			t.Fatalf("%q should not be rewritten", sql)
		}
		if rewritten != "" {
			t.Fatalf("%q: got %q", sql, rewritten)
		}
	}
}

func TestStripAsyncAlterTable(t *testing.T) {
	rewritten, _, ok := stripAsync("ALTER TABLE ASYNC t VALIDATE CONSTRAINT c")
	if !ok {
		t.Fatal("ALTER TABLE ASYNC was not recognised")
	}
	if want := "ALTER TABLE t VALIDATE CONSTRAINT c"; rewritten != want {
		t.Fatalf("rewrote to %q want %q", rewritten, want)
	}
	if _, _, ok := stripAsync("ALTER TABLE t VALIDATE CONSTRAINT c"); ok {
		t.Fatal("the synchronous form should not be rewritten")
	}
}

// A simple query may hold several statements, and psql -c sends one string for
// all of them. The keyword has to come off wherever it sits, or PostgreSQL
// answers with a syntax error where Aurora DSQL answers with its own rule.
func TestStripAsyncInAMultiStatementQuery(t *testing.T) {
	cases := []struct {
		name      string
		sql       string
		rewritten string
		statement int
	}{
		{
			name:      "after another DDL",
			sql:       "CREATE TABLE t (id int); CREATE INDEX ASYNC idx ON t (id)",
			rewritten: "CREATE TABLE t (id int); CREATE INDEX idx ON t (id)",
			statement: 1,
		},
		{
			name:      "inside a transaction",
			sql:       "BEGIN; CREATE INDEX ASYNC idx ON t (a); COMMIT",
			rewritten: "BEGIN; CREATE INDEX idx ON t (a); COMMIT",
			statement: 1,
		},
		{
			name:      "first of several",
			sql:       "CREATE INDEX ASYNC idx ON t (a); SELECT 1",
			rewritten: "CREATE INDEX idx ON t (a); SELECT 1",
			statement: 0,
		},
		{
			name:      "the alter form",
			sql:       "BEGIN; ALTER TABLE ASYNC t VALIDATE CONSTRAINT c; COMMIT",
			rewritten: "BEGIN; ALTER TABLE t VALIDATE CONSTRAINT c; COMMIT",
			statement: 1,
		},
		{
			name:      "more than one",
			sql:       "CREATE INDEX ASYNC a ON t (x); CREATE INDEX ASYNC b ON t (y)",
			rewritten: "CREATE INDEX a ON t (x); CREATE INDEX b ON t (y)",
			statement: 0,
		},
		{
			name:      "a trailing semicolon is not a statement",
			sql:       "CREATE INDEX ASYNC idx ON t (a);",
			rewritten: "CREATE INDEX idx ON t (a);",
			statement: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rewritten, statement, ok := stripAsync(tc.sql)
			if !ok {
				t.Fatalf("%q was not recognised", tc.sql)
			}
			if rewritten != tc.rewritten {
				t.Errorf("rewrote to %q want %q", rewritten, tc.rewritten)
			}
			if statement != tc.statement {
				t.Errorf("job on statement %d want %d", statement, tc.statement)
			}
		})
	}
}

// The word is only the dialect's keyword where the grammar puts it. Anywhere
// else it is someone's data or someone's identifier.
func TestStripAsyncOnlyTouchesTheKeyword(t *testing.T) {
	for _, sql := range []string{
		// An index that is actually named ASYNC.
		`CREATE INDEX "ASYNC" ON t (a)`,
		`CREATE INDEX "async" ON t (a)`,
		// The word inside data, a comment, and a dollar-quoted body.
		"INSERT INTO t (v) VALUES ('CREATE INDEX ASYNC idx ON t (a)')",
		"SELECT 'async' FROM t",
		"CREATE INDEX idx ON t (a) /* CREATE INDEX ASYNC */",
		"SELECT $$ CREATE INDEX ASYNC idx ON t (a) $$",
		// A column or table that happens to be called async.
		"SELECT async FROM t",
		"CREATE TABLE async (id int)",
		"ALTER TABLE async ADD COLUMN b int",
	} {
		if rewritten, _, ok := stripAsync(sql); ok {
			t.Errorf("%q should not be rewritten, got %q", sql, rewritten)
		}
	}
}

// The marker rides on the statement itself, so the backing database records the
// job under the same id the client is handed.
func TestJobMarkerCarriesTheJobID(t *testing.T) {
	id := newJobID()
	marked := jobMarker(id) + "CREATE INDEX idx ON t (a)"
	if !strings.Contains(marked, "dsql_job="+id) {
		t.Fatalf("marker lost the id: %q", marked)
	}
	if !strings.HasSuffix(marked, "CREATE INDEX idx ON t (a)") {
		t.Fatalf("marker did not prefix the statement: %q", marked)
	}
}

func TestServerVersionNum(t *testing.T) {
	// Aurora DSQL reports server_version 16.15 and server_version_num 160015.
	cases := map[string]string{
		"16.15":   "160015",
		"16":      "160000",
		"16.1.2":  "160102",
		"17.4.1":  "170401",
		"":        "0",
		"garbage": "0",
	}
	for version, want := range cases {
		if got := serverVersionNum(version); got != want {
			t.Fatalf("serverVersionNum(%q) = %q want %q", version, got, want)
		}
	}
}

// A job id is shaped the way Aurora DSQL shapes one -- 26 base32 characters,
// as in the tpqrncdmjja4tdl3zxo2qqvh4y the golden record holds -- so a client
// that stores one in a column of its own finds the same width against either.
func TestNewJobIDMatchesTheShapeDSQLUses(t *testing.T) {
	shape := regexp.MustCompile(`^[a-z2-7]{26}$`)

	seen := make(map[string]bool, 64)
	for range 64 {
		id := newJobID()
		if !shape.MatchString(id) {
			t.Fatalf("got %q (%d chars), want 26 characters of lowercase base32", id, len(id))
		}
		if seen[id] {
			t.Fatalf("two job ids came out the same: %q", id)
		}
		seen[id] = true
	}
}

// The shape the emulator writes, the shape the backing database reads out of
// the marker comment, and the shape sys.wait_for_job accepts all have to agree.
// The init scripts carry the same pattern as a literal, so this reads them.
func TestJobIDShapeAgreesWithTheInitScripts(t *testing.T) {
	for _, script := range []string{"01-sys.sql", "04-jobs.sql"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "docker", "init", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		if !strings.Contains(string(body), jobIDPattern) {
			t.Errorf("%s does not carry the job id shape %s", script, jobIDPattern)
		}
	}

	// And the shape actually matches what the emulator generates.
	if !regexp.MustCompile("^" + jobIDPattern + "$").MatchString(newJobID()) {
		t.Error("a generated job id does not match the shape the init scripts read")
	}
}
