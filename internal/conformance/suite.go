// Package conformance describes a suite of SQL probes and records how a target
// database answers them. The same suite runs against a real Aurora DSQL cluster
// to produce a golden record and against the emulator to check it.
package conformance

import "strings"

// Suite is a named set of probes plus the schema they need.
type Suite struct {
	Name    string
	Setup   []string
	Cleanup []string
	Cases   []Case
}

// Case is one probe. Steps run in order on a single connection, so a case can
// span an explicit transaction and observe what happens on the step that breaks
// a rule. Everything a step creates must appear in Suite.Cleanup.
type Case struct {
	Name  string `json:"name"`
	Group string `json:"group"`
	// Note records why the case exists or which open question it answers.
	Note string `json:"note,omitempty"`
	// Steps are executed in order; every step is observed.
	Steps []string `json:"steps"`
	// IgnoreRows skips row comparison, for results that cannot match across
	// systems (generated ids, timestamps, version strings).
	IgnoreRows bool `json:"ignore_rows,omitempty"`
	// KnownGap, when set, records an accepted divergence. The case is still
	// recorded and replayed, but its result is not enforced.
	KnownGap string `json:"known_gap,omitempty"`
	// Sessions, when set, gives each entry its own connection. The sessions run
	// concurrently, each step list in order, with a fixed delay between steps.
	Sessions [][]string `json:"sessions,omitempty"`
	// RecordOnly cases are recorded against a real cluster but never replayed
	// against the emulator, because it cannot reproduce them: PostgreSQL blocks
	// on a conflicting write where DSQL is lock-free.
	RecordOnly bool `json:"record_only,omitempty"`
}

func one(sql string) []string { return []string{sql} }

// DefaultSuite is the baseline probe set. It stays small on purpose: every case
// is one request against a metered cluster.
func setupStatements() []string {
	return []string{
		"DROP TABLE IF EXISTS baseline_conflict_child",
		"DROP TABLE IF EXISTS baseline_conflict",
		"DROP TABLE IF EXISTS baseline_child",
		"DROP TABLE IF EXISTS baseline_parent",
		"DROP TABLE IF EXISTS baseline_idx",
		"DROP TABLE IF EXISTS baseline_bulk",
		"DROP TABLE IF EXISTS baseline_drop_me",
		"DROP TABLE IF EXISTS baseline_alter_fk",
		"DROP TABLE IF EXISTS baseline_alter_pk",
		"DROP TABLE IF EXISTS baseline_alter_big",
		"DROP TABLE IF EXISTS baseline_alter",
		"DROP TABLE IF EXISTS baseline_implicit_bulk",
		"CREATE TABLE baseline_parent (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name text NOT NULL)",
		"CREATE TABLE baseline_child (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), parent_id uuid NOT NULL REFERENCES baseline_parent(id))",
		"CREATE TABLE baseline_idx (id uuid PRIMARY KEY, value text)",
		"CREATE TABLE baseline_bulk (id int)",
		"CREATE TABLE baseline_drop_me (id int)",
		"CREATE TABLE baseline_alter (id int PRIMARY KEY, a text, b int)",
		"CREATE TABLE baseline_alter_pk (id int PRIMARY KEY, a text)",
		"CREATE TABLE baseline_alter_big (big bigint, note text)",
		"CREATE TABLE baseline_alter_fk (id int PRIMARY KEY, parent_id uuid)",
		"CREATE TABLE baseline_implicit_bulk (id int)",
		"INSERT INTO baseline_parent (id, name) VALUES ('00000000-0000-0000-0000-0000000000aa', 'seed')",
		"CREATE TABLE baseline_conflict (id uuid PRIMARY KEY, name text NOT NULL)",
		"CREATE TABLE baseline_conflict_child (id uuid PRIMARY KEY, parent_id uuid NOT NULL REFERENCES baseline_conflict(id))",
		"INSERT INTO baseline_conflict (id, name) VALUES ('00000000-0000-0000-0000-0000000000ab', 'conflict-fk')",
		"INSERT INTO baseline_conflict (id, name) VALUES ('00000000-0000-0000-0000-0000000000ac', 'conflict-row')",
		"INSERT INTO baseline_conflict (id, name) VALUES ('00000000-0000-0000-0000-0000000000ad', 'disjoint-a')",
		"INSERT INTO baseline_conflict (id, name) VALUES ('00000000-0000-0000-0000-0000000000ae', 'disjoint-b')",
		"INSERT INTO baseline_conflict (id, name) VALUES ('00000000-0000-0000-0000-0000000000af', 'conflict-nonkey')",
	}
}

func cleanupStatements() []string {
	return []string{
		// Tables that reference another table must go first.
		"DROP TABLE IF EXISTS baseline_alter_fk",
		"DROP TABLE IF EXISTS baseline_alter_pk",
		"DROP TABLE IF EXISTS baseline_alter_big",
		"DROP TABLE IF EXISTS baseline_alter",
		"DROP TABLE IF EXISTS baseline_conflict_child",
		"DROP TABLE IF EXISTS baseline_conflict",
		"DROP TABLE IF EXISTS baseline_child",
		"DROP TABLE IF EXISTS baseline_deferred",
		"DROP TABLE IF EXISTS baseline_parent",
		"DROP TABLE IF EXISTS baseline_idx",
		"DROP TABLE IF EXISTS baseline_bulk",
		"DROP TABLE IF EXISTS baseline_drop_me",
		"DROP TABLE IF EXISTS baseline_implicit_bulk",
		"DROP TABLE IF EXISTS baseline_identity",
		"DROP TABLE IF EXISTS baseline_serial",
		"DROP TABLE IF EXISTS baseline_unlogged",
		"DROP TABLE IF EXISTS baseline_temp",
		"DROP TABLE IF EXISTS baseline_ctas",
		"DROP TABLE IF EXISTS baseline_txn_a",
		"DROP TABLE IF EXISTS baseline_txn_b",
		"DROP TABLE IF EXISTS baseline_txn_c",
		"DROP TABLE IF EXISTS baseline_identity_cached",
		"DROP TABLE IF EXISTS baseline_types_ok",
		"DROP TABLE IF EXISTS baseline_types_alias",
		"DROP TABLE IF EXISTS baseline_enum_col",
		"DROP TABLE IF EXISTS baseline_enum_t",
		"DROP TABLE IF EXISTS baseline_enum_check",
		"DROP TABLE IF EXISTS baseline_enum_check_v",
		"DROP VIEW IF EXISTS baseline_v",
		"DROP INDEX IF EXISTS baseline_idx_value",
		"DROP INDEX IF EXISTS baseline_idx_value_async",
		"DROP SEQUENCE IF EXISTS baseline_seq",
		"DROP SEQUENCE IF EXISTS baseline_seq2",
		"DROP SEQUENCE IF EXISTS baseline_seq_one",
		"DROP FUNCTION IF EXISTS baseline_fn",
		"DROP DOMAIN IF EXISTS baseline_domain",
		"DROP DOMAIN IF EXISTS baseline_mood_domain",
		"DROP SCHEMA IF EXISTS baseline_schema",
	}
}

