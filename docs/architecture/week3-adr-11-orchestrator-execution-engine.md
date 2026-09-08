# ADR-11: Orchestrator Execution Engine — Control-Plane/Launcher Split, Step-Executor Registry, and Implementation Language

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-09-08 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Developer Experience |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-1](week1-adr-1-agent-execution-model.md) (execution model), [ADR-2](week1-adr-2-agent-identity-and-permission-model.md) (identity), [ADR-3](week1-adr-3-policy-enforcement-layer.md) (policy/PDP), [ADR-5](week1-adr-5-observability-and-audit-trail.md) (audit trail), [ADR-7](week1-adr-7-multi-agent-review-workflow.md) (fixed core loop and declarative stage-list config — extended here, not reopened), [ADR-9](week2-adr-9-orchestrator-foundations.md) (config storage/bundle-build mechanism reused; `ContainerLauncher` test-seam interface becomes a real privilege boundary here), and [ADR-10](week2-adr-10-microvm-launch-and-lifecycle-mechanics.md) (resolves its open question of where the jailer-requires-root constraint sits in the orchestrator's own deployment model) |

## Context

ADR-1 decided the orchestrator is bespoke infrastructure and deliberately deferred its internal design. ADR-9 answered three foundational questions (config storage, human-approval mechanism, testable interface boundaries) but explicitly left open how the orchestrator's own codebase is structured and deployed — not a gap in ADR-9, just outside its scope. ADR-10 then surfaced a concrete, unresolved consequence of that gap: `jailer` (the production `ContainerLauncher` implementation) must run as root to perform its chroot/cgroup/uid-drop setup before exec'ing Firecracker, and "where the jailer-requires-root constraint sits in the orchestrator's own deployment/privilege model" was named there as needing an answer before `ContainerLauncher`'s production implementation is written.

Separately, requirements gathering for the orchestrator surfaced two more open points ADR-7 didn't need to answer at the time: how to keep the orchestrator's own code extensible for new kinds of steps (not just new *stage-list entries* within the shape ADR-7 already fixed) without reopening ADR-7's fixed core loop, and how to resolve, per task, which of a repo's configured steps actually apply — since ADR-7's stage entries carry conditions (risk tier, files touched) evaluated against task-specific facts, not just static config. Neither of these requires revisiting ADR-7's decision that the core loop (`generate → fan out → aggregate → evaluate → continue/stop`) is fixed, non-configurable orchestrator code, or its round cap (fixed at 5, a safety valve, not a tuning knob) — both are treated here as settled and unchanged.

This ADR closes all of these: the orchestrator's process architecture (given the launcher's privilege requirement), how step execution is structured for extensibility, how per-task step applicability is resolved, and the implementation language.

## Decision Drivers

