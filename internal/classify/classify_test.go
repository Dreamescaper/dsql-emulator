package classify_test

import (
	"bytes"
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
			verdict, err := c.Classify(tc.sql)
			if err != nil {
				t.Fatalf("classify %q: %v", tc.sql, err)
			}
			if verdict.RuleID != tc.rule {
				t.Fatalf("got rule %q want %q", verdict.RuleID, tc.rule)
			}
			if verdict.Code != "0A000" {
				t.Fatalf("got SQLSTATE %q want 0A000", verdict.Code)
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
		verdict, err := c.Classify(sql)
		if err != nil {
			t.Fatalf("classify %q: %v", sql, err)
		}
		if verdict.Rejected() {
			t.Fatalf("%q unexpectedly rejected by rule %q", sql, verdict.RuleID)
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
