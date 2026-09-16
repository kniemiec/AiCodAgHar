-- Wires up the orchestrator schema 002_orchestrator_schema.sql reserved but
-- left unwritten: persisted, per-step-resumable task state, driven by the
-- new internal/taskrunner package (docs/designs/next/full-pipeline-async-
-- approval-design.md §4.2).

ALTER TABLE orchestrator.tasks
    ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'running',
        -- running | waiting_approval | stop_success | stop_escalate
    ADD COLUMN IF NOT EXISTS current_round integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS approval_issue_number integer,
    ADD COLUMN IF NOT EXISTS approved_by text,
    ADD COLUMN IF NOT EXISTS prior_findings jsonb,
    ADD COLUMN IF NOT EXISTS reason text,
    ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();

-- step_runs now gets a row the instant a step is dispatched (status
-- 'launched'), before its outcome is known — exit_code/has_finding aren't
-- known yet at that point, so they must become nullable; a launched row is
-- updated in place once the step resolves. structured_result starts being
-- persisted too: it's what lets the next round's generate-phase dispatch
-- (and startup reconciliation, after resolving a still-launched row via the
-- launcher) rebuild PRIOR_FINDINGS_JSON without re-running anything.
ALTER TABLE orchestrator.step_runs
    ALTER COLUMN exit_code DROP NOT NULL,
    ALTER COLUMN has_finding DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'launched',
        -- launched | completed | failed
    ADD COLUMN IF NOT EXISTS structured_result jsonb,
    ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();
