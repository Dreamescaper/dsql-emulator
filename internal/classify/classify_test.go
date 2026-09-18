package classify_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Dreamescaper/dsql-emulator/internal/classify"
	"github.com/Dreamescaper/dsql-emulator/rules"
)

func newClassifier(t *testing.T) *classify.Classifier {
	t.Helper()
	rs, err := rules.Default()
	if err != nil {
		t.Fatalf("load ruleset: %v", err)
	}
	return classify.New(rs)
}

func TestClassifyRejectsUnsupportedStatements(t *testing.T) {
	c := newClassifier(t)

	cases := []struct {
		name string
		sql  string
		rule string
		code string
	}{
		{"truncate", "TRUNCATE widget", "truncate", "0A000"},
		{"create extension", "CREATE EXTENSION pgcrypto", "extensions", "0A000"},
		{"trigger", "CREATE TRIGGER tr AFTER INSERT ON t EXECUTE FUNCTION f()", "triggers", "0A000"},
		{"create database", "CREATE DATABASE other", "create_database", "0A000"},
		{"temporary table", "CREATE TEMPORARY TABLE t (id int)", "temporary_table", "0A000"},
		{"unlogged table", "CREATE UNLOGGED TABLE t (id int)", "unlogged_table", "0A000"},
		{"serial", "CREATE TABLE t (id serial)", "serial", "42704"},
		{"bigserial", "CREATE TABLE t (id bigserial)", "serial", "42704"},
		{"smallserial", "CREATE TABLE t (id smallserial)", "serial", "42704"},
		{"serial added by alter", "ALTER TABLE t ADD COLUMN id serial", "serial_alter_column", "42704"},
		{"unsupported type added by alter", "ALTER TABLE t ADD COLUMN c money", "unsupported_type_alter_column", "0A000"},
		// DSQL refuses the retype itself, so the target type does not matter.
		{"alter column type to an unsupported type", "ALTER TABLE t ALTER COLUMN c TYPE xml", "alter_column_type", "0A000"},
		{"alter column type to a supported type", "ALTER TABLE t ALTER COLUMN c TYPE bigint", "alter_column_type", "0A000"},
		{"alter column set data type", "ALTER TABLE t ALTER COLUMN c SET DATA TYPE varchar(20)", "alter_column_type", "0A000"},
		{"array added by alter", "ALTER TABLE t ADD COLUMN c text[]", "array_column_alter_column", "0A000"},
		{"identity without cache", "CREATE TABLE t (id bigint GENERATED ALWAYS AS IDENTITY)", "identity_cache", "0A000"},
		{"identity with small cache", "CREATE TABLE t (id bigint GENERATED ALWAYS AS IDENTITY (CACHE 100))", "identity_cache", "0A000"},
		{"materialized view", "CREATE MATERIALIZED VIEW mv AS SELECT 1", "materialized_view", "0A000"},
		{"create table as", "CREATE TABLE t AS SELECT 1", "create_table_as", "0A000"},
		{"enum type", "CREATE TYPE mood AS ENUM ('a')", "enum_type", "0A000"},
		{"composite type", "CREATE TYPE pair AS (a int, b int)", "composite_type", "0A000"},
		{"range type", "CREATE TYPE r AS RANGE (subtype = int4)", "range_type", "0A000"},
		{"sequence without cache", "CREATE SEQUENCE s", "sequence_cache", "0A000"},
		{"sequence with small cache", "CREATE SEQUENCE s CACHE 100", "sequence_cache", "0A000"},
		{"tablespace", "CREATE TABLESPACE ts LOCATION '/tmp/x'", "tablespace", "0A000"},
		{"foreign table", "CREATE FOREIGN TABLE ft (a int) SERVER s", "foreign_table", "0A000"},
		{"vacuum", "VACUUM t", "vacuum", "0A000"},
		{"listen", "LISTEN chan", "listen", "0A000"},
		{"notify", "NOTIFY chan", "notify", "0A000"},
		{"unlisten", "UNLISTEN chan", "unlisten", "0A000"},
		{"alter system", "ALTER SYSTEM SET work_mem = '1MB'", "alter_system", "0A000"},
		{"set transaction", "SET TRANSACTION READ ONLY", "set_transaction", "0A000"},
		{"savepoint", "SAVEPOINT sp", "savepoint", "0A000"},
		{"release savepoint", "RELEASE SAVEPOINT sp", "release_savepoint", "0A000"},
		{"rollback to savepoint", "ROLLBACK TO SAVEPOINT sp", "rollback_to_savepoint", "0A000"},
		{"set default isolation", "SET default_transaction_isolation = 'repeatable read'", "set_isolation", "0A000"},
		{"synchronous index", "CREATE INDEX idx ON t (a)", "sync_index", "0A000"},
		{"plpgsql function", "CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETURN 1; END $$", "create_function_language", "0A000"},
		{"unsupported type money", "CREATE TABLE t (v money)", "unsupported_type", "0A000"},
		{"unsupported type inet column", "CREATE TABLE t (v inet)", "unsupported_type", "0A000"},
		{"unsupported type bit", "CREATE TABLE t (v bit(8))", "unsupported_type", "0A000"},
		{"unsupported type geometric", "CREATE TABLE t (v point)", "unsupported_type", "0A000"},
		{"array column int", "CREATE TABLE t (v int[])", "array_column", "0A000"},
		{"array column text", "CREATE TABLE t (v text[])", "array_column", "0A000"},
		{"alter enum", "ALTER TYPE mood ADD VALUE 'meh'", "alter_enum", "0A000"},
		{"alter type rename", "ALTER TYPE mood RENAME TO mood2", "rename_type", "0A000"},
		{"drop type", "DROP TYPE IF EXISTS mood", "drop_type", "0A000"},
		{"for share", "SELECT 1 FROM t FOR SHARE", "unsupported_locking", "0A000"},
		{"for no key update", "SELECT 1 FROM t FOR NO KEY UPDATE", "unsupported_locking", "0A000"},
		{"text search", "SELECT to_tsvector('english', 'a')", "unsupported_text_search", "42704"},
		{"geometry", "SELECT line('{1,2,3}')", "unsupported_geometry", "0A000"},
		{"tablesample", "SELECT 1 FROM t TABLESAMPLE SYSTEM (1)", "tablesample", "0A000"},
		{"merge", "MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN DELETE", "merge", "0A000"},
		{"alter add constraint without NOT VALID", "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (id) REFERENCES parent(id)", "alter_add_constraint_requires_not_valid", "0A000"},
		{"alter add check without NOT VALID", "ALTER TABLE t ADD CONSTRAINT ck CHECK (a > 0)", "alter_add_constraint_requires_not_valid", "0A000"},
		{"alter validate without ASYNC", "ALTER TABLE t VALIDATE CONSTRAINT fk", "alter_validate_requires_async", "0A000"},
		{"show lc_collate", "SHOW lc_collate", "show_lc_collate", "42704"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := c.Classify(tc.sql)
			if err != nil {
				t.Fatalf("classify %q: %v", tc.sql, err)
			}
			if result.Verdict.RuleID != tc.rule {
				t.Fatalf("got rule %q want %q", result.Verdict.RuleID, tc.rule)
			}
			if result.Verdict.Code != tc.code {
				t.Fatalf("got SQLSTATE %q want %q", result.Verdict.Code, tc.code)
			}
		})
	}
}

