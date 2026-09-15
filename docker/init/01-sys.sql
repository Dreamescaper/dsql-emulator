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
    SELECT status INTO job_status FROM sys.jobs WHERE sys.jobs.job_id = p_job_id;
    IF job_status IS NULL THEN
        RAISE EXCEPTION 'unknown job %', p_job_id USING ERRCODE = '22023';
    END IF;
END $$;
