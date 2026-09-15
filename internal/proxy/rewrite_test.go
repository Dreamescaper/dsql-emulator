package proxy

import "testing"

func TestRewriteAsyncIndex(t *testing.T) {
	cases := []struct {
		sql       string
		rewritten string
		name      string
	}{
		{"CREATE INDEX ASYNC idx ON t (a)", "CREATE INDEX idx ON t (a)", "idx"},
		{"CREATE UNIQUE INDEX ASYNC idx ON t (a)", "CREATE UNIQUE INDEX idx ON t (a)", "idx"},
		{"CREATE INDEX ASYNC IF NOT EXISTS idx ON t (a)", "CREATE INDEX IF NOT EXISTS idx ON t (a)", "idx"},
		{"  create index async MyIdx on t (a)", "  create index MyIdx on t (a)", "myidx"},
		{`CREATE INDEX ASYNC "MyIdx" ON t (a)`, `CREATE INDEX "MyIdx" ON t (a)`, "MyIdx"},
		{`CREATE INDEX ASYNC "My""Idx" ON t (a)`, `CREATE INDEX "My""Idx" ON t (a)`, `My"Idx`},
		{"CREATE INDEX ASYNC s.idx ON t (a)", "CREATE INDEX s.idx ON t (a)", "idx"},
		{`CREATE INDEX ASYNC "s"."Idx" ON t (a)`, `CREATE INDEX "s"."Idx" ON t (a)`, "Idx"},
	}

	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			rewritten, name, ok := rewriteAsyncIndex(tc.sql)
			if !ok {
				t.Fatalf("%q was not recognised as an async index", tc.sql)
			}
			if rewritten != tc.rewritten {
				t.Fatalf("rewrote to %q want %q", rewritten, tc.rewritten)
			}
			if name != tc.name {
				t.Fatalf("index name %q want %q", name, tc.name)
			}
		})
	}
}

func TestRewriteAsyncIndexLeavesOtherStatementsAlone(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX idx ON t (a)",
		"CREATE INDEX CONCURRENTLY idx ON t (a)",
		"SELECT 1",
		"CREATE TABLE t (id int)",
	} {
		rewritten, name, ok := rewriteAsyncIndex(sql)
		if ok {
			t.Fatalf("%q should not be rewritten", sql)
		}
		if rewritten != sql || name != "" {
			t.Fatalf("%q: got (%q, %q)", sql, rewritten, name)
		}
	}
}

func TestJobIDForIndex(t *testing.T) {
	// The backing database derives the id the same way, with md5, so this value
	// is what `select md5('baseline_idx_value_async')` returns.
	if got, want := jobIDForIndex("baseline_idx_value_async"), "a5ffafe2ce6e8fbbba9f3fdfe857fcf4"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := jobIDForIndex(""); got != "" {
		t.Fatalf("got %q want an empty id for an unknown name", got)
	}
}
