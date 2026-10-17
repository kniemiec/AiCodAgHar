---
name: wf-implement
description: This skill should be used when the user runs `/wf-implement`, asks to "implement the design", "implement PR C of the X design", "build section N of the design", or otherwise wants code written from a design doc under docs/designs/. It takes a short goal (the whole design, one delivery slice such as a PR from the per-PR checklist, or a named section), checks the slice is ready to build, implements it with the tests the design names, verifies it with the repo's build/test/lint commands, updates the living component docs and records every deviation in an "As built" entry in the design doc.
---

# Workflow: Design Doc → Implementation

Turn a design doc (usually written by `/wf-design`) into working, tested code — either all of it or one slice of it. The deliverable is the code change itself plus the doc updates that keep the repo consistent. This workflow doesn't go through plan mode / `ExitPlanMode`: the design already is the plan.

Pipeline position: `wf-intent` → `wf-design` → `wf-implement` (this).

## Input

The text after `/wf-implement` is a short goal. Typical forms:
- `PR C of the observability design` / `next PR` — one row of the design's Delivery table or per-PR checklist.
- `section 7 of docs/designs/active/foo-design.md` — one numbered Design section.
- `the retry design` — everything in the design.

If invoked with no text, ask directly (plain text) what to implement.

## Step 1 — Resolve the design and the slice

1. **Find the design doc.** Use the path if one is given. Otherwise match the goal against titles in `docs/designs/active/` (and `completed/` if the user mentions a finished design). Ask only if two or more docs match equally well.
2. **Pin the slice.** Map the goal to concrete content in the doc:
   - A delivery slice (PR letter, phase): its Delivery row (content + requirement IDs), its per-PR checklist entry (*Files*, *Tests*, *Done when*), and the success criteria tagged with that slice.
   - A section: that Design section, plus any Testing Strategy items and success criteria that cite it.
   - The whole design: every slice, done in the doc's delivery order.
   - `next`: the first slice that has no "As built in …" entry and no matching commit.
3. **Write down the scope** in one short paragraph for yourself: the requirement IDs, the files and tests in scope, and what "done" means. Everything outside it (later slices, "Could" requirements the design defers, Out of Scope items) stays out. Don't build ahead into the next slice, even when it's tempting.

## Step 2 — Check the slice is ready

Don't start coding until each of these is true. If one fails, stop and report what is missing instead of working around it:
- **Earlier slices are in.** For each slice this one depends on, look for its commit (`git log --oneline`) and its "As built in …" entry in the design. Read those entries: they record how the earlier slices actually came out, and they override the design's original signatures.
- **No open owner decisions block it.** Check the design's Owner decisions / Open questions and Named Risks. A decision the slice needs that is still marked open is the user's call; ask with `AskUserQuestion` (recommended option first). Routine choices are not owner decisions; make them and record them (Step 6).
- **The working tree is safe to change.** Run `git status`. If there are uncommitted changes the slice would touch that aren't yours, ask before mixing them in. Note the branch; don't create or switch branches unless asked.

## Step 3 — Ground yourself in the current code

The design was true when it was written; the code may have moved since. Before editing:
- Read the living guides for the components touched (`docs/components/orchestrator.md`, `docs/components/container-launcher.md`) and the "Read the matching doc" rules in CLAUDE.md.
- Open every file the slice lists, and check each `path:line` claim the slice relies on. When the code no longer matches the design, follow the code and note the difference for the As-built entry.
- Find the existing patterns to copy: the fakes in `internal/control-plane/fakes/`, test helpers, how errors are classed and wrapped, how env config is read, how logging is called. New code should look like it was written by the same hand.
- For a wide search, use an Explore agent or a fork and keep only the conclusions in the main context.

## Step 4 — Implement in buildable steps

Work in an order where the tree compiles after each step, usually: contracts first (proto + `make proto-gen`, interfaces, domain types), then the implementations and fakes, then the wiring in `cmd/*/main.go`, then tests. Rules:
- **Keep commands small.** Run one build or test command at a time, not long compound shell scripts. Edit with the Edit/Write tools, not sed/python rewrites; a scripted rewrite that misses a case costs more than it saves.
- **Build often.** Run `go build ./...` (or `make build`) after each step, and `go vet` with the relevant build tags once tagged test files change.
- **Contracts stay compatible.** Proto changes are additive (new field numbers, new enum values with an `UNSPECIFIED` zero). Old clients and old servers must keep working, and the mocks in `mocks/` must speak the new contract too. Generated code in `api/gen/go` is checked in.
- **New dependencies.** Pin exact versions only, from one release train when the modules belong together, each at least two weeks old. Check that the version actually exists before you use it (for example, a semconv package version inside the pinned SDK), then run `go mod tidy`. Record every version and every transitive bump (such as a grpc upgrade) in the design's dependency section or As-built entry.
- **No secrets anywhere new.** Never put credentials, task descriptions or agent output into new log lines, event payloads, metric labels or error text. Use the existing sensitive-record and redaction paths.
- **Don't widen scope quietly.** If you find a real bug outside the slice, fix it only if it's small and blocks the slice, and say so in the hand-back. Otherwise list it as a follow-up.

