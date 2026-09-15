// Package conformance describes a suite of SQL probes and records how a target
// database answers them. The same suite runs against a real Aurora DSQL cluster
// to produce a golden record and against the emulator to check it.
package conformance

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
func DefaultSuite() Suite {
	return Suite{
		Name: "dsql-baseline",
		Setup: []string{
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
		},
		Cleanup: []string{
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
			"DROP VIEW IF EXISTS baseline_v",
			"DROP INDEX IF EXISTS baseline_idx_value",
			"DROP INDEX IF EXISTS baseline_idx_value_async",
			"DROP SEQUENCE IF EXISTS baseline_seq",
			"DROP SEQUENCE IF EXISTS baseline_seq2",
			"DROP SEQUENCE IF EXISTS baseline_seq_one",
			"DROP FUNCTION IF EXISTS baseline_fn",
			"DROP DOMAIN IF EXISTS baseline_domain",
			"DROP SCHEMA IF EXISTS baseline_schema",
		},
		Cases: []Case{
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
			{Name: "row_cap", Group: "transaction", Note: "DSQL fails the statement that crosses the cap and aborts the transaction", Steps: []string{"BEGIN", "INSERT INTO baseline_bulk SELECT generate_series(1, 3001)", "SELECT 1"}, KnownGap: "M3: the emulator cannot refuse a statement it has already sent, and does not model the aborted-transaction state"},
			{Name: "commit", Group: "transaction", Steps: []string{"BEGIN", "INSERT INTO baseline_parent (id, name) VALUES ('00000000-0000-0000-0000-0000000000bb', 'tx')", "COMMIT"}},
			{Name: "rollback", Group: "transaction", Steps: []string{"BEGIN", "INSERT INTO baseline_parent (id, name) VALUES ('00000000-0000-0000-0000-0000000000cc', 'tx')", "ROLLBACK", "SELECT count(*) FROM baseline_parent WHERE name = 'tx'"}},
			{Name: "read_only", Group: "transaction", Note: "DSQL refuses SET TRANSACTION and then the transaction is aborted", Steps: []string{"BEGIN", "SET TRANSACTION READ ONLY", "SELECT 1", "COMMIT"}, KnownGap: "M3: the SET is now refused, but the aborted-transaction state is not modelled"},

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
		},
	}
}