func DefaultSuite() Suite {
	cases := []Case{
		// Unsupported DDL and statements.
		{Name: "truncate", Group: "unsupported", Steps: one("TRUNCATE TABLE baseline_parent")},
		{Name: "create_extension", Group: "unsupported", Steps: one("CREATE EXTENSION IF NOT EXISTS pgcrypto")},
		{Name: "create_trigger", Group: "unsupported", Steps: one("CREATE TRIGGER baseline_trg AFTER INSERT ON baseline_parent EXECUTE FUNCTION baseline_fn()")},
		{Name: "create_database", Group: "unsupported", Steps: one("CREATE DATABASE baseline_other")},
		{Name: "temporary_table", Group: "unsupported", Steps: one("CREATE TEMPORARY TABLE baseline_temp (id int)")},
		{Name: "unlogged_table", Group: "unsupported", Steps: one("CREATE UNLOGGED TABLE baseline_unlogged (id int)")},
		{Name: "serial_column", Group: "unsupported", Steps: one("CREATE TABLE baseline_serial (id serial PRIMARY KEY)")},
		{Name: "materialized_view", Group: "unsupported", Steps: one("CREATE MATERIALIZED VIEW baseline_mv AS SELECT 1 AS x")},
		{Name: "create_table_as", Group: "unsupported", Note: "confirm CTAS is unsupported", Steps: one("CREATE TABLE baseline_ctas AS SELECT 1 AS x")},
		{Name: "enum_type", Group: "unsupported", Steps: one("CREATE TYPE baseline_mood AS ENUM ('sad', 'ok')")},
		{Name: "composite_type", Group: "unsupported", Steps: one("CREATE TYPE baseline_pair AS (a int, b int)")},
		{Name: "range_type", Group: "unsupported", Steps: one("CREATE TYPE baseline_range AS RANGE (subtype = int4)")},
		{Name: "domain_type", Group: "unsupported", Steps: one("CREATE DOMAIN baseline_domain AS int")},
		{Name: "tablespace", Group: "unsupported", Steps: one("CREATE TABLESPACE baseline_ts LOCATION '/tmp/baseline'")},
		{Name: "foreign_table", Group: "unsupported", Steps: one("CREATE FOREIGN TABLE baseline_ft (id int) SERVER baseline_srv")},
		{Name: "vacuum", Group: "unsupported", Steps: one("VACUUM baseline_parent")},
		{Name: "listen", Group: "unsupported", Steps: one("LISTEN baseline_chan")},
		{Name: "notify", Group: "unsupported", Steps: one("NOTIFY baseline_chan")},
		{Name: "unlisten", Group: "unsupported", Steps: one("UNLISTEN baseline_chan")},
		{Name: "alter_system", Group: "unsupported", Steps: one("ALTER SYSTEM SET work_mem = '4MB'")},
		{Name: "create_function", Group: "unsupported", Note: "is LANGUAGE sql rejected too, or only plpgsql?", Steps: one("CREATE FUNCTION baseline_fn() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$")},

		// Open questions from the ruleset verification backlog.
		{Name: "savepoint", Group: "backlog", Note: "savepoints: currently not rejected by the ruleset", Steps: one("SAVEPOINT baseline_sp")},
		{Name: "create_view", Group: "backlog", Note: "views: currently not rejected by the ruleset", Steps: one("CREATE VIEW baseline_v AS SELECT 1 AS x")},
		{Name: "create_sequence", Group: "backlog", Note: "sequences: currently not rejected by the ruleset", Steps: one("CREATE SEQUENCE baseline_seq")},
		{Name: "create_sequence_cached", Group: "backlog", Note: "sequences with high CACHE may be allowed", Steps: one("CREATE SEQUENCE baseline_seq2 CACHE 65536")},
		{Name: "create_schema", Group: "backlog", Steps: one("CREATE SCHEMA baseline_schema")},

		// Isolation level.
		{Name: "set_transaction_serializable", Group: "isolation", Steps: one("SET TRANSACTION ISOLATION LEVEL SERIALIZABLE")},
		{Name: "begin_serializable", Group: "isolation", Steps: one("BEGIN ISOLATION LEVEL SERIALIZABLE")},
		{Name: "begin_repeatable_read", Group: "isolation", Steps: []string{"BEGIN ISOLATION LEVEL REPEATABLE READ", "SELECT 1", "COMMIT"}},

		// Transaction rules.
		{Name: "two_ddl_one_txn", Group: "transaction", Steps: []string{"BEGIN", "CREATE TABLE baseline_txn_a (id int)", "CREATE TABLE baseline_txn_b (id int)"}},
		{Name: "ddl_then_dml", Group: "transaction", Steps: []string{"BEGIN", "CREATE TABLE baseline_txn_c (id int)", "INSERT INTO baseline_txn_c (id) VALUES (1)"}},
		{Name: "row_cap", Group: "transaction", Note: "DSQL fails the statement that crosses the cap and aborts the transaction", Steps: []string{"BEGIN", "INSERT INTO baseline_bulk SELECT generate_series(1, 3001)", "SELECT 1"}},
		{Name: "commit", Group: "transaction", Steps: []string{"BEGIN", "INSERT INTO baseline_parent (id, name) VALUES ('00000000-0000-0000-0000-0000000000bb', 'tx')", "COMMIT"}},
		{Name: "rollback", Group: "transaction", Steps: []string{"BEGIN", "INSERT INTO baseline_parent (id, name) VALUES ('00000000-0000-0000-0000-0000000000cc', 'tx')", "ROLLBACK", "SELECT count(*) FROM baseline_parent WHERE name = 'tx'"}},
		{Name: "read_only", Group: "transaction", Note: "DSQL refuses SET TRANSACTION and then the transaction is aborted", Steps: []string{"BEGIN", "SET TRANSACTION READ ONLY", "SELECT 1", "COMMIT"}},

		// Aborted-transaction behavior: a refusal fails the whole
		// transaction, later statements report 25P02, and only ROLLBACK (or
		// a COMMIT that reports ROLLBACK) ends it.
		{Name: "rejection_aborts_txn", Group: "transaction", Note: "a dialect refusal fails the transaction", Steps: []string{"BEGIN", "TRUNCATE baseline_parent", "SELECT 1", "ROLLBACK"}},
		{Name: "aborted_txn_prefers_25P02", Group: "transaction", Note: "in a failed transaction an unsupported statement reports 25P02 first", Steps: []string{"BEGIN", "TRUNCATE baseline_parent", "TRUNCATE baseline_parent", "ROLLBACK"}},
		{Name: "aborted_txn_recovers_after_rollback", Group: "transaction", Note: "ROLLBACK ends a failed transaction and the session is usable", Steps: []string{"BEGIN", "SET TRANSACTION READ ONLY", "ROLLBACK", "SELECT 1"}},
		{Name: "rejection_outside_txn_does_not_abort", Group: "transaction", Note: "an implicit transaction is not failed by a refusal", Steps: []string{"TRUNCATE baseline_parent", "SELECT 1"}},
		{Name: "row_cap_boundary", Group: "transaction", Note: "confirm exactly 3000 rows is allowed", Steps: []string{"BEGIN", "INSERT INTO baseline_bulk SELECT generate_series(1, 3000)", "SELECT 1", "ROLLBACK"}},
		{Name: "row_cap_discards_rows", Group: "transaction", Note: "confirm a failed row-cap transaction leaves no rows", Steps: []string{"BEGIN", "INSERT INTO baseline_bulk SELECT generate_series(1, 3001)", "ROLLBACK", "SELECT count(*) FROM baseline_bulk"}},
		{Name: "row_cap_implicit", Group: "transaction", Note: "an autocommit statement that crosses the cap", Steps: one("INSERT INTO baseline_implicit_bulk SELECT generate_series(1, 3001)")},

		// Supported behavior worth pinning.
		{Name: "select_one", Group: "supported", Steps: one("SELECT 1")},
		{Name: "show_transaction_isolation", Group: "supported", Steps: one("SHOW transaction_isolation")},
		{Name: "select_version", Group: "supported", Note: "record the server_version string", Steps: one("SELECT version()"), IgnoreRows: true},
		{Name: "insert_returning", Group: "supported", Steps: one("INSERT INTO baseline_idx (id, value) VALUES ('00000000-0000-0000-0000-0000000000dd', 'v1') ON CONFLICT (id) DO NOTHING RETURNING value")},
		{Name: "select_count", Group: "supported", Steps: one("SELECT count(*) FROM baseline_idx")},
		{Name: "update_row", Group: "supported", Steps: one("UPDATE baseline_idx SET value = 'v2' WHERE id = '00000000-0000-0000-0000-0000000000dd'")},
		{Name: "delete_row", Group: "supported", Steps: one("DELETE FROM baseline_idx WHERE id = '00000000-0000-0000-0000-0000000000dd'")},
		{Name: "cte", Group: "supported", Steps: one("WITH x AS (SELECT 1 AS n) SELECT n FROM x")},
		{Name: "join", Group: "supported", Steps: one("SELECT p.name FROM baseline_parent p JOIN baseline_child c ON c.parent_id = p.id")},
		{Name: "foreign_key_insert", Group: "supported", Steps: one("INSERT INTO baseline_child (id, parent_id) VALUES ('00000000-0000-0000-0000-0000000000ee', '00000000-0000-0000-0000-0000000000aa')")},
		{Name: "foreign_key_violation", Group: "supported", Steps: one("INSERT INTO baseline_child (id, parent_id) VALUES ('00000000-0000-0000-0000-0000000000ff', '00000000-0000-0000-0000-0000000000ff')")},
		{Name: "identity_column", Group: "supported", Steps: one("CREATE TABLE baseline_identity (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, value text)")},
		{Name: "deferrable_foreign_key", Group: "supported", Steps: one("CREATE TABLE baseline_deferred (id int PRIMARY KEY, parent_id uuid REFERENCES baseline_parent(id) DEFERRABLE INITIALLY DEFERRED)")},
		{Name: "alter_table_add_column", Group: "supported", Steps: one("ALTER TABLE baseline_idx ADD COLUMN extra text")},
		{Name: "drop_table", Group: "supported", Steps: one("DROP TABLE baseline_drop_me")},

		// Index dialect: DSQL requires ASYNC.
		{Name: "create_index_sync", Group: "index", Note: "DSQL requires ASYNC; sync should be refused", Steps: one("CREATE INDEX baseline_idx_value ON baseline_idx (value)")},
		{Name: "create_index_async", Group: "index", Note: "returns a generated job_id", Steps: one("CREATE INDEX ASYNC IF NOT EXISTS baseline_idx_value_async ON baseline_idx (value)"), IgnoreRows: true},
		{Name: "create_index_async_partial", Group: "index", Note: "partial index predicate", Steps: one("CREATE INDEX ASYNC baseline_partial_idx ON baseline_idx (value) WHERE value IS NOT NULL"), IgnoreRows: true},
		{Name: "create_index_async_unique_partial", Group: "index", Steps: one("CREATE UNIQUE INDEX ASYNC baseline_upartial_idx ON baseline_idx (value) WHERE value <> ''"), IgnoreRows: true},
		{Name: "create_index_async_expression", Group: "index", Steps: one("CREATE INDEX ASYNC baseline_expr_idx ON baseline_idx ((lower(value)))"), IgnoreRows: true},
		{Name: "create_index_async_include", Group: "index", Steps: one("CREATE INDEX ASYNC baseline_include_idx ON baseline_idx (value) INCLUDE (id)"), IgnoreRows: true},
		{Name: "create_index_async_nulls_not_distinct", Group: "index", Steps: one("CREATE UNIQUE INDEX ASYNC baseline_nnd_idx ON baseline_idx (value) NULLS NOT DISTINCT"), IgnoreRows: true},
		{Name: "create_index_async_unnamed", Group: "index", Note: "the server chooses the name, so no id can be derived from it", Steps: one("CREATE INDEX ASYNC ON baseline_idx (value)"), IgnoreRows: true},
		{Name: "create_index_async_qualified", Group: "index", Note: "the documentation says a schema-qualified name is not allowed", Steps: one("CREATE INDEX ASYNC public.baseline_qualified_idx ON baseline_idx (value)")},
		{Name: "sys_jobs", Group: "index", Steps: one("SELECT count(*) AS n FROM sys.jobs"), IgnoreRows: true},
		{Name: "sys_jobs_columns", Group: "index", Note: "pin the real sys.jobs shape; limited because the cluster's job log only grows", Steps: one("SELECT * FROM sys.jobs LIMIT 1"), IgnoreRows: true},
		{Name: "sys_wait_for_job", Group: "index", Note: "pin the real wait_for_job contract", Steps: one("SELECT sys.wait_for_job((SELECT job_id FROM sys.jobs LIMIT 1))")},
		{Name: "sys_call_wait_for_job", Group: "index", Note: "wait_for_job is a procedure on DSQL", Steps: one("CALL sys.wait_for_job((SELECT job_id FROM sys.jobs LIMIT 1))")},
		{Name: "sys_call_wait_for_job_literal", Group: "index", Note: "the CALL path with a literal argument", Steps: one("CALL sys.wait_for_job('no-such-job')")},

		// Not yet recorded; these answer the remaining backlog questions on
		// the next baseline run.
		{Name: "create_function_plpgsql", Group: "backlog", Note: "confirm non-sql function languages are refused", Steps: one("CREATE FUNCTION baseline_plpgsql() RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETURN 1; END $$")},
		{Name: "identity_column_cached", Group: "backlog", Note: "confirm identity with a large CACHE is allowed", Steps: one("CREATE TABLE baseline_identity_cached (id bigint GENERATED ALWAYS AS IDENTITY (CACHE 65536) PRIMARY KEY, value text)")},
		{Name: "create_sequence_cache_one", Group: "backlog", Note: "confirm CACHE 1 is allowed", Steps: one("CREATE SEQUENCE baseline_seq_one CACHE 1")},
		{Name: "rollback_to_savepoint", Group: "backlog", Steps: one("ROLLBACK TO SAVEPOINT baseline_sp")},
		{Name: "set_transaction_repeatable_read", Group: "backlog", Note: "confirm SET TRANSACTION is refused even for the supported level", Steps: one("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ")},
		{Name: "set_default_isolation", Group: "backlog", Note: "confirm SET default_transaction_isolation is refused", Steps: one("SET default_transaction_isolation = 'repeatable read'")},
	}

	cases = append(cases, supportedTypeCases()...)
	cases = append(cases, unsupportedTypeCases()...)
	cases = append(cases, enumCases()...)
	cases = append(cases, environmentCases()...)
	cases = append(cases, occCases()...)
	cases = append(cases, queryCases()...)
	cases = append(cases, alterCases()...)
	cases = append(cases, occConflictCases()...)

	return Suite{
		Name:    "dsql-baseline",
		Setup:   setupStatements(),
		Cleanup: cleanupStatements(),
		Cases:   cases,
	}
}

