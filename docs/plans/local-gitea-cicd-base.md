# Plan: Local Gitea + Actions — Base Setup for Agentic Dev Platform

| | |
|---|---|
| **Status** | Draft |
| **Date** | 2026-08-21 |
| **Owner** | Krzysztof Niemiec |
| **Purpose** | Get a local Gitea instance with working CI (Actions) running, as the minimal foundation to experiment with agentic software development. No project migration, no backup — just the working base. |

## Goal

A local git server with CI that an agent can push code to and see a pipeline run, on this machine, with nothing else layered on top yet.

## Steps

### 1. Prerequisites
- Docker + docker-compose available locally.
- Pick a local folder for persistent data (e.g. `~/gitea-data`).

### 2. Deploy Gitea + Actions runner
- Write `docker-compose.yml` with two services: `gitea` (app + Postgres or SQLite backend) and `act_runner` (Gitea Actions runner).
- Bring the stack up (`docker compose up -d`).

### 3. Initial configuration
- Complete Gitea's first-run install wizard, create the admin account.
- Enable Actions (`[actions] ENABLED = true` in `app.ini`, or via install wizard if offered).
- Generate a runner registration token in Gitea admin settings and register `act_runner` against the instance.

### 4. Smoke test
- Create one test repo.
- Push a commit with a minimal `.gitea/workflows/test.yml` (e.g. checkout + echo/build step).
- Confirm the runner picks up the job and it completes successfully.

## Success Criteria

- Gitea reachable locally (web UI + git clone/push over SSH or HTTP).
- A pushed workflow file triggers a run on `act_runner` and completes green.

## Explicitly Not in This Plan

- Migrating existing GitHub repos.
- Backup / disaster recovery.
- Multi-repo org structure, branch protection, or access-control setup.
