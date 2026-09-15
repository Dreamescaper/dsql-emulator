-- Aurora DSQL rejects any transaction that modifies more than 3000 rows, and the
-- statement that crosses the limit fails with SQLSTATE 54000. PostgreSQL has no
-- equivalent, so a row trigger counts modifications per transaction and raises
-- the same error, which fails the statement and aborts the transaction exactly
-- as DSQL does. The cap is read from dsql.row_cap, which the emulator passes
-- through the startup options.
--
-- Everything here lives in the backing database and is invisible to clients.

CREATE SCHEMA IF NOT EXISTS dsql_internal;

CREATE OR REPLACE FUNCTION dsql_internal.row_cap() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cap bigint;
    total bigint;
BEGIN
    cap := COALESCE(NULLIF(current_setting('dsql.row_cap', true), ''), '3000')::bigint;
    -- Transaction-local, so the count resets with the transaction.
    total := COALESCE(NULLIF(current_setting('dsql.rows', true), ''), '0')::bigint + 1;
    IF total > cap THEN
        RAISE EXCEPTION 'transaction row limit exceeded' USING ERRCODE = '54000';
    END IF;
    PERFORM set_config('dsql.rows', total::text, true);
    RETURN NULL;
END $$;

-- Attach the trigger to tables created after this point.
CREATE OR REPLACE FUNCTION dsql_internal.attach_row_cap() RETURNS event_trigger
LANGUAGE plpgsql AS $$
DECLARE
    cmd record;
BEGIN
    FOR cmd IN SELECT * FROM pg_event_trigger_ddl_commands() LOOP
        -- A CREATE TABLE with a primary key also reports its index, which
        -- cannot carry a trigger, so only tables are considered.
        IF cmd.command_tag IN ('CREATE TABLE', 'CREATE TABLE AS', 'SELECT INTO')
           AND cmd.object_type = 'table'
           AND cmd.schema_name NOT IN ('pg_catalog', 'information_schema', 'sys', 'dsql_internal') THEN
            EXECUTE format(
                'CREATE TRIGGER dsql_row_cap AFTER INSERT OR UPDATE OR DELETE ON %s '
                'FOR EACH ROW EXECUTE FUNCTION dsql_internal.row_cap()',
                cmd.object_identity);
        END IF;
    END LOOP;
END $$;

DROP EVENT TRIGGER IF EXISTS dsql_attach_row_cap;
CREATE EVENT TRIGGER dsql_attach_row_cap ON ddl_command_end
    WHEN TAG IN ('CREATE TABLE', 'CREATE TABLE AS', 'SELECT INTO')
    EXECUTE FUNCTION dsql_internal.attach_row_cap();

-- Attach it to the tables that already exist.
DO $$
DECLARE
    rel record;
BEGIN
    FOR rel IN
        SELECT n.nspname AS schema_name, c.relname AS table_name
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relkind = 'r'
          AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sys', 'dsql_internal')
    LOOP
        EXECUTE format(
            'DROP TRIGGER IF EXISTS dsql_row_cap ON %I.%I', rel.schema_name, rel.table_name);
        EXECUTE format(
            'CREATE TRIGGER dsql_row_cap AFTER INSERT OR UPDATE OR DELETE ON %I.%I '
            'FOR EACH ROW EXECUTE FUNCTION dsql_internal.row_cap()',
            rel.schema_name, rel.table_name);
    END LOOP;
END $$;
