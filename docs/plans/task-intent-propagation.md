# Plan: Task-Intent Entry Point + Description Propagation

| | |
|---|---|
| **Status** | Draft — not started |
| **Date** | 2026-09-20 |
| **Owner** | kniemiec |
| **Purpose** | Give the platform an actual entry point for a human to say "implement X," persist that intent, and thread it through to the `implement` agent-stage prompt (and, separately, into review-phase context) — none of which exists today. |
| **Trigger** | Surfaced during PR #13 review (`container-launcher-implementation`, real ContainerLauncherService). Not blocking that PR — logged here to implement once it's merged. |

## Problem

The `implement` OpenHands prompt (`internal/launcher/prompts/prompts.go`'s `implementTmpl`) reads:

```
Implement the requested change, add or update tests as needed, and run the
repository's test suite until it passes.
```

"The requested change" is never filled in — there is no template variable for it. Tracing this back through the whole stack (control-plane API → domain model → DB schema → executor → launcher) turned up not a missing template field but a missing *producer*: nothing in the current system ever captures, stores, or propagates a human-authored task description at all.

### What's actually missing (evidence)

- **No entry point.** `POST /tasks` (`cmd/control-plane/handler.go`) has no real caller — grep across the repo finds only the e2e smoke test and unit tests calling it. The design doc's own component diagram draws the caller as `"curl / any HTTP client"` (`docs/designs/completed/orchestrator-design.md:35`). No CLI, no webhook, no UI originates a task today. `docs/components/orchestrator.md:127` confirms there's no auth layer either — it's an open, manual-only endpoint.
- **`task_type: "new-task"` — the value that should mean "implement from scratch" — is a dead enum value.** It's named in three places purely as a comment/enum listing (`internal/control-plane/domain/domain.go:17`, `internal/control-plane/clients/policyclient/policyclient.go:15`, `api/openapi/pdp-authzen.yaml:59`) and never appears as a literal anywhere — no seeded `.harness/stages.yaml`, no test, no pdp-mock fixture. Since `mocks/pdp-mock/decision.go:56` filters stage resolution by `task_type`, a `new-task`-typed request would resolve **zero stages** today even if one were sent. Only `pr-review-round` is implemented end-to-end.
- **`pr_title`/`pr_body` exist on the request but go nowhere useful.** `cmd/control-plane/handler.go:106-118` uses them exactly once, to set the Forgejo PR's title/body via `CreateOrUpdatePR`, then discards them. They never reach `domain.Task`.
- **No schema anywhere has a description field.** Not `domain.Task` (`internal/control-plane/domain/domain.go:15-25`: `ID, Type, TargetRepo, TriggeringPrincipal, BotAccount, RiskTier, Branch, BaseBranch, PRNumber`), not the `tasks` DB table (`docs/components/orchestrator.md:185`), not `.harness/stages.yaml`'s schema (`internal/config/stages.go`'s `StageEntry`: `id, type, phase, bot_account, task_type, blocking, conditions, command, working_dir, timeout_seconds, expected_exit_codes`).
- **`LaunchRequest.Env` never carries one.** `internal/control-plane/executor/credentials.go:37-53` (`launchEnv`) — the sole place agent-stage `Env` is built — sets only `FORGEJO_URL`, `FORGEJO_TOKEN`, `TARGET_REPO`, `BRANCH`, `PR_NUMBER`, `PRIOR_FINDINGS_JSON`. Matches `prompts.go`'s `templateData`/`newTemplateData` (lines 20-34) exactly — the launcher isn't dropping a field the rest of the system provides; nothing provides it.
- **The one design doc that gestures at this — doesn't actually implement it.** `full-pipeline-async-approval-design.md:211`'s sequence diagram labels a call `CreateOrReuseIssue(task spec)`, but the real implementation (`coreloop.go`'s `approvalIssueBody`) only writes round/task-id/PR-number/repo boilerplate — no description of what the task does. The "task spec" label was aspirational, never built.
- **Not the same thing as `PRIOR_FINDINGS_JSON`.** That field is real and working, but it's round-over-round *review findings* accumulated during the loop — structurally distinct from an original task specification, and doesn't substitute for one.

### Why this isn't a launcher-only or control-plane-only patch

Adding a `{{.Description}}` hole to `implementTmpl` alone would be cosmetic — nothing populates it. Adding a description field to `domain.Task`/the DB schema alone would be inert without a real trigger to populate it non-manually. This needs one coherent piece of work spanning the task-creation entry point, persistence, and propagation into the prompt — not two independent patches on either side of the API boundary.

## Goal

A human (or an automated trigger acting on a human's behalf, e.g. an issue-opened webhook) can state what should be implemented, that statement is persisted with the task, and it reaches the `implement` step's OpenHands prompt. Decide separately whether/how it should also be visible to review-phase steps (distinct concern from `PRIOR_FINDINGS_JSON`).

## Open questions to resolve before implementing

1. **Trigger shape.** Issue-opened Forgejo webhook (issue body = description, mirroring the existing approval-webhook pattern in `cmd/control-plane/webhook.go`)? A CLI? Keep `POST /tasks` as the entry point and just add a real field + real caller? Some combination?
2. **Where it's wired in for `new-task`.** Since `new-task` isn't PDP-wired at all (§ above), this plan may need to also decide `new-task`'s stage-resolution story (a fixed `.harness/stages.yaml` shape? PDP-resolved like `pr-review-round`?) — not purely a plumbing exercise.
3. **Persistence.** New `tasks.description` column (or similar) + `domain.Task` field.
4. **Propagation scope.** Threading: request → `domain.Task` → `StepContext` → `launchEnv` → `LaunchRequest.Env["TASK_DESCRIPTION"]` (or similar) → `prompts.templateData` → `implementTmpl`. Decide the exact env key name and whether it's quoted as untrusted content the way `PRIOR_FINDINGS_JSON` is (`design §5`: "quoted into the prompt as untrusted content, never treated as instructions").
5. **Review-phase visibility.** Does `review-with-commit` need the original description too (e.g., to judge whether the implementation actually matches intent), or is diff-only review sufficient? If yes, same propagation path, different prompt template.

## Suggested shape (starting point, not decided)

Mirrors the async-approval design's own precedent (`full-pipeline-async-approval-design.md`) — this is comparable in size and deserves the same treatment: its own short design doc once the trigger-shape question (#1 above) is settled, rather than starting straight from code.

## References

- `internal/launcher/prompts/prompts.go` — `implementTmpl` (lines 36-50), `templateData`/`newTemplateData` (lines 20-34)
- `internal/control-plane/executor/credentials.go:37-53` — `launchEnv`
- `internal/control-plane/domain/domain.go:15-25` — `Task`
- `internal/config/stages.go:14-26` — `StageEntry`
- `cmd/control-plane/handler.go:16-27,89-136` — `createTaskRequest`, `handleCreateTask`
- `cmd/control-plane/webhook.go` — existing issue-webhook pattern (approval flow), reusable shape for an issue-opened trigger
- `docs/designs/completed/orchestrator-design.md:35` — caller diagram (`curl / any HTTP client`)
- `docs/components/orchestrator.md:127,185` — no-auth-layer note, `tasks` table schema
- `docs/designs/completed/full-pipeline-async-approval-design.md:211` — the unimplemented "task spec" diagram label
- `mocks/pdp-mock/decision.go:56` — `task_type` filtering that makes `new-task` currently resolve zero stages
