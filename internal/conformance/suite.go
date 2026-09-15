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
}

func one(sql string) []string { return []string{sql} }

// DefaultSuite is the baseline probe set. It stays small on purpose: every case
// is one request against a metered cluster.
func setupStatements() []string {
	return []string{
		"DROP TABLE IF EXISTS baseline_child",
		"DROP TABLE IF EXISTS baseline_parent",
		"DROP TABLE IF EXISTS baseline_idx",
		"DROP TABLE IF EXISTS baseline_bulk",
		"DROP TABLE IF EXISTS baseline_drop_me",
		"CREATE TABLE baseline_parent (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name text NOT NULL)",
		"CREATE TABLE baseline_child (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), parent_id uuid NOT NULL REFERENCES baseline_parent(id))",
		"CREATE TABLE baseline_idx (id uuid PRIMARY KEY, value text)",
		"CREATE TABLE baseline_bulk (id int)",
		"CREATE TABLE baseline_drop_me (id int)",
		"INSERT INTO baseline_parent (id, name) VALUES ('00000000-0000-0000-0000-0000000000aa', 'seed')",
	}
}

func cleanupStatements() []string {
	return []string{
		// Tables that reference baseline_parent must go first.
		"DROP TABLE IF EXISTS baseline_child",
		"DROP TABLE IF EXISTS baseline_deferred",
		"DROP TABLE IF EXISTS baseline_parent",
		"DROP TABLE IF EXISTS baseline_idx",
		"DROP TABLE IF EXISTS baseline_bulk",
		"DROP TABLE IF EXISTS baseline_drop_me",
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
		"DROP VIEW IF EXISTS baseline_v",
		"DROP INDEX IF EXISTS baseline_idx_value",
		"DROP INDEX IF EXISTS baseline_idx_value_async",
		"DROP SEQUENCE IF EXISTS baseline_seq",
		"DROP SEQUENCE IF EXISTS baseline_seq2",
		"DROP SEQUENCE IF EXISTS baseline_seq_one",
		"DROP FUNCTION IF EXISTS baseline_fn",
		"DROP DOMAIN IF EXISTS baseline_domain",
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
		{Name: "create_index_async", Group: "index", Steps: one("CREATE INDEX ASYNC IF NOT EXISTS baseline_idx_value_async ON baseline_idx (value)"), KnownGap: "M6: the emulator does not rewrite CREATE INDEX ASYNC yet"},
		{Name: "sys_jobs", Group: "index", Steps: one("SELECT count(*) AS n FROM sys.jobs"), IgnoreRows: true, KnownGap: "M6: sys.jobs is not emulated"},

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