// supportedTypeCases create a column of every data type the Aurora DSQL docs
// list as supported, exercising aliases, precision, and a round trip.
func supportedTypeCases() []Case {
	return []Case{
		{Name: "types_supported_columns", Group: "types", Note: "every documented supported type as a column", Steps: one(`CREATE TABLE baseline_types_ok (
			c_smallint smallint,
			c_integer integer,
			c_bigint bigint,
			c_real real,
			c_double double precision,
			c_numeric numeric,
			c_numeric_ps numeric(18,6),
			c_decimal decimal(10,2),
			c_char character(10),
			c_varchar character varying(20),
			c_bpchar bpchar(5),
			c_text text,
			c_date date,
			c_time time,
			c_timetz time with time zone,
			c_timestamp timestamp,
			c_timestamptz timestamp with time zone,
			c_interval interval,
			c_boolean boolean,
			c_bytea bytea,
			c_uuid uuid,
			c_json json,
			c_jsonb jsonb)`)},
		{Name: "types_alias_columns", Group: "types", Note: "documented aliases as column types", Steps: one(`CREATE TABLE baseline_types_alias (
			c_int2 int2, c_int4 int4, c_int8 int8,
			c_float4 float4, c_float8 float8,
			c_bool bool, c_varchar varchar(5), c_char char(5),
			c_dec dec, c_timetz timetz, c_timestamptz timestamptz)`)},
		{Name: "types_roundtrip", Group: "types", Note: "a row through every supported type", Steps: []string{
			`INSERT INTO baseline_types_ok
			 (c_smallint, c_integer, c_bigint, c_real, c_double, c_numeric, c_numeric_ps,
			  c_decimal, c_char, c_varchar, c_bpchar, c_text, c_date, c_time, c_timetz,
			  c_timestamp, c_timestamptz, c_interval, c_boolean, c_bytea, c_uuid, c_json, c_jsonb)
			 VALUES (1, 2, 3, 1.5, 2.5, 3.5, 4.5, 5.5, 'abc', 'def', 'ghi', 'jkl',
			  '2026-01-02', '03:04:05', '03:04:05+00', '2026-01-02 03:04:05',
			  '2026-01-02 03:04:05+00', '1 day', true, '\x6869',
			  '00000000-0000-0000-0000-000000000001', '{"a":1}', '{"b":2}')`,
			"SELECT count(*) FROM baseline_types_ok",
		}},
		{Name: "types_runtime_array", Group: "types", Note: "arrays are documented as query-runtime only", Steps: one("SELECT string_to_array('1,2,3', ',')")},
		{Name: "types_runtime_inet", Group: "types", Note: "inet is documented as query-runtime only", Steps: one("SELECT '127.0.0.1'::inet")},
		{Name: "types_runtime_json_ops", Group: "types", Steps: one(`SELECT '{"a":1}'::jsonb ->> 'a'`)},
	}
}