func TestClassifyAllowsSupportedStatements(t *testing.T) {
	c := newClassifier(t)

	sqls := []string{
		"SELECT 1",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"BEGIN",
		"COMMIT",
		"CREATE TABLE t (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name text NOT NULL)",
		"CREATE TABLE t (id bigint GENERATED ALWAYS AS IDENTITY (CACHE 65536) PRIMARY KEY)",
		"CREATE TABLE t (id int PRIMARY KEY, parent_id int REFERENCES parent(id))",
		"CREATE TABLE t (id int, parent_id int REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)",
		"ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (id) REFERENCES parent(id) NOT VALID",
		"ALTER TABLE t ADD COLUMN c text",
		"ALTER TABLE t ADD COLUMN c text STORAGE PLAIN",
		"ALTER TABLE t DROP COLUMN c",
		"CREATE SEQUENCE s CACHE 65536",
		"CREATE SEQUENCE s2 CACHE 1",
		"CREATE DOMAIN d AS int",
		"CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$",
		"CREATE VIEW v AS SELECT 1 AS x",
		"CREATE SCHEMA s",
		"DROP TABLE t",
		"SELECT 1 FROM t FOR UPDATE",
		"SELECT 1 FROM t FOR KEY SHARE",
		"ANALYZE t",
		"SHOW server_version",
		"SHOW timezone",
		"SHOW client_encoding",
		"SELECT count(*) FROM t GROUP BY GROUPING SETS ((1), ())",
		"SELECT ROW_NUMBER() OVER (ORDER BY a) FROM t",
		"WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM r WHERE n < 3) SELECT n FROM r",
		"SELECT count(*) FILTER (WHERE a = 1) FROM t",
		"CREATE TABLE t (a smallint, b integer, c bigint, d real, e double precision, f numeric(18,6), g char(5), h varchar(5), i text)",
		"CREATE TABLE t2 (a date, b time, c timetz, d timestamp, e timestamptz, f interval, g boolean, h bytea, i uuid, j json, k jsonb)",
		"CREATE TABLE t3 (a int2, b int4, c int8, d float4, e float8, f bool, g bpchar(5), h decimal(10,2))",
	}

	for _, sql := range sqls {
		result, err := c.Classify(sql)
		if err != nil {
			t.Fatalf("classify %q: %v", sql, err)
		}
		if result.Verdict.Rejected() {
			t.Fatalf("%q unexpectedly rejected by rule %q", sql, result.Verdict.RuleID)
		}
	}
}

