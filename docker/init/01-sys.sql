-- Aurora DSQL exposes an async index build through sys.jobs and sys.wait_for_job.
-- The emulator builds indexes synchronously, so this schema provides the surface
-- the queries expect.
CREATE SCHEMA IF NOT EXISTS sys;

CREATE TABLE IF NOT EXISTS sys.jobs (
    job_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_type text NOT NULL,
    status text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION sys.wait_for_job(job_id uuid)
RETURNS text
LANGUAGE sql
AS $$ SELECT 'completed'::text $$;