## Step 5 — Write the tests the design names

Implement each test the slice's *Tests* entry and Testing Strategy items name, in the files they name, with the existing build-tag split (unit on fakes with no tag; `integration`, `e2e`, `launcherkvm` as in CLAUDE.md). Each success criterion tagged with this slice needs a test, or a stated reason why it can only be checked by hand.

Make tests prove something:
- **Negative tests need a control.** A "secret never appears in X" test must also check that the secret reached the system and that X received ordinary records, and must fail with a "the test proves nothing" message if either is missing.
- **Table tests cover every row** of the design table they test (outcomes, failure classes, status mappings).
- **Timing or failure-isolation tests** (for example a dead Collector or an unreachable endpoint) compare against a baseline run, not a fixed number.
- Tests that hang are usually input rejected before reaching the code under test; check validation paths before adding timeouts.

## Step 6 — Update the docs in the same change

- **Living guides** (`docs/components/*.md`): update every section the slice makes stale, including new env vars, endpoints, metrics, error/outcome semantics, shutdown behaviour and the test list. CLAUDE.md requires this in the same change.
- **The design doc**: add an `**As built in <slice>**` entry (next to any earlier ones, typically in the interface/consolidated-reference section) listing each difference from the design — renamed or reshaped signatures, pinned versions, decisions taken where the design was silent, what was deliberately not built, and why. Keep entries factual and narrowing/additive in tone; don't rewrite the original design text.
- When the **whole design** is now implemented, append a dated entry to its `## Results` section. Don't move the doc to `docs/designs/completed/`; the author does that.
- Never edit ADRs (`docs/architecture/`), intent docs (`docs/intents/`) or completed designs; they are frozen records. Update `CLAUDE.md` only if a command or gotcha it documents has changed.

## Step 7 — Verify

Run, one at a time, and fix what fails:
1. `make build` and `make test`.
2. `go vet ./...`, plus `go vet -tags <tag> ./...` for every build tag whose files you touched (these don't compile under plain `make test`).
3. `buf lint` (or `make proto-gen`) if the proto changed.
4. `go test -race` on packages where you added goroutines, callbacks, shared state or atomics.
5. `gofmt -l` on the files you touched (not the whole repo; pre-existing issues are not yours to fix).
6. `make test-integration` / `make test-e2e` when the slice has tests with those tags **and** the local stack is up. Rebuild the changed images first (`docker compose -f deploy/docker-compose.yml build <svc>`, then `up -d`, then `make seed`), and use `-count=1`. If Docker or the stack isn't available, don't start it unasked; report these suites as not run.

Then clean up: remove stray binaries or scratch files in the repo root (`go build` without `-o` drops binaries there) and check `git status` shows only intended changes.

## Step 8 — Hand back

Don't commit, push or open a PR unless the user asks. Report concisely:
- **What was built**, grouped by the slice's requirement IDs or areas, a short paragraph each.
- **Decisions worth checking**: deviations from the design, version pins and transitive bumps, choices made where the design was silent, behaviour changes users will notice, and anything deliberately not built.
- **Verification**: exactly what passed, and what was not run and why. Never claim a suite passed that you didn't run.
- **Before merge**: the concrete steps still needed (e.g. image rebuilds and integration runs), and what the next slice depends on from this one.

Don't paste diffs or whole files into the chat.

## Scope notes

- Implements exactly the requested slice; later slices, deferred "Could" items and Out of Scope items stay unbuilt and are named as such.
- Writes source, tests, generated code, `go.mod`/`go.sum`, living component docs, and the design doc's As-built/Results entries. It doesn't touch ADRs, intents, completed designs or design statuses.
- If the design turns out to be wrong in a way that changes its shape (not just a signature), stop and report it rather than redesigning silently; suggest re-running `/wf-design` on that part.