func TestClassifyForwardsUnparseableSQL(t *testing.T) {
	c := newClassifier(t)

	// Aurora DSQL syntax the stock PostgreSQL parser does not understand must
	// not be treated as a rejection; the backing server answers it.
	if _, err := c.Classify("CREATE INDEX ASYNC idx ON t (a)"); err == nil {
		t.Fatal("expected a parse error for CREATE INDEX ASYNC")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	bad := []byte("dsql_version: x\nunknown_key: 1\n")
	if _, err := rules.Load(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error for unknown ruleset field")
	}
}

func TestClassifyReportsStatementKinds(t *testing.T) {
	c := newClassifier(t)

	cases := []struct {
		sql  string
		want []classify.Kind
	}{
		{"SELECT 1", []classify.Kind{classify.KindSelect}},
		{"INSERT INTO t VALUES (1)", []classify.Kind{classify.KindDML}},
		{"UPDATE t SET a = 1", []classify.Kind{classify.KindDML}},
		{"DELETE FROM t", []classify.Kind{classify.KindDML}},
		{"CREATE TABLE t (id int)", []classify.Kind{classify.KindDDL}},
		{"ALTER TABLE t ADD COLUMN a int", []classify.Kind{classify.KindDDL}},
		{"DROP TABLE t", []classify.Kind{classify.KindDDL}},
		{"BEGIN", []classify.Kind{classify.KindBegin}},
		{"START TRANSACTION", []classify.Kind{classify.KindBegin}},
		{"COMMIT", []classify.Kind{classify.KindCommit}},
		{"ROLLBACK", []classify.Kind{classify.KindRollback}},
		{"SELECT 1; INSERT INTO t VALUES (1)", []classify.Kind{classify.KindSelect, classify.KindDML}},
	}

	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			result, err := c.Classify(tc.sql)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if len(result.Kinds) != len(tc.want) {
				t.Fatalf("got %v want %v", result.Kinds, tc.want)
			}
			for i := range tc.want {
				if result.Kinds[i] != tc.want[i] {
					t.Fatalf("got %v want %v", result.Kinds, tc.want)
				}
			}
		})
	}
}

