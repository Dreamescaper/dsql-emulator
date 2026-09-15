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
		"ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (id) REFERENCES parent(id)",
		"CREATE SEQUENCE s CACHE 65536",
		"CREATE SEQUENCE s2 CACHE 1",
		"CREATE DOMAIN d AS int",
		"CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$",
		"CREATE VIEW v AS SELECT 1 AS x",
		"CREATE SCHEMA s",
		"DROP TABLE t",
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
