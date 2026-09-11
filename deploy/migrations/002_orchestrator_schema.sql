-- orchestrator schema: live state driving the state machine (Task, Round,
-- StepRun). Full CRUD for orchestrator_app. Nothing in this codebase writes
-- to these tables yet — coreloop currently runs statelessly per request;
-- persisting its live state to Postgres is future work this migration
-- reserves the schema for, not something Phase 5 wires up. Phase 5's own
-- scope is the audit trail (see 003_audit_schema.sql).

CREATE SCHEMA IF NOT EXISTS orchestrator;

CREATE TABLE IF NOT EXISTS orchestrator.tasks (
    id                    text PRIMARY KEY,
    type                  text NOT NULL,
    target_repo           text NOT NULL,
    triggering_principal  text NOT NULL,
    bot_account           text NOT NULL,
    risk_tier             text NOT NULL,
    branch                text NOT NULL,
    base_branch           text NOT NULL,
    pr_number             integer NOT NULL DEFAULT 0,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orchestrator.rounds (
    task_id      text NOT NULL REFERENCES orchestrator.tasks (id),
    round_number integer NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, round_number)
);

CREATE TABLE IF NOT EXISTS orchestrator.step_runs (
    id            bigserial PRIMARY KEY,
    task_id       text NOT NULL,
    round_number  integer NOT NULL,
    step_id       text NOT NULL,
    step_type     text NOT NULL,
    invocation_id text NOT NULL UNIQUE,
    blocking      boolean NOT NULL,
    exit_code     integer NOT NULL,
    has_finding   boolean NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (task_id, round_number) REFERENCES orchestrator.rounds (task_id, round_number)
);

GRANT USAGE ON SCHEMA orchestrator TO orchestrator_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA orchestrator TO orchestrator_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA orchestrator TO orchestrator_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA orchestrator GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO orchestrator_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA orchestrator GRANT USAGE, SELECT ON SEQUENCES TO orchestrator_app;