func TestClassifyRejectsUnsupportedIsolation(t *testing.T) {
	c := newClassifier(t)

	sqls := []string{
		"BEGIN ISOLATION LEVEL SERIALIZABLE",
		"START TRANSACTION ISOLATION LEVEL READ COMMITTED",
		"SET TRANSACTION ISOLATION LEVEL SERIALIZABLE",
		"SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL SERIALIZABLE",
		"SET default_transaction_isolation = 'serializable'",
	}

	for _, sql := range sqls {
		t.Run(sql, func(t *testing.T) {
			result, err := c.Classify(sql)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if result.Verdict.Code != "0A000" {
				t.Fatalf("got SQLSTATE %q want 0A000", result.Verdict.Code)
			}
			if result.Verdict.RuleID != "isolation" {
				t.Fatalf("got rule %q want isolation", result.Verdict.RuleID)
			}
			if !strings.Contains(result.Verdict.Message, "Unsupported isolation level") {
				t.Fatalf("message %q does not mention the isolation level", result.Verdict.Message)
			}
		})
	}
}

func TestClassifyAllowsRepeatableReadIsolation(t *testing.T) {
	c := newClassifier(t)

	sqls := []string{
		"BEGIN ISOLATION LEVEL REPEATABLE READ",
	}

	for _, sql := range sqls {
		result, err := c.Classify(sql)
		if err != nil {
			t.Fatalf("classify %q: %v", sql, err)
		}
		if result.Verdict.Rejected() {
			t.Fatalf("%q unexpectedly rejected by %q", sql, result.Verdict.RuleID)
		}
	}
}

// Aurora DSQL names the type it refused, spelled as PostgreSQL displays it
// rather than as the statement wrote it.
func TestClassifyNamesTheRefusedType(t *testing.T) {
	tests := []struct {
		sql  string
		want string
	}{
		{"CREATE TABLE t (v money)", "datatype money not supported"},
		{"CREATE TABLE t (v xml)", "datatype xml not supported"},
		{"CREATE TABLE t (v bit(8))", "datatype bit not supported"},
		// varbit and int4 are names the parser uses internally; DSQL reports
		// the ones PostgreSQL displays.
		{"CREATE TABLE t (v varbit(8))", "datatype bit varying not supported"},
		{"CREATE TABLE t (v int[])", "datatype integer[] not supported"},
		{"CREATE TABLE t (v text[])", "datatype text[] not supported"},
		{"CREATE TABLE t (v int4range)", "datatype int4range not supported"},
		{"ALTER TABLE t ADD COLUMN v money", "datatype money not supported"},
		{"ALTER TABLE t ADD COLUMN v text[]", "datatype text[] not supported"},
		// A geometric type is refused through the function that builds one.
		{"SELECT line('{1,2,3}')", "datatype line not supported"},
		{"SELECT circle('<(0,0),1>'::text)", "datatype circle not supported"},
	}

	c := newClassifier(t)
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			result, err := c.Classify(tt.sql)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if !result.Verdict.Rejected() {
				t.Fatalf("%q was not refused", tt.sql)
			}
			if result.Verdict.Message != tt.want {
				t.Fatalf("got %q want %q", result.Verdict.Message, tt.want)
			}
		})
	}
}

// A refusal that names the language behaves the same way.
func TestClassifyNamesTheRefusedLanguage(t *testing.T) {
	c := newClassifier(t)
	result, err := c.Classify("CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETURN 1; END $$")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if want := "CREATE FUNCTION with language plpgsql not supported"; result.Verdict.Message != want {
		t.Fatalf("got %q want %q", result.Verdict.Message, want)
	}
}

