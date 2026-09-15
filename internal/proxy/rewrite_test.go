package proxy

import "testing"

func TestParseAsyncIndex(t *testing.T) {
	cases := []struct {
		sql       string
		rewritten string
		name      string
		qualified bool
	}{
		{"CREATE INDEX ASYNC idx ON t (a)", "CREATE INDEX idx ON t (a)", "idx", false},
		{"CREATE UNIQUE INDEX ASYNC idx ON t (a)", "CREATE UNIQUE INDEX idx ON t (a)", "idx", false},
		{"CREATE INDEX ASYNC IF NOT EXISTS idx ON t (a)", "CREATE INDEX IF NOT EXISTS idx ON t (a)", "idx", false},
		{"  create index async MyIdx on t (a)", "  create index MyIdx on t (a)", "myidx", false},
		{`CREATE INDEX ASYNC "MyIdx" ON t (a)`, `CREATE INDEX "MyIdx" ON t (a)`, "MyIdx", false},
		{`CREATE INDEX ASYNC "My""Idx" ON t (a)`, `CREATE INDEX "My""Idx" ON t (a)`, `My"Idx`, false},
		// Aurora DSQL puts the index in the table's schema, so a qualified name
		// is refused rather than rewritten.
		{"CREATE INDEX ASYNC s.idx ON t (a)", "CREATE INDEX s.idx ON t (a)", "idx", true},
		{`CREATE INDEX ASYNC "s"."Idx" ON t (a)`, `CREATE INDEX "s"."Idx" ON t (a)`, "Idx", true},

		// Partial index, the clause this was extended for.
		{"CREATE INDEX ASYNC idx ON t (a) WHERE a > 0", "CREATE INDEX idx ON t (a) WHERE a > 0", "idx", false},
		{"CREATE UNIQUE INDEX ASYNC idx ON t (a) WHERE a IS NOT NULL AND b = 'x'", "CREATE UNIQUE INDEX idx ON t (a) WHERE a IS NOT NULL AND b = 'x'", "idx", false},
		{"CREATE INDEX ASYNC idx ON t (a) INCLUDE (b) WHERE b IS NOT NULL", "CREATE INDEX idx ON t (a) INCLUDE (b) WHERE b IS NOT NULL", "idx", false},
		{"CREATE UNIQUE INDEX ASYNC idx ON t (a) NULLS NOT DISTINCT", "CREATE UNIQUE INDEX idx ON t (a) NULLS NOT DISTINCT", "idx", false},

		// No name: the server chooses one, so there is nothing to derive a job
		// id from.
		{"CREATE INDEX ASYNC ON t ((lower(a)))", "CREATE INDEX ON t ((lower(a)))", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			index, ok := parseAsyncIndex(tc.sql)
			if !ok {
				t.Fatalf("%q was not recognised as an async index", tc.sql)
			}
			if index.rewritten != tc.rewritten {
				t.Fatalf("rewrote to %q want %q", index.rewritten, tc.rewritten)
			}
			if index.name != tc.name {
				t.Fatalf("index name %q want %q", index.name, tc.name)
			}
			if index.qualified != tc.qualified {
				t.Fatalf("qualified %v want %v", index.qualified, tc.qualified)
			}
		})
	}
}

func TestParseAsyncIndexLeavesOtherStatementsAlone(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX idx ON t (a)",
		"CREATE INDEX idx ON t (a) WHERE a > 0",
		"CREATE INDEX CONCURRENTLY idx ON t (a)",
		"SELECT 1",
		"CREATE TABLE t (id int)",
	} {
		index, ok := parseAsyncIndex(sql)
		if ok {
			t.Fatalf("%q should not be rewritten", sql)
		}
		if index.rewritten != "" || index.name != "" {
			t.Fatalf("%q: got %+v", sql, index)
		}
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

func TestJobIDForIndex(t *testing.T) {
	// The backing database derives the id the same way, formatting md5 of the
	// index name as a UUID, which is also what wait_for_job accepts.
	if got, want := jobIDForIndex("baseline_idx_value_async"), "a5ffafe2-ce6e-8fbb-ba9f-3fdfe857fcf4"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := jobIDForIndex(""); got != "" {
		t.Fatalf("got %q want an empty id for an unknown name", got)
	}
}