- **Least privilege for the root-capable launch path** (ADR-10's open item) — a root requirement in one narrow capability must not become a root requirement for the whole orchestrator, including its REST API and UI surface.
- **Avoiding a solved problem's redundant solution** — the per-invocation microVM boundary (ADR-1/ADR-4/ADR-10) already provides isolation; the orchestrator's own process architecture doesn't need to re-solve that at a second layer.
- **Extensibility without reopening fixed decisions** — new step types should be addable without touching ADR-7's core loop, but this is not a mandate to make the loop itself data-driven.
- **Auditability** — "why did or didn't step X run for this task" must be answerable from one place (ADR-5), not reconstructed by cross-referencing orchestrator code against policy data.
- **Operational simplicity proportional to actual need** — governance/tooling-vetting surface (ADR-0's MCP/tool-governance driver, applied here to language/toolchain choice) sized for a solo pilot, not speculative future scale.
- **Ecosystem fit with already-decided infrastructure** — OPA, Forgejo, Firecracker/jailer, Postgres, Prometheus.

## Options Considered

### A. Orchestrator process architecture

**A1. Single monolithic process, root throughout.** Rejected — the REST API and UI-facing surface would run with root privilege solely because one narrow internal capability (jailer invocation) needs it, which is a much larger attack surface than the actual requirement justifies.

**A2. A fully isolated process per task/request.** Rejected — the isolation problem this would solve (containing one task's blast radius from another) is already solved at the microVM layer (ADR-1). Adding process-per-request isolation on top is redundant and reintroduces the state-scatter problem ADR-5's centralized audit trail and ADR-3's centralized quota enforcement were designed to avoid.

**A3. Two-component split: an unprivileged control-plane plus a privileged launcher, communicating over a narrow local interface — chosen.** Detailed below.

### B. Step execution extensibility

**B1. Hardcode each step type's handling inline in the core loop.** Rejected — every new step type would require editing shared loop/aggregation code, recreating the exact problem ADR-7 already rejected (as its option 1) for stage additions generally.

**B2. Event-driven plugin/self-registration** (stages subscribe to an event and self-register, no fixed list). Rejected, for the same reasons ADR-7 already rejected this as its option 2: it solves a dynamism/scaling problem this pilot doesn't have, at the cost of real added complexity (pub/sub or registry infrastructure, harder-to-reason-about ordering). Not reopening that decision here.

**B3. A closed-set `StepExecutor` registry, one implementation per step `type`, selected by a type string from the existing declarative step-list config — chosen.** Detailed below.

### C. Per-task step applicability resolution

**C1. Orchestrator evaluates step conditions itself, in plain code, against cached bundle data.** Considered, rejected as the sole mechanism — "did the mandatory step run, and why" is exactly the kind of question ADR-5's audit trail exists to answer, and splitting that evaluation from the PDP call would leave it unanswerable from one audited source.

**C2. A hybrid split** — mandatory/global-baseline conditions resolved via the PDP, optional/repo-level conditions resolved as plain data filtering. Considered, rejected for now — adds a second resolution path and a "which condition type is which" classification burden for a distinction that hasn't yet shown to matter in practice.

**C3. Fold per-task step-applicability resolution into the existing PDP call — chosen.** Detailed below.

### D. Implementation language

**D1. Python.** Rejected — weaker fit for the launcher's low-level surface (jailer process management, `AF_VSOCK`, nftables, cgroup v2 tend to mean `ctypes`/subprocess plumbing rather than idiomatic libraries); packaging a root-capable interpreter process is a messier security surface than a static binary.

**D2. Java.** Rejected — JVM startup/memory footprint is a poor fit for a lean, root-capable launcher; low-level systems programming is more awkward (JNI or shelling out) than Go's near-syscall-level stdlib; weaker ecosystem alignment with the already-decided, largely Go-native infrastructure (OPA, Forgejo).

**D3. Rust.** Considered seriously, not chosen for v1. A real memory-safety argument exists specifically for the launcher (root-capable, handles the MMDS credential handshake directly) — genuinely stronger than Go's for that one component. Not adopted because: it would mean two languages/toolchains for a solo pilot (doubling the dependency-vetting surface ADR-0's tool-governance driver already cares about), a real development-velocity cost (borrow-checker friction, compile times) with no demonstrated incident motivating it, and a direct counter-precedent — `runc`/`containerd`'s shim, which plays exactly this "privileged, root-capable launcher of sandboxed workloads" role at much larger scale, are Go, not Rust. Logged as a documented future hardening option for the launcher specifically, revisited only if that component's threat model or a real incident motivates it — the same "log it, don't build it" treatment ADR-1 gave resumable workspaces and ADR-7 gave the event-driven plugin registry.

**D4. Go, single language across control-plane and launcher — chosen.** Detailed below.

## Decision

### A. Process architecture: unprivileged control-plane + privileged launcher

- **Control-plane** (unprivileged): REST API, UI-serving, PDP calls, audit writes (ADR-5's Postgres table), the task/round state machine, and `StepExecutor` dispatch. Runs as a single multi-tenant, logically stateless service — state lives in Postgres, not in-process — not a process per task or per request.
- **Launcher** (privileged, root, per ADR-10's jailer requirement): exposes a narrow local interface — launch / kill / extract-logs — over a local channel (unix domain socket or loopback gRPC; exact mechanism an open question below). This is `ContainerLauncher`'s production implementation (ADR-9). The interface boundary ADR-9 originally built purely as a unit-test seam now does double duty as the actual privilege boundary between an unprivileged, internet/UI-facing process and a root-capable one.
- Horizontal scaling of the control-plane (multiple instances claiming tasks off a shared table/queue) is not needed for the pilot (ADR-0's solo-pilot scope) but isn't foreclosed by this design — the same "documented, deferred, not built" treatment used elsewhere in this series (ADR-1's resumable workspaces, ADR-7's plugin registry).

### B. Step execution: ADR-7's core loop unchanged, extended with a `StepExecutor` registry

- ADR-7's fixed core loop (`generate → fan out → aggregate → evaluate → continue / stop-success / stop-escalate`) and its round cap (fixed at 5, in core-loop code, never configurable) are unchanged by this ADR.
- ADR-7's declarative stage-list config (`.harness/stages.yaml`, stored and versioned per ADR-9's mechanism) gains a `type` field on each entry: `agent-stage` (today's review/security-triage shape) or `deterministic-step` (lint, test, deploy, security-scan). Same file, same bundle-build/narrow-only mechanism — no new config-versioning system introduced.
- The global baseline defines a mandatory minimum step set (e.g., independent test re-verification, security-triage) that per-repo override can only extend, never remove — narrow-only applied to step selection symmetrically with how ADR-3/ADR-9 already apply it to policy.
- The core loop's "fan out" stage becomes generic over step type: for each step in the resolved list, dispatch to a `StepExecutor` looked up by `type`. Adding a new step type (e.g. a future `approval-gate` or `notify` type) means implementing and registering one new executor — never modifying the loop, aggregation, or other executors. This is closed-set, compile-time/deploy-time extensibility, deliberately not the event-driven self-registration ADR-7 already declined (option B2 above).
- **Unified execution primitive**: every `StepExecutor`, agent or deterministic, executes via `ContainerLauncher` — mint a credential (possibly zero- or read-only-scope for a step like linting), launch a sandboxed container/microVM, run the step's payload (an agent CLI + prompt, or a fixed command), extract logs via vsock, tear down. No step type gets a separate "run directly on the control-plane or launcher host" path. This keeps ADR-0's "% of actions passing through a policy gate" metric meaningful for deterministic steps too, and keeps repo-declared step commands — a real code-execution surface, the same trust level as a Makefile or CI config already in the repo — inside the sandbox boundary ADR-4/ADR-10 built, never running as a host process.

### C. Per-task step applicability: resolved by the existing PDP call

- Config resolution (merging global baseline + repo override, narrow-only validation) stays fully static, done once at bundle-build time per ADR-9 — unchanged, and not per-request.
- Per-task condition evaluation (does this task's risk tier / files-touched satisfy a given step's `conditions`) is folded into the PDP call the orchestrator already makes before minting a credential (ADR-3). The PDP response gains a `resolved_steps` field (step ids applicable to this specific task, already condition-matched against the narrow-only-merged config) alongside the existing `{decision, granted_scopes, reason, policy_bundle_version}`. One call, one audited answer to "what should happen and why" for this task.
- Fail-closed applies here as it already does for every other PDP-gated decision (ADR-3): if the PDP is unreachable, no steps are resolved and no invocation proceeds. Not a new hard dependency — the same one ADR-3 already accepted.

### D. Implementation language: Go, for both control-plane and launcher

- Ecosystem alignment with already-decided infrastructure (OPA, Forgejo/Gitea lineage, Firecracker tooling) — mature or first-party client libraries rather than community ports.
- Systems-level fit for the launcher (`AF_VSOCK`, nftables, cgroup v2, jailer process management) via near-syscall-level stdlib support, without JNI/`ctypes`-style indirection.
- A single static binary minimizes the attack surface of the root-capable launcher specifically — no interpreter/runtime/dependency tree to patch on a process running as root.
- Goroutines are a natural fit for ADR-7's parallel stage fan-out and for orchestrating many concurrent tasks, without GIL constraints or JVM thread weight.
- Direct precedent: `runc`/`containerd`, playing the same "privileged, root-capable launcher of sandboxed workloads" role at much larger scale, are Go — evidence this is a proven shape for exactly this component, not a novel risk.
- One language across both components keeps the tool-governance/dependency-vetting surface (ADR-0) proportional to a solo pilot, at the cost of not getting Rust's stronger memory-safety guarantee for the launcher specifically — accepted for now, logged in Consequences as the first thing to revisit if that component's threat model changes.

## Consequences

**Positive**
- Closes ADR-10's open question directly: the jailer-requires-root constraint sits in a dedicated, narrowly-scoped launcher component, not the whole orchestrator.
- Extends ADR-7's declarative config model to deterministic steps without reopening its fixed core loop or its narrow-only precedent — one config mechanism, one mental model, consistent with ADR-9's "reuse over parallel infrastructure" driver.
- New step types are addable by writing and registering one executor, without touching shared control-flow code — a concrete, testable answer to the extensibility requirement that opened this discussion.
- Unifying the execution primitive keeps ADR-0's policy-gate coverage metric and the sandbox boundary meaningful for deterministic steps, not just agent steps.
- "Why did or didn't step X run for this task" is answerable from one audited decision record, not reconstructed by cross-referencing two independent code paths.
- A single-language toolchain keeps governance/vetting surface proportionate to a solo pilot while still closing the privilege-separation gap ADR-10 flagged.

**Negative / trade-offs**
- The control-plane/launcher split is new operational surface relative to a single process: two deployables, and a local IPC boundary that itself needs securing — a compromised control-plane process should not be able to freely command a root-capable launcher, so this boundary needs real authentication, not just network locality.
- Folding step-applicability into the PDP call mixes concerns (authorization vs. pipeline shape) in one response schema — accepted for auditability, but worth revisiting (per option C2) if it measurably affects PDP call latency or Rego complexity.
- The `StepExecutor` registry is closed and compiled-in — a genuinely new step type still requires an orchestrator code change and redeploy, unlike stage-list entries, which are pure config. A deliberately smaller form of extensibility than a full plugin system, not an oversight.
- Not adopting Rust for the launcher leaves its credential-handling code without Rust's memory-safety guarantee; accepted as proportionate now, flagged as the first thing to revisit if the launcher's threat model changes.
- Deterministic step commands remain repo-declared, version-controlled code the orchestrator will execute inside the sandbox — the same trust level as an existing CI config, but worth naming explicitly as a real, bounded execution surface rather than overlooking it.

## Open Questions

- Local IPC mechanism and authentication between control-plane and launcher (unix domain socket with filesystem permissions vs. loopback gRPC with mTLS) — needs pinning before the launcher is built, since it's now a real privilege boundary, not just a code interface.
- JSON Schema for the `deterministic-step` entry type (command, working directory, timeout, expected exit codes) — extends ADR-9's already-open schema/validation question to the new entry type.
- Exact minimal credential scope per deterministic step type (does a lint step need any git token at all, versus a read-only clone) — needs a mapping decision similar to ADR-2's bot-account-pool sizing question.
- Whether `resolved_steps` needs its own explicit field in the audit record (ADR-5) alongside `decision`/`granted_scopes` — likely yes, not finalized here.
- Whether the launcher should ever be reachable by more than one control-plane instance — only relevant if/when control-plane horizontal scaling is pursued; not needed for the pilot.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- [ADR-2: Agent Identity & Permission Model](week1-adr-2-agent-identity-and-permission-model.md)
- [ADR-3: Authorization / Policy Enforcement Layer](week1-adr-3-policy-enforcement-layer.md)
- [ADR-5: Observability & Audit Trail](week1-adr-5-observability-and-audit-trail.md)
- [ADR-7: Multi-Agent Review & Iteration Workflow](week1-adr-7-multi-agent-review-workflow.md)
- [ADR-9: Orchestrator Foundations](week2-adr-9-orchestrator-foundations.md)
- [ADR-10: MicroVM Launch & Lifecycle Mechanics](week2-adr-10-microvm-launch-and-lifecycle-mechanics.md)
- `runc` and `containerd` — reference precedent for a privileged, root-capable sandbox launcher implemented in Go — https://github.com/opencontainers/runc, https://github.com/containerd/containerd
