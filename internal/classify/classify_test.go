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
	}{
		{"truncate", "TRUNCATE widget", "truncate"},
		{"create extension", "CREATE EXTENSION pgcrypto", "extensions"},
		{"trigger", "CREATE TRIGGER tr AFTER INSERT ON t EXECUTE FUNCTION f()", "triggers"},
		{"create database", "CREATE DATABASE other", "create_database"},
		{"temporary table", "CREATE TEMPORARY TABLE t (id int)", "temporary_table"},
		{"unlogged table", "CREATE UNLOGGED TABLE t (id int)", "unlogged_table"},
		{"serial", "CREATE TABLE t (id serial)", "serial"},
		{"bigserial", "CREATE TABLE t (id bigserial)", "serial"},
		{"smallserial", "CREATE TABLE t (id smallserial)", "serial"},
		{"materialized view", "CREATE MATERIALIZED VIEW mv AS SELECT 1", "materialized_view"},
		{"create table as", "CREATE TABLE t AS SELECT 1", "create_table_as"},
		{"enum type", "CREATE TYPE mood AS ENUM ('a')", "enum_type"},
		{"composite type", "CREATE TYPE pair AS (a int, b int)", "composite_type"},
		{"range type", "CREATE TYPE r AS RANGE (subtype = int4)", "range_type"},
		{"domain type", "CREATE DOMAIN d AS int", "domain_type"},
		{"tablespace", "CREATE TABLESPACE ts LOCATION '/tmp/x'", "tablespace"},
		{"foreign table", "CREATE FOREIGN TABLE ft (a int) SERVER s", "foreign_table"},
		{"vacuum", "VACUUM t", "vacuum"},
		{"listen", "LISTEN chan", "listen"},
		{"notify", "NOTIFY chan", "notify"},
		{"unlisten", "UNLISTEN chan", "unlisten"},
		{"alter system", "ALTER SYSTEM SET work_mem = '1MB'", "alter_system"},
		{"create function", "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$", "create_function"},
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
			if result.Verdict.Code != "0A000" {
				t.Fatalf("got SQLSTATE %q want 0A000", result.Verdict.Code)
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
		"CREATE TABLE t (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY)",
		"CREATE TABLE t (id int PRIMARY KEY, parent_id int REFERENCES parent(id))",
		"CREATE TABLE t (id int, parent_id int REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)",
		"ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (id) REFERENCES parent(id)",
		"CREATE SEQUENCE s CACHE 65536",
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
		{"CREATE INDEX idx ON t (a)", []classify.Kind{classify.KindDDL}},
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
		"SET TRANSACTION ISOLATION LEVEL REPEATABLE READ",
		"SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL REPEATABLE READ",
		"SET default_transaction_isolation = 'repeatable read'",
		"SET TRANSACTION READ ONLY",
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
