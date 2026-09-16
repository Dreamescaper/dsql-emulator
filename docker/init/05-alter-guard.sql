-- Aurora DSQL refuses to drop a column that is part of a table's primary key:
-- "cannot drop primary key column <name>". PostgreSQL performs the drop and
-- removes the key with it, which would silently lose it.
--
-- Deciding this needs the catalog, which the emulator does not read, so the
-- check runs here. PostgreSQL reports the drop structurally rather than as
-- text, in two parts:
--
--   * at ddl_command_start, while the keys are still in place, every primary
--     key column in the database is snapshotted as `<table oid>:<attnum>`;
--   * at sql_drop, pg_event_trigger_dropped_objects() names each column the
--     command dropped, and one of them matching the snapshot is the refusal.
--
-- Object addresses are integers, so no statement text is parsed and nothing
-- depends on quoting, IF EXISTS, ONLY, or schema qualification. A command that
-- drops several columns at once is covered, which reading the statement text
-- was not. sql_drop fires after the drop but inside the same transaction, so
-- raising there fails the statement and aborts the transaction exactly as
-- refusing up front would.

CREATE SCHEMA IF NOT EXISTS dsql_internal;

-- The snapshot covers every primary key in the database, because the statement
-- being started is not yet parsed and its target is unknown. That is a scan of
-- pg_index per ALTER TABLE, which an emulator's schema can afford.
CREATE OR REPLACE FUNCTION dsql_internal.snapshot_pk_columns() RETURNS event_trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM set_config('dsql.pk_columns', COALESCE((
        SELECT string_agg(i.indrelid || ':' || a.attnum, ',')
          FROM pg_index i
          JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
         WHERE i.indisprimary), ''), true);
END $$;

CREATE OR REPLACE FUNCTION dsql_internal.guard_drop_pk_column() RETURNS event_trigger
LANGUAGE plpgsql AS $$
DECLARE
    key_columns text[];
    dropped record;
BEGIN
    key_columns := string_to_array(
        COALESCE(NULLIF(current_setting('dsql.pk_columns', true), ''), ','), ',');

    FOR dropped IN
        SELECT * FROM pg_event_trigger_dropped_objects()
         WHERE object_type = 'table column'
           -- Only a column the command named itself; one that goes with a
           -- dropped table is not a refusal.
           AND original
    LOOP
        IF dropped.objid || ':' || dropped.objsubid = ANY (key_columns) THEN
            RAISE EXCEPTION 'cannot drop primary key column %', dropped.address_names[3]
                USING ERRCODE = '0A000';
        END IF;
    END LOOP;
END $$;

DROP EVENT TRIGGER IF EXISTS dsql_snapshot_pk_columns;
CREATE EVENT TRIGGER dsql_snapshot_pk_columns ON ddl_command_start
    WHEN TAG IN ('ALTER TABLE')
    EXECUTE FUNCTION dsql_internal.snapshot_pk_columns();

DROP EVENT TRIGGER IF EXISTS dsql_guard_drop_pk_column;
CREATE EVENT TRIGGER dsql_guard_drop_pk_column ON sql_drop
    WHEN TAG IN ('ALTER TABLE')
    EXECUTE FUNCTION dsql_internal.guard_drop_pk_column();
