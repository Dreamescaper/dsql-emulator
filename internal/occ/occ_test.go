package occ_test

import (
	"testing"

	"github.com/Dreamescaper/dsql-emulator/internal/occ"
)

func TestIsConflict(t *testing.T) {
	for _, code := range []string{"55P03", "40001"} {
		if !occ.IsConflict(code) {
			t.Errorf("IsConflict(%q) = false, want true", code)
		}
	}
	for _, code := range []string{"40P01", "23505", "", "55000"} {
		if occ.IsConflict(code) {
			t.Errorf("IsConflict(%q) = true, want false", code)
		}
	}
}

func TestAnalyzeBuildsShadowStatements(t *testing.T) {
	tests := []struct {
		name   string
		sql    string
		tag    string
		rows   int
		shadow string
		rowset bool
	}{
		{
			name:   "update counts its target rows",
			sql:    "UPDATE baseline_conflict SET name = 'ww-a' WHERE id = 'ac'",
			tag:    "UPDATE",
			rows:   -1,
			shadow: "SELECT count(*) FROM baseline_conflict WHERE id = 'ac'",
		},
		{
			name:   "delete counts its target rows",
			sql:    "DELETE FROM baseline_conflict WHERE id = 'ac'",
			tag:    "DELETE",
			rows:   -1,
			shadow: "SELECT count(*) FROM baseline_conflict WHERE id = 'ac'",
		},
		{
			name:   "update carries its from clause",
			sql:    "UPDATE a SET x = 1 FROM b WHERE a.id = b.id",
			tag:    "UPDATE",
			rows:   -1,
			shadow: "SELECT count(*) FROM a, b WHERE a.id = b.id",
		},
		{
			name:   "delete carries its using clause",
			sql:    "DELETE FROM a USING b WHERE a.id = b.id",
			tag:    "DELETE",
			rows:   -1,
			shadow: "SELECT count(*) FROM a, b WHERE a.id = b.id",
		},
		{
			name:   "unqualified update counts the whole table",
			sql:    "UPDATE t SET v = 1",
			tag:    "UPDATE",
			rows:   -1,
			shadow: "SELECT count(*) FROM t",
		},
		{
			name: "insert states its own row count",
			sql:  "INSERT INTO child (id, parent_id) VALUES ('b0', 'ab')",
			tag:  "INSERT",
			rows: 1,
		},
		{
			name: "insert counts every values row",
			sql:  "INSERT INTO t (a) VALUES (1), (2), (3)",
			tag:  "INSERT",
			rows: 3,
		},
		{
			name:   "for update drops only the locking clause",
			sql:    "SELECT name FROM baseline_conflict WHERE id = 'ac' FOR UPDATE",
			tag:    "SELECT",
			rows:   -1,
			shadow: "SELECT name FROM baseline_conflict WHERE id = 'ac'",
			rowset: true,
		},
		{
			name:   "for key share drops only the locking clause",
			sql:    "SELECT name FROM baseline_conflict WHERE id = 'ac' FOR KEY SHARE",
			tag:    "SELECT",
			rows:   -1,
			shadow: "SELECT name FROM baseline_conflict WHERE id = 'ac'",
			rowset: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := occ.Analyze(tt.sql)
			if got == nil {
				t.Fatalf("Analyze(%q) = nil, want an intent", tt.sql)
			}
			if got.Tag != tt.tag {
				t.Errorf("tag = %q, want %q", got.Tag, tt.tag)
			}
			if got.Rows != tt.rows {
				t.Errorf("rows = %d, want %d", got.Rows, tt.rows)
			}
			if got.Shadow != tt.shadow {
				t.Errorf("shadow = %q, want %q", got.Shadow, tt.shadow)
			}
			if got.Rowset != tt.rowset {
				t.Errorf("rowset = %v, want %v", got.Rowset, tt.rowset)
			}
		})
	}
}

// Statements whose answer the emulator cannot reproduce keep PostgreSQL's
// behavior rather than being reported as a commit-time conflict.
func TestAnalyzeDeclinesWhatItCannotReproduce(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM t",
		"UPDATE t SET v = 1 RETURNING id",
		"DELETE FROM t WHERE id = 1 RETURNING id",
		"INSERT INTO t (a) VALUES (1) RETURNING id",
		"INSERT INTO t (a) VALUES (1) ON CONFLICT DO NOTHING",
		"INSERT INTO t SELECT * FROM u",
		"BEGIN",
		"COMMIT",
		"CREATE TABLE t (id int)",
		"UPDATE a SET v = 1; UPDATE b SET v = 2",
		"not sql at all",
	} {
		if got := occ.Analyze(sql); got != nil {
			t.Errorf("Analyze(%q) = %+v, want nil", sql, got)
		}
	}
}

func TestCommandTag(t *testing.T) {
	tests := []struct {
		tag  string
		rows int
		want string
	}{
		{"UPDATE", 1, "UPDATE 1"},
		{"DELETE", 0, "DELETE 0"},
		{"SELECT", 2, "SELECT 2"},
		{"INSERT", 1, "INSERT 0 1"},
	}
	for _, tt := range tests {
		if got := (occ.Intent{Tag: tt.tag}).CommandTag(tt.rows); got != tt.want {
			t.Errorf("CommandTag(%d) for %s = %q, want %q", tt.rows, tt.tag, got, tt.want)
		}
	}
}

// A parameterised statement has no shadow: the values the client bound belong
// to the statement that was refused, not to the one that would replace it.
func TestAnalyzeDeclinesParameterisedStatements(t *testing.T) {
	for _, sql := range []string{
		"UPDATE t SET v = $1 WHERE id = $2",
		"DELETE FROM t WHERE id = $1",
		"INSERT INTO t (a) VALUES ($1)",
		"SELECT v FROM t WHERE id = $1 FOR UPDATE",
	} {
		if got := occ.Analyze(sql); got != nil {
			t.Errorf("Analyze(%q) = %+v, want nil", sql, got)
		}
	}
}
