-- Task Intent Propagation design §2: persists the human-authored task
-- description a Forgejo issue-opened webhook (or POST /tasks) supplies, so
-- it can be threaded into the implement and review-stage prompts.

ALTER TABLE orchestrator.tasks
    ADD COLUMN IF NOT EXISTS description text NOT NULL DEFAULT '';
