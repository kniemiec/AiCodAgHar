# Plan: Local Gitea + Actions as GitHub Replacement (Agentic Dev Platform Foundation)

| | |
|---|---|
| **Status** | Draft |
| **Date** | 2026-08-21 |
| **Owner** | Krzysztof Niemiec |
| **Purpose** | Stand up a local, self-hosted git + CI/CD platform to replace GitHub as the primary source control for personal projects, and to serve as the experimental substrate for agentic software development scenarios. |

## Goal

Run Gitea (or Forgejo) locally as the main source-control host for all projects, with a built-in Actions-compatible CI runner for moderately complex pipelines. Keep day-to-day operation fully local (no cloud dependency for git/CI), with periodic encrypted backups shipped off-box purely for disaster recovery (lost/damaged machine).

## Why this option

- Gitea/Forgejo bundles git hosting + PRs/issues + webhooks + Actions CI in one deployable unit — closest like-for-like GitHub replacement with the least moving parts.
- Actions workflows use GitHub Actions-compatible YAML, so existing knowledge/workflows transfer directly.
- Considered and deferred: Jenkins (more powerful but no hosting, heavier plugin/Groovy maintenance), ArgoCD (GitOps/CD for Kubernetes only — irrelevant unless a scenario specifically needs k8s deployment).

## Architecture Overview

- **Host**: local machine, Docker + docker-compose.
- **Services**: `gitea` (app + git hosting + web UI), `postgres` (Gitea's DB backend — more robust than SQLite for concurrent Actions runs), `act_runner` (Gitea Actions runner, registered against the Gitea instance).
- **Storage**: all persistent state (repos, DB, LFS, config) on local Docker volumes / bind mounts — no live component points at cloud storage.
- **Backup**: scheduled `gitea dump` (consistent snapshot of repos + DB + config) → encrypted, versioned sync to cloud object storage via `restic` (preferred) or `rclone`.

## Phases

### Phase 0 — Prerequisites
- Confirm Docker + docker-compose (or `docker compose`) available locally.
- Decide host storage path for volumes (e.g. `~/gitea-data`), sized for expected repo volume.
- Decide cloud backup target: S3-compatible bucket (AWS S3, Backblaze B2, or similar). Backblaze B2 is the cheapest common choice for cold backup storage.

### Phase 1 — Deploy Gitea + Actions runner
- Write `docker-compose.yml` with `gitea`, `postgres`, `act_runner` services.
- Bring stack up, complete Gitea's first-run install wizard, create admin account.
- Enable Actions in `app.ini` (`[actions] ENABLED = true`).
- Register `act_runner` against the instance using a runner registration token from Gitea admin settings.

### Phase 2 — Base configuration
- Create an organization to hold projects (mirrors a GitHub "personal org" structure).
- Add SSH key(s) for git push/pull over SSH.
- Set up branch protection / PR review defaults as desired.
- Smoke-test: create a throwaway repo, push a commit, confirm it's reachable via clone/SSH.

### Phase 3 — Migrate or (re)create projects
- For each existing GitHub project to migrate: use Gitea's built-in "Migrate Repository" (supports pulling from GitHub directly, including issues/PRs/labels if desired) or a plain `git push --mirror` for code-only migration.
- Update local clones' remotes to point at the new Gitea instance.
- Decide, per repo, whether GitHub stays as a read-only mirror/backup or is fully retired.

### Phase 4 — CI workflows
- Add `.gitea/workflows/*.yml` (same syntax as `.github/workflows/*.yml`) to a pilot repo.
- Validate a moderately complex pipeline end-to-end: multi-step build → test → artifact upload, using the registered `act_runner`.
- Document any Actions feature gaps encountered vs. GitHub Actions (marketplace action compatibility varies).

### Phase 5 — Backup & disaster recovery
- Write a backup script: run `gitea dump` inside the container, then push the resulting archive off-box with `restic` (encrypted, incremental, retention policy) to the chosen bucket.
- Schedule via cron or systemd timer (e.g. nightly).
- Store the restic repository password / cloud credentials in a password manager — losing these makes backups unrecoverable.

### Phase 6 — Validate restore
- On a separate machine or a scratch VM, restore from the latest backup (`restic restore` → `gitea restore` from dump) and confirm a full working instance comes back up with repos and CI history intact.
- This step is not optional — an untested backup is not a backup.

## Open Decisions

- Which cloud object storage provider/bucket for backups (cost vs. familiarity).
- Backup frequency and retention window (e.g. nightly, keep 30 days + weekly for a year).
- Whether GitHub repos are fully retired or kept as a passive mirror during a transition period.
- Resource sizing (CPU/RAM) for running Actions jobs locally alongside other work on the same machine.

## Success Criteria

- All active personal projects hosted and pushable/clonable from the local Gitea instance.
- At least one pipeline exercising a moderately complex scenario (multi-stage build/test, matrix or dependent jobs) runs green via `act_runner`.
- A restore drill from cloud backup succeeds on a separate machine.
- Zero dependency on cloud connectivity for normal day-to-day git/CI operations.

## Out of Scope (for now)

- Kubernetes/GitOps deployment tooling (ArgoCD) — revisit only if a scenario specifically requires k8s.
- Jenkins — revisit only if a scenario needs a plugin/integration Gitea Actions can't cover.
- Multi-user / team access controls beyond what's needed for a single-operator experimental setup.