// unsupportedTypeCases try each data type that is not on the supported list.
// Every case drops the table it may have created, so a surprise acceptance
// still cleans up after itself.
func unsupportedTypeCases() []Case {
	types := []string{
		"money", "xml", "inet", "cidr", "macaddr", "point", "line", "lseg", "box",
		"path", "polygon", "circle", "tsvector", "tsquery", "int4range",
		"tsrange", "bit(8)", "varbit(8)", "pg_lsn", "int[]", "text[]",
	}

	cases := make([]Case, 0, len(types))
	for _, typ := range types {
		table := "baseline_type_" + typeSlug(typ)
		cases = append(cases, Case{
			Name:  "type_unsupported_" + typeSlug(typ),
			Group: "types",
			Note:  "unsupported column type " + typ,
			Steps: []string{
				"CREATE TABLE " + table + " (v " + typ + ")",
				"DROP TABLE IF EXISTS " + table,
			},
		})
	}
	return cases
}

// enumCases cover enum types, which DSQL does not provide, and the text plus
// CHECK and domain patterns applications use instead.
func enumCases() []Case {
	return []Case{
		{Name: "enum_create_type", Group: "enum", Note: "CREATE TYPE ... AS ENUM", Steps: one("CREATE TYPE baseline_mood AS ENUM ('sad', 'ok')")},
		{Name: "enum_alter_add_value", Group: "enum", Steps: one("ALTER TYPE baseline_mood ADD VALUE 'meh'")},
		{Name: "enum_alter_rename", Group: "enum", Steps: one("ALTER TYPE baseline_mood RENAME TO baseline_mood_renamed")},
		{Name: "enum_drop_if_exists", Group: "enum", Steps: one("DROP TYPE IF EXISTS baseline_mood")},
		{Name: "enum_column_undefined", Group: "enum", Note: "a column of a type that cannot be created", Steps: one("CREATE TABLE baseline_enum_col (m baseline_mood)")},
		{Name: "enum_cast_undefined", Group: "enum", Note: "a cast to a type that cannot be created", Steps: one("SELECT 'sad'::baseline_mood")},
		{Name: "enum_domain_create", Group: "enum", Note: "domain as the enum workaround", Steps: one("CREATE DOMAIN baseline_mood_domain AS text CHECK (VALUE IN ('sad', 'ok'))")},
		{Name: "enum_domain_violation", Group: "enum", Note: "domain CHECK rejects an unknown label", Steps: []string{
			"CREATE TABLE baseline_enum_t (id int, m baseline_mood_domain)",
			"INSERT INTO baseline_enum_t (id, m) VALUES (1, 'bogus')",
		}},
		{Name: "enum_check_create", Group: "enum", Note: "CHECK constraint as the enum workaround", Steps: one("CREATE TABLE baseline_enum_check (id int, m text CHECK (m IN ('sad', 'ok')))")},
		{Name: "enum_check_violation", Group: "enum", Note: "CHECK constraint rejects an unknown label", Steps: []string{
			"CREATE TABLE baseline_enum_check_v (id int, m text CHECK (m IN ('sad', 'ok')))",
			"INSERT INTO baseline_enum_check_v (id, m) VALUES (1, 'bogus')",
		}},
	}
}

