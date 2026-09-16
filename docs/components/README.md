# Component documentation

This directory holds **living, current-state developer documentation** — one file per deployable component in this repo (a running service or a standalone CLI). It exists for developers, and agents, who are about to *work on* or *use* a component and need to build an accurate mental model fast.

## How this differs from `docs/designs/` and `docs/architecture/`

This repo's pipeline is **intent → design → implementation → documentation**:

| Stage | Lives in | Answers | Frozen once written? |
|---|---|---|---|
| Intent | `docs/intents/` | What problem are we solving? | Yes |
| Design | `docs/designs/` (`next/` = not yet implemented) | How should it work? What are the tradeoffs? | Yes, as a historical record of the decision — superseded designs stay for context |
| Architecture decisions | `docs/architecture/` (ADRs) | Why did we choose X over Y? | Yes |
| **Documentation** | **`docs/components/`** | **What does the code actually do, right now?** | **No — updated in the same PR as the change that makes it stale** |

A design doc describes what a change is *supposed to* become and stays as-written even after landing (it's the historical record of that decision). A file in this directory describes what's *actually true* today. The two will drift, overlap, and sometimes disagree on emphasis — that's expected, not a bug to reconcile. When they conflict on a fact, trust `docs/components/`; when you want the *why*, follow its "Design docs" link back to intent/design.

## Convention

Keep each component file concise and developer-facing, not exhaustive. Prefer a table or diagram over prose where one fits. Suggested section order (skip any that don't apply):

1. **What it is** — one paragraph
2. **Modularity** — package/module map, how the pieces fit together
3. **API / contracts** — what it exposes and what it consumes
4. **Configuration** — env vars, config files
5. **Running it** — this component's own build/run/config, standalone
6. **Testing** — how, and what each layer proves
7. **Known gaps** — deliberately unimplemented, with a link to the design doc that will close the gap

**A component's own architecture is described assuming its real dependencies exist** — describe what it depends on by contract (e.g. "a Policy Decision Point"), not by whatever locally stands in for that dependency today. Local stand-ins used only to make a component testable (fixture-driven services, static credential pools, etc.) aren't part of that component's architecture, and don't belong in its file — document *them* in their own file when they're substantial enough to need one, and use [`docs/local-development.md`](../local-development.md) for anything that's really about assembling a local stack (which service stands in for what, ports, bring-up/teardown).

## Components

| File | Covers |
|---|---|
| [`orchestrator.md`](orchestrator.md) | `cmd/control-plane` and the `internal/*` packages it's built from |

More to come as they're built out: `pdp-mock`, `launcher-mock`, `secrets-mock` (all under `mocks/` — local stand-ins for the orchestrator's Policy Decision Point, step launcher, and secrets/credential issuer dependencies), `seed` (`cmd/seed`).

See also [`docs/local-development.md`](../local-development.md) — the cross-cutting "bring up a full local stack" guide, not a component doc.
