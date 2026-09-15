-- Aurora DSQL's administrative user is `admin`, and clients connect as it.
-- The emulator relays the client's credentials to this database, so the role has
-- to exist here or every connection would fail with "role admin does not exist".
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'admin') THEN
        CREATE ROLE admin LOGIN SUPERUSER;
    END IF;
END
$$;