// environmentCases pin what the server reports about itself: version,
// database, timezone, and collation.
func environmentCases() []Case {
	return []Case{
		{Name: "env_server_version", Group: "environment", Steps: one("SHOW server_version")},
		{Name: "env_version_function", Group: "environment", Steps: one("SELECT version()"), IgnoreRows: true},
		{Name: "env_server_version_setting", Group: "environment", Steps: one("SELECT current_setting('server_version')")},
		{Name: "env_server_version_num", Group: "environment", Steps: one("SELECT current_setting('server_version_num')")},
		{Name: "env_show_server_version_num", Group: "environment", Steps: one("SHOW server_version_num")},
		{Name: "env_current_database", Group: "environment", Steps: one("SELECT current_database()")},
		{Name: "env_current_schema", Group: "environment", Steps: one("SELECT current_schema()")},
		{Name: "env_timezone", Group: "environment", Steps: one("SHOW timezone")},
		{Name: "env_client_encoding", Group: "environment", Steps: one("SHOW client_encoding")},
		{Name: "env_lc_collate", Group: "environment", Steps: one("SHOW lc_collate")},
	}
}

// occCases cover the row-locking clauses DSQL uses for conflict detection.
// True two-session conflicts are not represented here: the suite runs one
// connection, and PostgreSQL blocks where DSQL is lock-free.
func occCases() []Case {
	return []Case{
		{Name: "occ_for_update", Group: "occ", Steps: one("SELECT id FROM baseline_parent WHERE id = '00000000-0000-0000-0000-0000000000aa' FOR UPDATE")},
		{Name: "occ_for_key_share", Group: "occ", Steps: one("SELECT id FROM baseline_parent WHERE id = '00000000-0000-0000-0000-0000000000aa' FOR KEY SHARE")},
		{Name: "occ_for_no_key_update", Group: "occ", Note: "documented as unsupported", Steps: one("SELECT id FROM baseline_parent WHERE id = '00000000-0000-0000-0000-0000000000aa' FOR NO KEY UPDATE")},
		{Name: "occ_for_share", Group: "occ", Note: "documented as unsupported", Steps: one("SELECT id FROM baseline_parent WHERE id = '00000000-0000-0000-0000-0000000000aa' FOR SHARE")},
	}
}

