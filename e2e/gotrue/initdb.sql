-- The e2e GoTrue's database, initialised the way the deployment's db-init Job
-- initialises the real one: a login role whose search_path is `auth`, a
-- database it owns, an `auth` schema it owns, and a NOLOGIN `postgres` role —
-- GoTrue v2.189.0's migrations GRANT to `postgres` unconditionally and refuse
-- to boot without it. Runs once, as the container's superuser, on an empty
-- (tmpfs) data directory.
CREATE ROLE muster_auth WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD 'muster-auth-e2e-not-a-secret';
ALTER ROLE muster_auth SET search_path = auth;
CREATE ROLE postgres NOLOGIN;
CREATE DATABASE muster_auth_e2e OWNER muster_auth;
REVOKE ALL ON DATABASE muster_auth_e2e FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE muster_auth_e2e TO muster_auth;
\connect muster_auth_e2e
CREATE SCHEMA IF NOT EXISTS auth AUTHORIZATION muster_auth;