// Aurora DSQL accepts only a bigint identity column, and its refusal names the
// type that was written. Reported as issue #2: an EF Core model with int keys
// passed against the emulator and failed on the first CREATE TABLE against a
// cluster.
func TestClassifyRefusesNonBigintIdentityColumns(t *testing.T) {
	c := newClassifier(t)

	refused := []struct {
		sql  string
		want string
	}{
		{
			sql:  "CREATE TABLE t (id integer GENERATED BY DEFAULT AS IDENTITY (CACHE 1) PRIMARY KEY)",
			want: "datatype integer not supported, identity column type must be bigint",
		},
		{
			sql:  "CREATE TABLE t (id int GENERATED ALWAYS AS IDENTITY (CACHE 1) PRIMARY KEY)",
			want: "datatype integer not supported, identity column type must be bigint",
		},
		{
			sql:  "CREATE TABLE t (id smallint GENERATED BY DEFAULT AS IDENTITY (CACHE 1))",
			want: "datatype smallint not supported, identity column type must be bigint",
		},
		{
			// The identity column is not the first, and is not the key.
			sql:  "CREATE TABLE t (name text, seq integer GENERATED BY DEFAULT AS IDENTITY (CACHE 65536))",
			want: "datatype integer not supported, identity column type must be bigint",
		},
	}
	for _, tt := range refused {
		t.Run(tt.sql, func(t *testing.T) {
			result, err := c.Classify(tt.sql)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if !result.Verdict.Rejected() {
				t.Fatalf("%q was accepted", tt.sql)
			}
			if result.Verdict.Code != "0A000" {
				t.Errorf("got SQLSTATE %q want 0A000", result.Verdict.Code)
			}
			if result.Verdict.Message != tt.want {
				t.Errorf("got %q want %q", result.Verdict.Message, tt.want)
			}
		})
	}

	accepted := []string{
		"CREATE TABLE t (id bigint GENERATED BY DEFAULT AS IDENTITY (CACHE 1) PRIMARY KEY)",
		"CREATE TABLE t (id bigint GENERATED ALWAYS AS IDENTITY (CACHE 65536))",
		// A plain integer column is fine; only an identity one is restricted.
		"CREATE TABLE t (id integer PRIMARY KEY, n smallint)",
		"CREATE TABLE t (id bigint PRIMARY KEY)",
	}
	for _, sql := range accepted {
		t.Run(sql, func(t *testing.T) {
			result, err := c.Classify(sql)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if result.Verdict.Rejected() {
				t.Fatalf("%q was refused: %s %s", sql, result.Verdict.Code, result.Verdict.Message)
			}
		})
	}
}

// An identity column with no CACHE is answered by the cache rule, which is the
// behaviour the golden record already holds; the type rule must not displace it.
func TestClassifyKeepsTheIdentityCacheRefusalFirst(t *testing.T) {
	c := newClassifier(t)
	result, err := c.Classify("CREATE TABLE t (id integer GENERATED BY DEFAULT AS IDENTITY)")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if !strings.Contains(result.Verdict.Message, "cache size") {
		t.Fatalf("got %q, want the cache refusal", result.Verdict.Message)
	}
}

