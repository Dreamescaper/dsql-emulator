-- Records an asynchronous DDL operation as a job in sys.jobs. Aurora DSQL runs
-- index builds and constraint validation in the background and reports them
-- through sys.jobs; the emulator runs them synchronously, so this is what makes
-- a job visible once the work is done.
--
-- The job id is derived from the object name with md5, which is the same
-- derivation the emulator uses for the id it returns to the client, so the
-- returned id can be looked up here.
--
-- The trigger is registered for CREATE INDEX and ALTER TABLE. Registering for
-- CREATE INDEX is what keeps a table's key index out of sys.jobs, since that
-- index is created by CREATE TABLE. ALTER TABLE only records a job for
-- VALIDATE CONSTRAINT, which is detected from the statement text; the
-- synchronous form never reaches here because the emulator refuses it.

CREATE SCHEMA IF NOT EXISTS dsql_internal;

CREATE OR REPLACE FUNCTION dsql_internal.record_ddl_job() RETURNS event_trigger
LANGUAGE plpgsql AS $$
DECLARE
    cmd record;
    object_name text;
    job_type text;
    digest text;
BEGIN
    FOR cmd IN SELECT * FROM pg_event_trigger_ddl_commands() LOOP
        object_name := NULL;
        job_type := NULL;

        IF cmd.command_tag = 'CREATE INDEX' THEN
            job_type := 'INDEX_BUILD';
            SELECT n.nspname || '.' || c.relname, md5(c.relname)
              INTO object_name, digest
              FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE c.oid = cmd.objid;
        ELSIF cmd.command_tag = 'ALTER TABLE'
              AND current_query() ~* 'VALIDATE\s+CONSTRAINT' THEN
            -- class_id 1259 is pg_class, the catalog DSQL reports.
            job_type := 'VALIDATE_CONSTRAINT';
            SELECT n.nspname || '.' || c.relname, md5('validate:' || c.relname)
              INTO object_name, digest
              FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE c.oid = cmd.objid;
        END IF;

        IF job_type IS NOT NULL AND digest IS NOT NULL THEN
            INSERT INTO sys.jobs
                (job_id, status, details, job_type, class_id, object_id, object_name, start_time, update_time)
            -- Shaped as a UUID, because wait_for_job converts ids to one.
            VALUES (substr(digest, 1, 8) || '-' || substr(digest, 9, 4) || '-'
                        || substr(digest, 13, 4) || '-' || substr(digest, 17, 4) || '-'
                        || substr(digest, 21, 12),
                    'completed', NULL, job_type, 1259, cmd.objid, object_name, now(), now())
            ON CONFLICT (job_id) DO UPDATE
                SET status = EXCLUDED.status, update_time = EXCLUDED.update_time;
        END IF;
    END LOOP;
END $$;

DROP EVENT TRIGGER IF EXISTS dsql_record_ddl_job;
CREATE EVENT TRIGGER dsql_record_ddl_job ON ddl_command_end
    WHEN TAG IN ('CREATE INDEX', 'ALTER TABLE')
    EXECUTE FUNCTION dsql_internal.record_ddl_job();
