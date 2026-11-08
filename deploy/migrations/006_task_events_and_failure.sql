-- Platform observability, PR A (docs/designs/active/platform-observability-design.md
-- §2/§3): a first-class `failed` task status with a failure taxonomy, and an
-- append-only task_events table written in the same transaction as state
-- changes. Idempotent: safe to rerun. Stop the control-plane before applying
-- — step (b) would otherwise flip a task that is mid-flight in a live runLoop.

-- (a) failure columns + status vocabulary.
ALTER TABLE orchestrator.tasks
    ADD COLUMN IF NOT EXISTS error_class text,
    ADD COLUMN IF NOT EXISTS retryable boolean,
    ADD COLUMN IF NOT EXISTS retry_count integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS finished_at timestamptz;

ALTER TABLE orchestrator.tasks DROP CONSTRAINT IF EXISTS tasks_status_check;
ALTER TABLE orchestrator.tasks ADD CONSTRAINT tasks_status_check
    CHECK (status IN ('running', 'waiting_approval', 'stop_success', 'stop_escalate', 'failed'));

CREATE TABLE IF NOT EXISTS orchestrator.task_events (
    id            bigserial PRIMARY KEY,
    task_id       text        NOT NULL REFERENCES orchestrator.tasks (id),
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    round         integer     NOT NULL DEFAULT 0,
    step_id       text,
    invocation_id text,
    type          text        NOT NULL,
    payload       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    trace_id      text,
    span_id       text
);
CREATE INDEX IF NOT EXISTS task_events_task_idx ON orchestrator.task_events (task_id, id);
CREATE INDEX IF NOT EXISTS task_events_type_at_idx ON orchestrator.task_events (type, at);

-- The default privileges from 002 grant UPDATE/DELETE on every new table;
-- the event log is INSERT/SELECT only for the application role.
REVOKE UPDATE, DELETE ON orchestrator.task_events FROM orchestrator_app;

-- (b) tasks the old Runner.fail left status=running with an "error:" reason
-- are failures. platform: there is no "unknown" class; retryable: that is the
-- retry the old behaviour promised, and terminal would strand them.
UPDATE orchestrator.tasks
   SET status = 'failed', error_class = 'platform', retryable = true, retry_count = 0
 WHERE status = 'running' AND reason LIKE 'error:%';

-- (c) best available terminal timestamp: the terminal row's last write.
UPDATE orchestrator.tasks
   SET finished_at = updated_at
 WHERE status IN ('stop_success', 'stop_escalate') AND finished_at IS NULL;

-- (d) every task gets a non-empty timeline.
INSERT INTO orchestrator.task_events (task_id, at, round, type, payload)
SELECT t.id, t.updated_at, t.current_round, 'task.migrated',
       jsonb_build_object('status', t.status, 'round', t.current_round, 'history_recorded', false)
  FROM orchestrator.tasks t
 WHERE NOT EXISTS (SELECT 1 FROM orchestrator.task_events e WHERE e.task_id = t.id);