// ALTER TABLE ADD COLUMN is refused for a different reason than CREATE TABLE
// is: the refusal is about the constraint, not the column's type. Recorded
// against a cluster, NOT NULL, DEFAULT, CHECK, UNIQUE and IDENTITY are all
// refused, so a bigint identity column -- which CREATE TABLE accepts -- cannot
// be added to a table that exists either.
func TestClassifyRefusesAddColumnWithAConstraint(t *testing.T) {
	c := newClassifier(t)

	const want = "ALTER TABLE ADD COLUMN with constraint not supported"
	for _, sql := range []string{
		"ALTER TABLE t ADD COLUMN seq integer GENERATED BY DEFAULT AS IDENTITY (CACHE 1)",
		"ALTER TABLE t ADD COLUMN seq bigint GENERATED BY DEFAULT AS IDENTITY (CACHE 1)",
		"ALTER TABLE t ADD COLUMN nn int NOT NULL DEFAULT 0",
		"ALTER TABLE t ADD COLUMN df int DEFAULT 7",
		"ALTER TABLE t ADD COLUMN ck int CHECK (ck > 0)",
		"ALTER TABLE t ADD COLUMN uq int UNIQUE",
		"ALTER TABLE t ADD COLUMN pk int PRIMARY KEY",
		"ALTER TABLE t ADD COLUMN fk int REFERENCES p (id)",
	} {
		t.Run(sql, func(t *testing.T) {
			result, err := c.Classify(sql)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if !result.Verdict.Rejected() {
				t.Fatalf("%q was accepted", sql)
			}
			if result.Verdict.Code != "0A000" {
				t.Errorf("got SQLSTATE %q want 0A000", result.Verdict.Code)
			}
			if result.Verdict.Message != want {
				t.Errorf("got %q want %q", result.Verdict.Message, want)
			}
		})
	}

	// A column added without one is what the record shows being accepted.
	// COLLATE and STORAGE carry no constraint, and an explicit NULL states the
	// default nullability rather than restricting anything; it is left through
	// until a recording says otherwise.
	for _, sql := range []string{
		"ALTER TABLE t ADD COLUMN c text",
		"ALTER TABLE t ADD COLUMN d text STORAGE PLAIN",
		`ALTER TABLE t ADD COLUMN e text COLLATE "C"`,
		"ALTER TABLE t ADD COLUMN f int NULL",
	} {
		t.Run(sql, func(t *testing.T) {
			result, err := c.Classify(sql)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if result.Verdict.Rejected() {
				t.Fatalf("%q was refused: %s %s", sql, result.Verdict.Code, result.Verdict.Message)
			}
		})
	}
}

// Aurora DSQL's ALTER TABLE cannot add a key constraint to a table that already
// exists; the key has to be declared with the table. Reported as issue #3: a
// schema script that adds primary keys afterwards passes against the emulator
// and fails partway through against a cluster, once the tables are already
// there.
func TestClassifyRefusesAlterTableAddKeyConstraint(t *testing.T) {
	c := newClassifier(t)

	refused := []string{
		"ALTER TABLE t ADD CONSTRAINT pk_t PRIMARY KEY (id)",
		"ALTER TABLE t ADD PRIMARY KEY (id)",
		"ALTER TABLE t ADD CONSTRAINT uq_t UNIQUE (a, b)",
		"ALTER TABLE t ADD UNIQUE (a)",
	}
	for _, sql := range refused {
		t.Run(sql, func(t *testing.T) {
			result, err := c.Classify(sql)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if !result.Verdict.Rejected() {
				t.Fatalf("%q was accepted", sql)
			}
			if result.Verdict.Code != "0A000" {
				t.Errorf("got SQLSTATE %q want 0A000", result.Verdict.Code)
			}
			if want := "unsupported ALTER TABLE ADD CONSTRAINT statement"; result.Verdict.Message != want {
				t.Errorf("got %q want %q", result.Verdict.Message, want)
			}
		})
	}

	accepted := []string{
		// DSQL takes this one as far as complaining about the index, which the
		// alter_unique_using_index probe records, so it must reach the backend.
		"ALTER TABLE t ADD CONSTRAINT uq_t UNIQUE USING INDEX uq_idx",
		"ALTER TABLE t ADD CONSTRAINT pk_t PRIMARY KEY USING INDEX pk_idx",
		// A key declared with the table is how DSQL expects one.
		"CREATE TABLE t (id int PRIMARY KEY, a text UNIQUE)",
		// The constraint kinds the dialect does allow through ALTER TABLE.
		"ALTER TABLE t ADD CONSTRAINT ck_t CHECK (a > 0) NOT VALID",
		"ALTER TABLE t ADD CONSTRAINT fk_t FOREIGN KEY (p) REFERENCES p (id) NOT VALID",
	}
	for _, sql := range accepted {
		t.Run(sql, func(t *testing.T) {
			result, err := c.Classify(sql)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if result.Verdict.Rejected() {
				t.Fatalf("%q was refused: %s %s", sql, result.Verdict.Code, result.Verdict.Message)
			}
		})
	}
}
