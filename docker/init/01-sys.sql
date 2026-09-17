-- Aurora DSQL exposes an async index build through sys.jobs and sys.wait_for_job.
-- The emulator builds indexes synchronously, so this schema provides the surface
-- the queries expect.
CREATE SCHEMA IF NOT EXISTS sys;

CREATE TABLE IF NOT EXISTS sys.jobs (
    job_id text PRIMARY KEY,
    status text NOT NULL,
    details text,
    job_type text NOT NULL,
    class_id oid NOT NULL,
    object_id oid NOT NULL,
    object_name text,
    start_time timestamptz NOT NULL DEFAULT now(),
    update_time timestamptz
);

-- Aurora DSQL exposes this as a procedure, so calling it in a SELECT fails the
-- same way there. The emulator builds synchronously, so a completed job is
-- already in sys.jobs and there is nothing to wait for.
CREATE OR REPLACE PROCEDURE sys.wait_for_job(p_job_id text)
LANGUAGE plpgsql
AS $$
DECLARE
    job_status text;
BEGIN
    -- A job id is 26 base32 characters carrying the same 128 bits as a UUID,
    -- and Aurora DSQL converts it to one, so an id that is not of that shape
    -- fails with 22P02 rather than being reported as unknown. The message is
    -- DSQL's rather than PostgreSQL's, which names the offending value.
    IF p_job_id IS NULL OR p_job_id !~ '^[a-z2-7]{26}$' THEN
        RAISE EXCEPTION 'Unable to convert text to UUID' USING ERRCODE = '22P02';
    END IF;
    SELECT status INTO job_status FROM sys.jobs WHERE sys.jobs.job_id = p_job_id;
    IF job_status IS NULL THEN
        RAISE EXCEPTION 'unknown job %', p_job_id USING ERRCODE = '22023';
    END IF;
END $$;
