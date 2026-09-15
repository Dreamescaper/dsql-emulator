-- Aurora DSQL exposes an async index build through sys.jobs and sys.wait_for_job.
-- The emulator builds indexes synchronously, so this schema provides the surface
-- the queries expect.
CREATE SCHEMA IF NOT EXISTS sys;

CREATE TABLE IF NOT EXISTS sys.jobs (
    job_id text PRIMARY KEY,
    job_type text NOT NULL,
    status text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Index builds are recorded by dsql_internal.record_index_job. The emulator
-- builds indexes synchronously, so a job is already complete by the time it is
-- visible; this reports its status, and rejects an id it does not know.
CREATE OR REPLACE FUNCTION sys.wait_for_job(job_id text)
RETURNS text
LANGUAGE plpgsql
AS $$
DECLARE
    job_status text;
BEGIN
    SELECT status INTO job_status FROM sys.jobs WHERE sys.jobs.job_id = wait_for_job.job_id;
    IF job_status IS NULL THEN
        RAISE EXCEPTION 'unknown job %', wait_for_job.job_id USING ERRCODE = '22023';
    END IF;
    RETURN job_status;
END $$;
