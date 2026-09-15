-- Aurora DSQL refuses to drop a column that is part of a table's primary key:
-- "cannot drop primary key column <name>". PostgreSQL performs the drop and
-- removes the key with it, which would silently lose it.
--
-- Deciding this needs the catalog, which the emulator does not read, so the
-- check runs here instead. It fires at ddl_command_start, while the key is still
-- in place, and reads the statement text: an event trigger at that point has no
-- parse tree. Only the single-action form is inspected, and anything it cannot
-- read is left alone, so the guard never refuses a statement it does not
-- understand.

CREATE SCHEMA IF NOT EXISTS dsql_internal;

CREATE OR REPLACE FUNCTION dsql_internal.guard_drop_pk_column() RETURNS event_trigger
LANGUAGE plpgsql AS $$
DECLARE
    matched text[];
    target regclass;
    column_name text;
    key_column text;
BEGIN
    matched := regexp_match(current_query(),
        '(?is)^\s*ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?'
        '((?:"[^"]*"|[\w$]+)(?:\s*\.\s*(?:"[^"]*"|[\w$]+))?)\s*\*?\s+'
        'DROP\s+(?:COLUMN\s+)?(?:IF\s+EXISTS\s+)?("[^"]*"|[\w$]+)\s*'
        '(?:RESTRICT|CASCADE)?\s*;?\s*$');
    IF matched IS NULL THEN
        RETURN;
    END IF;

    target := to_regclass(matched[1]);
    IF target IS NULL THEN
        RETURN;
    END IF;

    -- Fold the column name the way PostgreSQL would.
    column_name := CASE
        WHEN matched[2] LIKE '"%"' THEN replace(substr(matched[2], 2, length(matched[2]) - 2), '""', '"')
        ELSE lower(matched[2])
    END;

    SELECT a.attname INTO key_column
    FROM pg_index i
    JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY (i.indkey)
    WHERE i.indrelid = target AND i.indisprimary AND a.attname = column_name;

    IF key_column IS NOT NULL THEN
        RAISE EXCEPTION 'cannot drop primary key column %', key_column USING ERRCODE = '0A000';
    END IF;
END $$;

DROP EVENT TRIGGER IF EXISTS dsql_guard_drop_pk_column;
CREATE EVENT TRIGGER dsql_guard_drop_pk_column ON ddl_command_start
    WHEN TAG IN ('ALTER TABLE')
    EXECUTE FUNCTION dsql_internal.guard_drop_pk_column();
