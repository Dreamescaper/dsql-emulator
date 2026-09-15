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
    index_name text;
BEGIN
    FOR cmd IN SELECT * FROM pg_event_trigger_ddl_commands() WHERE command_tag = 'CREATE INDEX' LOOP
        SELECT relname INTO index_name FROM pg_class WHERE oid = cmd.objid;
        IF index_name IS NOT NULL THEN
            INSERT INTO sys.jobs (job_id, job_type, status)
            VALUES (md5(index_name), 'INDEX_BUILD', 'COMPLETED')
            ON CONFLICT (job_id) DO UPDATE
                SET status = EXCLUDED.status, created_at = now();
        END IF;
    END LOOP;
END $$;

DROP EVENT TRIGGER IF EXISTS dsql_record_index_job;
CREATE EVENT TRIGGER dsql_record_index_job ON ddl_command_end
    WHEN TAG IN ('CREATE INDEX')
    EXECUTE FUNCTION dsql_internal.record_index_job();
