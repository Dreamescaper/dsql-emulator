-- Records an asynchronous index build as a job in sys.jobs. Aurora DSQL builds
-- indexes in the background and reports progress through sys.jobs; the emulator
-- builds synchronously, so this is what makes the job visible once the index
-- exists.
--
-- The job id is derived from the index name, which is the same derivation the
-- emulator uses for the id it returns to the client, so the returned id can be
-- looked up here.
--
-- The trigger is registered for CREATE INDEX only, so the index that CREATE
-- TABLE creates for a primary key or unique constraint does not produce a job.

CREATE SCHEMA IF NOT EXISTS dsql_internal;

CREATE OR REPLACE FUNCTION dsql_internal.record_index_job() RETURNS event_trigger
LANGUAGE plpgsql AS $$
DECLARE
    cmd record;
BEGIN
    FOR cmd IN SELECT * FROM pg_event_trigger_ddl_commands() WHERE command_tag = 'CREATE INDEX' LOOP
        -- class_id 1259 is pg_class, the catalog DSQL reports for an index.
        INSERT INTO sys.jobs
            (job_id, status, details, job_type, class_id, object_id, object_name, start_time, update_time)
        -- The job id is md5 of the index name, shaped as a UUID to match what
        -- the emulator hands the client and what wait_for_job accepts.
        SELECT substr(m, 1, 8) || '-' || substr(m, 9, 4) || '-' || substr(m, 13, 4)
                   || '-' || substr(m, 17, 4) || '-' || substr(m, 21, 12),
               'completed', NULL, 'INDEX_BUILD', 1259, oid, object_name, now(), now()
        FROM (
            SELECT md5(c.relname) AS m, c.oid AS oid,
                   n.nspname || '.' || c.relname AS object_name
            FROM pg_class c
            JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE c.oid = cmd.objid
        ) AS job
        ON CONFLICT (job_id) DO UPDATE
            SET status = EXCLUDED.status, update_time = EXCLUDED.update_time;
    END LOOP;
END $$;

DROP EVENT TRIGGER IF EXISTS dsql_record_index_job;
CREATE EVENT TRIGGER dsql_record_index_job ON ddl_command_end
    WHEN TAG IN ('CREATE INDEX')
    EXECUTE FUNCTION dsql_internal.record_index_job();
