-- Records an asynchronous DDL operation as a job in sys.jobs. Aurora DSQL runs
-- index builds and constraint validation in the background and reports them
-- through sys.jobs; the emulator runs them synchronously, so this is what makes
-- a job visible once the work is done.
--
-- The emulator picks the job id, hands it to the client, and passes it down in
-- a marker comment on the statement itself: `/* dsql_job=<job id> */ CREATE
-- INDEX ...`. That is one statement and no extra round trip, and it works for a
-- statement that names no object, such as an unnamed index, where an id derived
-- from the object name would have had nothing to derive from. Each build gets
-- its own id, as it does on a real cluster.
--
-- The marker is also what identifies an emulator-issued asynchronous ALTER
-- TABLE, so only the ASYNC form records a validation job; the synchronous form
-- never reaches here because the emulator refuses it. A statement run directly
-- against the backing database carries no marker and is recorded under a fresh
-- id, which nothing will look up.
--
-- Registering for CREATE INDEX rather than CREATE TABLE is what keeps a table's
-- key index out of sys.jobs, since that index is created by CREATE TABLE.

CREATE SCHEMA IF NOT EXISTS dsql_internal;

CREATE OR REPLACE FUNCTION dsql_internal.record_ddl_job() RETURNS event_trigger
LANGUAGE plpgsql AS $$
DECLARE
    cmd record;
    marker text[];
    object_name text;
    job_type text;
BEGIN
    marker := regexp_match(current_query(),
        'dsql_job=([a-z2-7]{26})');

    FOR cmd IN SELECT * FROM pg_event_trigger_ddl_commands() LOOP
        object_name := NULL;
        job_type := NULL;

        IF cmd.command_tag = 'CREATE INDEX' THEN
            job_type := 'INDEX_BUILD';
        ELSIF cmd.command_tag = 'ALTER TABLE' AND marker IS NOT NULL THEN
            job_type := 'VALIDATE_CONSTRAINT';
        END IF;

        IF job_type IS NULL THEN
            CONTINUE;
        END IF;

        SELECT n.nspname || '.' || c.relname INTO object_name
          FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE c.oid = cmd.objid;

        INSERT INTO sys.jobs
            (job_id, status, details, job_type, class_id, object_id, object_name, start_time, update_time)
        -- class_id 1259 is pg_class, the catalog DSQL reports.
        VALUES (COALESCE(marker[1], gen_random_uuid()::text),
                'completed', NULL, job_type, 1259, cmd.objid, object_name, now(), now())
        ON CONFLICT (job_id) DO UPDATE
            SET status = EXCLUDED.status, update_time = EXCLUDED.update_time;
    END LOOP;
END $$;

DROP EVENT TRIGGER IF EXISTS dsql_record_ddl_job;
CREATE EVENT TRIGGER dsql_record_ddl_job ON ddl_command_end
    WHEN TAG IN ('CREATE INDEX', 'ALTER TABLE')
    EXECUTE FUNCTION dsql_internal.record_ddl_job();