// queryCases cover SELECT features, common query patterns, and a handful of
// patterns and operators DSQL is expected to refuse.
func queryCases() []Case {
	return []Case{
		// Supported clauses.
		{Name: "q_where", Group: "queries", Steps: one("SELECT name FROM baseline_parent WHERE name = 'seed'")},
		{Name: "q_distinct", Group: "queries", Steps: one("SELECT DISTINCT name FROM baseline_parent ORDER BY name")},
		{Name: "q_group_by", Group: "queries", Steps: one("SELECT name, count(*) FROM baseline_parent GROUP BY name ORDER BY name")},
		{Name: "q_group_by_all", Group: "queries", Note: "documented DSQL extension", Steps: one("SELECT name, count(*) FROM baseline_parent GROUP BY ALL")},
		{Name: "q_group_by_distinct", Group: "queries", Steps: one("SELECT name, count(*) FROM baseline_parent GROUP BY DISTINCT name ORDER BY name")},
		{Name: "q_having", Group: "queries", Steps: one("SELECT name, count(*) FROM baseline_parent GROUP BY name HAVING count(*) >= 0 ORDER BY name")},
		{Name: "q_order_nulls", Group: "queries", Steps: one("SELECT name FROM baseline_parent ORDER BY name ASC NULLS FIRST")},
		{Name: "q_order_desc_nulls_last", Group: "queries", Steps: one("SELECT name FROM baseline_parent ORDER BY name DESC NULLS LAST")},
		{Name: "q_limit", Group: "queries", Steps: one("SELECT name FROM baseline_parent LIMIT 1")},
		{Name: "q_window_rank", Group: "queries", Steps: one("SELECT name, RANK() OVER (PARTITION BY name ORDER BY name) FROM baseline_parent")},

		// Joins and set operations.
		{Name: "q_inner_join", Group: "queries", Steps: one("SELECT p.name FROM baseline_parent p INNER JOIN baseline_child c ON c.parent_id = p.id")},
		{Name: "q_left_join", Group: "queries", Steps: one("SELECT p.name FROM baseline_parent p LEFT JOIN baseline_child c ON c.parent_id = p.id ORDER BY 1")},
		{Name: "q_right_join", Group: "queries", Steps: one("SELECT p.name FROM baseline_parent p RIGHT JOIN baseline_child c ON c.parent_id = p.id")},
		{Name: "q_full_join", Group: "queries", Steps: one("SELECT p.name FROM baseline_parent p FULL JOIN baseline_child c ON c.parent_id = p.id ORDER BY 1")},
		{Name: "q_cross_join", Group: "queries", Steps: one("SELECT p.name FROM baseline_parent p CROSS JOIN baseline_child c ORDER BY 1")},
		{Name: "q_union", Group: "queries", Steps: one("SELECT name FROM baseline_parent UNION SELECT name FROM baseline_parent ORDER BY 1")},
		{Name: "q_union_all", Group: "queries", Steps: one("SELECT name FROM baseline_parent UNION ALL SELECT name FROM baseline_parent ORDER BY 1")},
		{Name: "q_intersect", Group: "queries", Steps: one("SELECT name FROM baseline_parent INTERSECT SELECT name FROM baseline_parent ORDER BY 1")},
		{Name: "q_except", Group: "queries", Steps: one("SELECT name FROM baseline_parent EXCEPT SELECT name FROM baseline_parent ORDER BY 1")},

		// Common patterns not called out by the documentation.
		{Name: "q_cte", Group: "queries", Steps: one("WITH x AS (SELECT name FROM baseline_parent) SELECT name FROM x")},
		{Name: "q_cte_multiple", Group: "queries", Steps: one("WITH a AS (SELECT 1 AS n), b AS (SELECT n FROM a) SELECT n FROM b")},
		{Name: "q_subquery_scalar", Group: "queries", Steps: one("SELECT (SELECT count(*) FROM baseline_parent)")},
		{Name: "q_subquery_in", Group: "queries", Steps: one("SELECT name FROM baseline_parent WHERE id IN (SELECT parent_id FROM baseline_child)")},
		{Name: "q_subquery_exists", Group: "queries", Steps: one("SELECT p.name FROM baseline_parent p WHERE EXISTS (SELECT 1 FROM baseline_child c WHERE c.parent_id = p.id)")},
		{Name: "q_subquery_correlated", Group: "queries", Steps: one("SELECT p.name FROM baseline_parent p WHERE p.name = (SELECT q.name FROM baseline_parent q WHERE q.id = p.id) ORDER BY 1")},
		{Name: "q_unnest", Group: "queries", Steps: one("SELECT unnest(string_to_array('a,b,c', ',')) ORDER BY 1")},
		{Name: "q_array_runtime", Group: "queries", Steps: one("SELECT string_to_array('1,2,3', ',')")},
		{Name: "q_generate_series", Group: "queries", Steps: one("SELECT * FROM generate_series(1, 3) ORDER BY 1")},
		{Name: "q_case", Group: "queries", Steps: one("SELECT CASE WHEN 1 = 1 THEN 'yes' ELSE 'no' END")},
		{Name: "q_coalesce_nullif", Group: "queries", Steps: one("SELECT coalesce(NULL, 'x'), nullif(1, 2)")},
		{Name: "q_cast", Group: "queries", Steps: one("SELECT '1'::int + 1, 1::text")},
		{Name: "q_string_ops", Group: "queries", Steps: one("SELECT 'a' || 'b', upper('a'), lower('A'), length('abc')")},
		{Name: "q_aggregates", Group: "queries", Steps: one("SELECT count(*), count(DISTINCT name), min(name), max(name) FROM baseline_parent")},
		{Name: "q_between", Group: "queries", Steps: one("SELECT 1 WHERE 2 BETWEEN 1 AND 3")},
		{Name: "q_is_distinct_from", Group: "queries", Steps: one("SELECT 1 IS DISTINCT FROM 2")},

		// DML shapes.
		{Name: "q_insert_returning", Group: "queries", Steps: one("INSERT INTO baseline_idx (id, value) VALUES ('00000000-0000-0000-0000-0000000000a1', 'v1') RETURNING value")},
		{Name: "q_upsert", Group: "queries", Steps: one("INSERT INTO baseline_idx (id, value) VALUES ('00000000-0000-0000-0000-0000000000a1', 'v2') ON CONFLICT (id) DO UPDATE SET value = EXCLUDED.value")},
		{Name: "q_insert_select", Group: "queries", Steps: one("INSERT INTO baseline_bulk SELECT generate_series(1, 3)")},
		{Name: "q_update_returning", Group: "queries", Steps: one("UPDATE baseline_idx SET value = 'v3' WHERE id = '00000000-0000-0000-0000-0000000000a1' RETURNING value")},
		{Name: "q_delete_returning", Group: "queries", Steps: one("DELETE FROM baseline_idx WHERE id = '00000000-0000-0000-0000-0000000000a1' RETURNING value")},
		{Name: "q_delete_using", Group: "queries", Steps: one("DELETE FROM baseline_bulk USING baseline_parent WHERE baseline_bulk.id = 1")},

		// Utility commands.
		{Name: "q_explain", Group: "queries", Steps: one("EXPLAIN SELECT 1")},
		{Name: "q_analyze", Group: "queries", Steps: one("ANALYZE baseline_parent")},
		{Name: "q_set_constraints", Group: "queries", Steps: []string{"BEGIN", "SET CONSTRAINTS ALL DEFERRED", "COMMIT"}},

		// Refused: functions on types DSQL does not have.
		{Name: "q_fulltext_to_tsvector", Group: "queries", Steps: one("SELECT to_tsvector('english', 'hello world')")},
		{Name: "q_fulltext_to_tsquery", Group: "queries", Steps: one("SELECT to_tsquery('english', 'hello & world')")},
		{Name: "q_fulltext_websearch", Group: "queries", Steps: one("SELECT websearch_to_tsquery('english', 'hello world')")},
		{Name: "q_fulltext_match", Group: "queries", Steps: one("SELECT 1 WHERE to_tsvector('english', 'a') @@ to_tsquery('english', 'a')")},
		{Name: "q_geom_line", Group: "queries", Steps: one("SELECT line('{1,2,3}')")},
		{Name: "q_geom_circle", Group: "queries", Steps: one("SELECT circle('<(0,0),1>'::text)")},

		// Refused: constructs the documentation does not list.
		{Name: "q_grouping_sets", Group: "queries", Steps: one("SELECT count(*) FROM baseline_parent GROUP BY GROUPING SETS ((name), ()) ORDER BY 1")},
		{Name: "q_rollup", Group: "queries", Steps: one("SELECT count(*) FROM baseline_parent GROUP BY ROLLUP (name) ORDER BY 1")},
		{Name: "q_cube", Group: "queries", Steps: one("SELECT count(*) FROM baseline_parent GROUP BY CUBE (name) ORDER BY 1")},
		{Name: "q_lateral", Group: "queries", Steps: one("SELECT p.name FROM baseline_parent p CROSS JOIN LATERAL (SELECT count(*) FROM baseline_child c WHERE c.parent_id = p.id) AS x ORDER BY 1")},
		{Name: "q_tablesample", Group: "queries", Steps: one("SELECT name FROM baseline_parent TABLESAMPLE SYSTEM (1)")},
		{Name: "q_distinct_on", Group: "queries", Steps: one("SELECT DISTINCT ON (name) name FROM baseline_parent ORDER BY name")},
		{Name: "q_window_row_number", Group: "queries", Steps: one("SELECT ROW_NUMBER() OVER (ORDER BY name) FROM baseline_parent ORDER BY 1")},
		{Name: "q_window_lag", Group: "queries", Steps: one("SELECT LAG(name) OVER (ORDER BY name) FROM baseline_parent ORDER BY 1")},
		{Name: "q_with_recursive", Group: "queries", Steps: one("WITH RECURSIVE t(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM t WHERE n < 3) SELECT n FROM t ORDER BY 1")},
		{Name: "q_aggregate_filter", Group: "queries", Steps: one("SELECT count(*) FILTER (WHERE name = 'seed') FROM baseline_parent")},
		{Name: "q_merge", Group: "queries", Steps: one("MERGE INTO baseline_idx t USING baseline_parent s ON t.id = '00000000-0000-0000-0000-0000000000a1' WHEN MATCHED THEN UPDATE SET value = 'x'")},
	}
}

