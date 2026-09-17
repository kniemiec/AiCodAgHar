-- audit schema: the append-only, hash-chained invocation_audit trail
-- (ADR-5). One row per invocation_id, merging ADR-2 (delegation record) +
-- ADR-3 (PDP decision, incl. ADR-11's resolved_steps) + ADR-1 (execution
-- outcome) + ADR-9 (approval fields), plus prev_hash/row_hash.
--
-- step_type also covers two Phase 5 additions beyond "agent-stage" /
-- "deterministic-step": "pdp-decision" (one row per round's PDP Evaluate
-- call, so a deny/require-approval round that launches nothing is still
-- audited) and "task-outcome" (one row per task's terminal
-- stop-success/stop-escalate outcome).
--
-- "round" is a Phase 5 addition to the InvocationAuditRecord shape sketched
-- in the plan doc: without it, the policy-gate reconciliation check
-- (launch count vs. PDP-allow count) would have to parse invocation_id
-- strings to correlate a round's PDP decision with that round's launches.
--
-- Grants: SELECT + INSERT only, never UPDATE/DELETE. SELECT is required —
-- the hash chain must read the previous row's row_hash to extend the
-- chain, and GetTaskChain is a read — so ADR-5's "INSERT-only" is
-- interpreted here as "append-only, no mutation," verified by the
-- Postgres-role privilege test in
-- internal/control-plane/audit/postgres_integration_test.go (attempts an UPDATE as this
-- role and confirms Postgres itself rejects it).
--
-- resolved_steps is `text`, not `jsonb`: row_hash is computed over the
-- exact bytes of resolved_steps_json as originally marshaled, and jsonb
-- re-serializes on storage (different whitespace than Go's compact
-- encoder), which broke hash verification on read-back. granted_scopes
-- stays jsonb safely, since its hash input is always re-marshaled fresh
-- from the typed Go value, never read back from stored bytes.

CREATE SCHEMA IF NOT EXISTS audit;

CREATE TABLE IF NOT EXISTS audit.invocation_audit (
    id                    bigserial PRIMARY KEY,
    invocation_id         text NOT NULL UNIQUE,
    task_id               text NOT NULL,
    round                 integer NOT NULL,
    step_id               text NOT NULL DEFAULT '',
    step_type             text NOT NULL,
    bot_account           text NOT NULL DEFAULT '',
    target_repo           text NOT NULL DEFAULT '',
    policy_decision       text NOT NULL DEFAULT '',
    granted_scopes        jsonb NOT NULL DEFAULT '[]'::jsonb,
    policy_bundle_version text NOT NULL DEFAULT '',
    resolved_steps        text,
    exit_code             integer NOT NULL DEFAULT 0,
    status                text NOT NULL DEFAULT '',
    approved              boolean NOT NULL DEFAULT false,
    approved_by           text NOT NULL DEFAULT '',
    evidence              text NOT NULL DEFAULT '',
    prev_hash             text NOT NULL,
    row_hash              text NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS invocation_audit_task_id_idx ON audit.invocation_audit (task_id, id);

GRANT USAGE ON SCHEMA audit TO orchestrator_audit_writer;
GRANT SELECT, INSERT ON audit.invocation_audit TO orchestrator_audit_writer;
GRANT USAGE, SELECT ON audit.invocation_audit_id_seq TO orchestrator_audit_writer;
