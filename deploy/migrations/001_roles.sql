-- Two application roles, per ADR-5's "INSERT-only" being a Postgres grant,
-- not a convention. Idempotent: safe to rerun (updates the password if the
-- role already exists).
--
-- Placeholders (__ORCHESTRATOR_APP_PASSWORD__, __ORCHESTRATOR_AUDIT_WRITER_PASSWORD__)
-- are substituted by deploy/migrate.sh before this file reaches psql —
-- psql's own :'var' substitution does not work inside a dollar-quoted
-- DO $$ ... $$ body (it's deliberately skipped there, since PL/pgSQL uses
-- := for assignment).

DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'orchestrator_app') THEN
    EXECUTE format('ALTER ROLE orchestrator_app WITH LOGIN PASSWORD %L', '__ORCHESTRATOR_APP_PASSWORD__');
  ELSE
    EXECUTE format('CREATE ROLE orchestrator_app WITH LOGIN PASSWORD %L', '__ORCHESTRATOR_APP_PASSWORD__');
  END IF;
END
$$;

DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = 'orchestrator_audit_writer') THEN
    EXECUTE format('ALTER ROLE orchestrator_audit_writer WITH LOGIN PASSWORD %L', '__ORCHESTRATOR_AUDIT_WRITER_PASSWORD__');
  ELSE
    EXECUTE format('CREATE ROLE orchestrator_audit_writer WITH LOGIN PASSWORD %L', '__ORCHESTRATOR_AUDIT_WRITER_PASSWORD__');
  END IF;
END
$$;