// occConflictCases cover concurrency. The conflicting ones are record-only:
// DSQL adjudicates at commit and is lock-free, while PostgreSQL blocks before
// failing, so the emulator cannot reproduce them and a replay would hang.
func occConflictCases() []Case {
	return []Case{
		{Name: "occ_write_write", Group: "occ_conflict", RecordOnly: true,
			Note: "two writers to one row: the loser fails at commit",
			Sessions: [][]string{
				{"BEGIN", "UPDATE baseline_conflict SET name = 'ww-a' WHERE id = '00000000-0000-0000-0000-0000000000ac'", "COMMIT"},
				{"BEGIN", "UPDATE baseline_conflict SET name = 'ww-b' WHERE id = '00000000-0000-0000-0000-0000000000ac'", "COMMIT"},
			}},
		{Name: "occ_for_update_vs_write", Group: "occ_conflict", RecordOnly: true,
			Note: "FOR UPDATE versus a write",
			Sessions: [][]string{
				{"BEGIN", "SELECT name FROM baseline_conflict WHERE id = '00000000-0000-0000-0000-0000000000ac' FOR UPDATE", "COMMIT"},
				{"BEGIN", "UPDATE baseline_conflict SET name = 'fu-b' WHERE id = '00000000-0000-0000-0000-0000000000ac'", "COMMIT"},
			}},
		{Name: "occ_for_key_share_vs_delete", Group: "occ_conflict", RecordOnly: true,
			Note: "FOR KEY SHARE versus deleting the key",
			Sessions: [][]string{
				{"BEGIN", "SELECT name FROM baseline_conflict WHERE id = '00000000-0000-0000-0000-0000000000ac' FOR KEY SHARE", "COMMIT"},
				{"BEGIN", "DELETE FROM baseline_conflict WHERE id = '00000000-0000-0000-0000-0000000000ac'", "COMMIT"},
			}},
		{Name: "occ_fk_delete_insert", Group: "occ_conflict", RecordOnly: true,
			Note: "delete a referenced row while another session inserts a referencing row",
			Sessions: [][]string{
				{"BEGIN", "DELETE FROM baseline_conflict WHERE id = '00000000-0000-0000-0000-0000000000ab'", "COMMIT"},
				{"BEGIN", "INSERT INTO baseline_conflict_child (id, parent_id) VALUES ('00000000-0000-0000-0000-0000000000b0', '00000000-0000-0000-0000-0000000000ab')", "COMMIT"},
			}},
		{Name: "occ_fk_nonkey_update", Group: "occ_conflict",
			Note: "a non-key update does not conflict with a referencing insert",
			Sessions: [][]string{
				{"BEGIN", "UPDATE baseline_conflict SET name = 'nk' WHERE id = '00000000-0000-0000-0000-0000000000af'", "COMMIT"},
				{"BEGIN", "INSERT INTO baseline_conflict_child (id, parent_id) VALUES ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000af')", "COMMIT"},
			}},
		{Name: "occ_disjoint_writes", Group: "occ_conflict",
			Note: "writes to different rows do not conflict",
			Sessions: [][]string{
				{"BEGIN", "UPDATE baseline_conflict SET name = 'dw-a' WHERE id = '00000000-0000-0000-0000-0000000000ad'", "COMMIT"},
				{"BEGIN", "UPDATE baseline_conflict SET name = 'dw-b' WHERE id = '00000000-0000-0000-0000-0000000000ae'", "COMMIT"},
			}},
	}
}

// alterCases cover the ALTER TABLE forms Aurora DSQL supports, including the
// restrictions it adds: a primary key column cannot be dropped, a CHECK or
// FOREIGN KEY added by ALTER TABLE must use NOT VALID, and validation runs
// through the ASYNC form.
func alterCases() []Case {
	return []Case{
		{Name: "alter_drop_column", Group: "alters", Steps: one("ALTER TABLE baseline_alter DROP COLUMN b")},
		{Name: "alter_drop_pk_column", Group: "alters", Note: "DSQL does not support dropping a primary key column; PostgreSQL does", Steps: one("ALTER TABLE baseline_alter_pk DROP COLUMN id"), KnownGap: "deciding whether a column is part of the primary key needs catalog knowledge the proxy does not keep"},
		{Name: "alter_add_column", Group: "alters", Steps: one("ALTER TABLE baseline_alter ADD COLUMN c text")},
		{Name: "alter_add_column_storage", Group: "alters", Note: "compression is controlled with STORAGE", Steps: one("ALTER TABLE baseline_alter ADD COLUMN d text STORAGE PLAIN")},
		{Name: "alter_set_storage", Group: "alters", Steps: one("ALTER TABLE baseline_alter ALTER COLUMN a SET STORAGE PLAIN")},
		{Name: "alter_add_fk_not_valid", Group: "alters", Steps: one("ALTER TABLE baseline_alter_fk ADD CONSTRAINT alter_fk FOREIGN KEY (parent_id) REFERENCES baseline_parent(id) NOT VALID")},
		{Name: "alter_add_fk_without_not_valid", Group: "alters", Note: "DSQL requires NOT VALID; PostgreSQL allows it", Steps: one("ALTER TABLE baseline_alter_fk ADD CONSTRAINT alter_fk2 FOREIGN KEY (parent_id) REFERENCES baseline_parent(id)")},
		{Name: "alter_add_check_not_valid", Group: "alters", Steps: one("ALTER TABLE baseline_alter ADD CONSTRAINT alter_chk CHECK (a IS NOT NULL) NOT VALID")},
		{Name: "alter_add_check_without_not_valid", Group: "alters", Note: "DSQL requires NOT VALID; PostgreSQL allows it", Steps: one("ALTER TABLE baseline_alter ADD CONSTRAINT alter_chk2 CHECK (a IS NOT NULL)")},
		{Name: "alter_validate_constraint_async", Group: "alters", Note: "the asynchronous form that returns a job", Steps: one("ALTER TABLE ASYNC baseline_alter_fk VALIDATE CONSTRAINT alter_fk"), IgnoreRows: true},
		{Name: "alter_validate_constraint_sync", Group: "alters", Note: "without ASYNC should be refused", Steps: one("ALTER TABLE baseline_alter_fk VALIDATE CONSTRAINT alter_fk")},
		{Name: "alter_unique_using_index", Group: "alters", IgnoreRows: true, KnownGap: "DSQL builds the index asynchronously, so it is not yet valid when the constraint is added (55000); the emulator builds synchronously", Steps: []string{
			"CREATE UNIQUE INDEX ASYNC baseline_alter_big_uq ON baseline_alter_big (big)",
			"ALTER TABLE baseline_alter_big ADD CONSTRAINT baseline_alter_big_uq UNIQUE USING INDEX baseline_alter_big_uq",
		}},
	}
}

// typeSlug turns a SQL type into a name-safe token.
func typeSlug(typ string) string {
	var b strings.Builder
	for _, r := range typ {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
