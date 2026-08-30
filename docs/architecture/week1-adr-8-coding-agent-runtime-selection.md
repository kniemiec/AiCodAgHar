# ADR-8: Coding-Agent Runtime Selection

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-27 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Developer Experience |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-0](week1-adr-0-and-capability-map.md); binds the "headless coding-agent CLI" [ADR-1](week1-adr-1-agent-execution-model.md) assumes runs inside each container; privilege separation between roles is enforced by [ADR-2](week1-adr-2-agent-identity-and-permission-model.md)'s bot-account/credential scoping; targets [ADR-6](week1-adr-6-git-hosting-platform-selection.md)'s Forgejo instance; decided after and directly shaped by [ADR-7](week1-adr-7-multi-agent-review-workflow.md), which defines the three agent roles and the cross-model independence requirement this ADR satisfies |

## Context

ADR-1 deferred naming a concrete coding-agent runtime to this ADR. Originally that looked like a single question — which CLI/framework runs inside the agent container — evaluated against ADR-1's one-shot execution model. ADR-7 was deliberately decided first because it changes the question: instead of one role (coding), there are now three — **coding** (write access, clone/branch/edit/test/open-or-update-PR), **review** (read + comment-only, code-quality focus), and **security-triage** (read + comment-only, consumes deterministic-scanner output rather than reviewing a diff cold) — each its own invocation under ADR-1's mechanics, with ADR-2 enforcing the privilege boundary between them via distinct bot accounts. ADR-7 additionally requires that review and security-triage run independent of the coding role — ideally on a different underlying model — to avoid the self-attribution bias documented there (a model reviewing code adjacent to how it just reasoned about writing it tends to miss what it just got wrong).

**Candidates surveyed.** Aider (interactive pair-programming tool, never redesigned for autonomous one-shot runs; release cadence has slowed as the market moved agentic) and SWE-agent (headless-capable and purpose-built for autonomous issue resolution, but narrower ecosystem than OpenHands for the same use case) were both considered and set aside. Vendor-proprietary CLIs (Claude Code, Codex CLI, Gemini CLI, and similar) were excluded on structural grounds: each ties the runtime choice to one model vendor, which would quietly reopen the model-selection question ADR-0 explicitly scoped out of this initiative — noted with the disclosure that Claude Code is the tool this entire ADR series has been authored with, a direct conflict of interest worth naming rather than letting bias the comparison silently. That left three self-hostable, model-agnostic candidates: **OpenCode**, **OpenHands**, and **Goose** — the same three compared for the (then single) coding role before ADR-7.

**The key thing ADR-7 changes about this comparison:** all three candidates are model-agnostic runtimes. ADR-7's independence requirement is about the *model*, not the *framework* — the bias research it cites is about a model reviewing its own reasoning trace, not about which harness orchestrates that model. That decouples "which framework" from "which model backs each role," meaning cross-model independence is achievable through configuration alone, without needing a different tool per role. Given that, the comparison collapses back to a single question: which one runtime best serves the union of what all three roles need, given the coding role is still by far the heaviest requirement.

**Verified specifics on OpenHands** (checked directly rather than assumed, given how fast this space moves): its git-provider integration merged native **Forgejo** support in December 2025 (PR #11111, closing issue #9354) — covering repository access, branch management, and pull-request operations through the same abstraction layer as its GitHub/GitLab clients, authenticated via an extension of its PAT system. Its headless mode is invoked directly with a task prompt (`openhands --headless -t "..."` or `-f task.txt`) — the GitHub-issue-label "Resolver" product is built on top of headless mode, not the only way to reach it, so it fits ADR-1's orchestrator-hands-a-prompt-to-a-container shape directly. Its runtime is pluggable: the default builds and launches its own nested Docker container (an "action execution server" behind a REST API); `LocalRuntime` instead runs that same action-execution server directly on the host process, avoiding the nested-sandbox conflict with ADR-1's already-sandboxed container.

## Decision Drivers

- **Capability fit for the coding role** — still the heaviest bar: native, robust create-or-update-PR handling against Forgejo, matching ADR-7's round-based loop.
- **Operational simplicity** — one tool to install, patch, and vet (per ADR-0's MCP/tool-governance driver) rather than three.
- **Sandbox architecture fit** — must run inside ADR-1's orchestrator-owned container without nesting a second Docker layer.
- **Cross-model independence achievable via configuration** (ADR-7) — a driver *for* consolidating on one runtime, not against it, given the model-agnostic decoupling above.
- **Minimizing additional token-handling surface** — avoid routing the ADR-2-scoped git credential through an extra third-party component when a first-party-maintained client already exists, given ADR-6's CVE deep-dive showed exactly this class of surface (token/scope handling) is where real vulnerabilities concentrate.
- **Model selection stays out of scope** (ADR-0) — the runtime choice must not implicitly lock in a model vendor.
- **Governance/stewardship risk** — a driver in general (per the lens already applied to Gitea vs. Forgejo), acknowledged here but not decisive — see Consequences.

## Options Considered

**1. Aider.** Rejected — wrong operating paradigm for ADR-1's autonomous one-shot model; stayed a pair-programming tool as the market went agentic.

**2. SWE-agent.** Rejected for this decision — headless-capable and purpose-built for autonomous issue resolution, but a narrower ecosystem than OpenHands for the same pattern today. Kept in reserve as a fallback, not carried forward.

**3. Vendor-proprietary CLIs** (Claude Code, Codex CLI, Gemini CLI, etc.). Rejected — each entangles the runtime decision with a specific model vendor, reopening ADR-0's out-of-scope model-selection question.

**4. Goose.** Meets the review/security-triage roles' requirements well — provider-agnostic, Apache 2.0, governed by the Linux Foundation's Agentic AI Foundation (the strongest governance signal of any candidate evaluated across this whole ADR series), and its calling card — 70+ MCP tool extensions — lines up with ADR-0's MCP-governance driver. Lacks native Forgejo repository/PR mutation support; closing that gap for the coding role would mean wiring in `gitea-mcp`, a third-party MCP server handling the scoped git token — an extra, less-scrutinized hop the ADR-6 CVE research argues against introducing without cause. Not chosen as the primary runtime; logged as the leading candidate if per-role framework diversification is ever pursued (see Open Questions).

**5. OpenCode.** Meets the review/security-triage roles' requirements well — MIT-licensed, model-agnostic across 75+ providers, by far the largest community of the three, and no sandbox-nesting friction (runs as a plain process). Same gap as Goose on native Forgejo PR mutation for the coding role. Same fallback status as Goose.

**6. OpenHands — chosen.** Native, verified, merged Forgejo integration; purpose-built create-or-update-PR handling that matches ADR-7's round-based loop directly; headless mode already fits ADR-1's invocation shape without relying on its issue-label-triggered Resolver product; `LocalRuntime` resolves the sandbox-nesting conflict with ADR-1's container without giving up capability. General enough as an autonomous SWE agent to serve the lighter-weight review and security-triage roles fully — those roles are a strict subset of what it already does, with write capability blocked at the ADR-2 credential layer rather than needing a different tool.

## Decision

Adopt **OpenHands** as the single coding-agent runtime for all three roles ADR-7 defines: coding, review, and security-triage.

- **Invocation**: headless mode, invoked directly by the orchestrator with a task prompt per invocation (`--headless -t/-f`), consistent with ADR-1's per-round container lifecycle — not the GitHub-Action Resolver product, which assumes a trigger mechanism (issue labels) this harness doesn't use.
- **Sandboxing**: `LocalRuntime` — the action-execution server runs directly in the container process the orchestrator already owns, rather than launching a nested Docker container. ADR-1's container remains the only sandbox boundary.
- **Git-forge interaction**: OpenHands handles clone, branch, push, and PR create-or-update against Forgejo, authenticated against the ADR-2-scoped token injected at container start — not a third-party MCP server. **Correction from the original assumption below, per PoC evidence**: in the headless SDK CLI actually exercised (v1.21.0), this happens via the agent's own general-purpose shell tool use — `git` commands plus, empirically, a hand-constructed `curl` call against Forgejo's REST API — rather than a dedicated native-provider action type invoked as a distinct tool. The functional outcome matches what was assumed (correct clone/push/PR-create/PR-update behavior, correct auth, no duplicate PRs across rounds); the mechanism is closer to "a capable general-purpose agent correctly figures out the right API calls" than a purpose-built, tested provider integration. One concrete consequence: the token flows through the visible action/observation stream as an ordinary shell variable (`${FORGEJO_TOKEN}`) rather than being kept opaque by dedicated credential-handling code — in one PoC run the agent even echoed a partial token prefix to verify the variable was set, which then persisted into the run's log. Not a blocking issue (no full token was ever exposed, and per-invocation tokens are short-lived and revocable per ADR-2/ADR-4 regardless), but worth factoring into how invocation logs are stored and redacted once a real orchestrator exists.
- **Role differentiation is credential- and model-driven, not framework-driven**:
  - *Coding*: write-scoped bot account (ADR-2); primary coding model (model choice itself remains out of scope per ADR-0).
  - *Review*: read+comment-only bot account; a backing model distinct from the coding role, satisfying ADR-7's cross-model independence requirement.
  - *Security-triage*: read+comment-only bot account; consumes SARIF from deterministic scanners as input context (ADR-7); a backing model distinct from the coding role (may share a model with the review role, since review and triage don't review each other's output).
- **Not adopted for v1**: per-role framework diversification. Goose and OpenCode remain the identified fallback candidates for review/security-triage specifically, not built now.

## Consequences

**Positive**
- The coding role — still the load-bearing case — gets a purpose-built, already-merged integration for exactly ADR-1/ADR-7's clone→edit→test→create-or-update-PR loop, verified against Forgejo rather than assumed.
- One tool across all three roles instead of three: less to install, patch, and vet under ADR-0's tool-governance driver.
- `LocalRuntime` resolves the sandbox-nesting friction identified while evaluating OpenHands, without trading away capability.
- Cross-model independence (ADR-7) is achieved through model configuration alone — no extra infrastructure needed.
- Avoids introducing a third-party MCP server as a path for the scoped git credential to flow through, reducing attack surface relative to the Goose/OpenCode-plus-`gitea-mcp` alternative for the coding role specifically.

**Negative / trade-offs**
- **Governance**: OpenHands is steered by a company (All Hands AI), not neutral foundation governance the way Goose is (Linux Foundation AAIF) — a real inconsistency against the governance-risk lens this series otherwise applied (Forgejo over Gitea). Accepted here because the concrete integration/token-surface argument outweighs the more abstract governance-risk argument for this specific decision, but worth an occasional check-in if OpenHands' stewardship trajectory shifts the way Gitea's did.
- **Single-framework/scaffold risk**: if OpenHands' own scaffold has a systematic blind spot independent of backing model, review/triage running through the same scaffold could reproduce it even with a different model. More speculative than the concretely-documented model-level self-attribution bias ADR-7 addressed — logged as an open question, not designed around now.
- Review/triage roles get access to a much larger feature surface (full autonomous coding capability) than they use; write capability is blocked at the ADR-2 credential layer, but the framework itself is more than either role strictly needs. Accepted explicitly as a reasonable trade for overall solution simplicity.
- Dependency on `LocalRuntime` specifically, a less-traveled code path than OpenHands' default Docker-based runtime. Note this is a simplicity/operational-weight trade-off, not a security one: ADR-4 wraps every invocation in a Firecracker microVM (or its documented gVisor/dev-loop equivalents), so nesting the default Docker Runtime's action-execution container inside that boundary — even if it requires `--privileged` internally, which rootless Docker-in-Docker still does — would be contained by the same hardware boundary regardless; it isn't disqualified on safety grounds, just heavier. `LocalRuntime` is preferred for being the smaller footprint, not the only safe option. **Validated in practice, 2026-08-30** — see [Results](../plans/openhands-localruntime-validation.md#results): the full coding-role loop, ADR-7's round-based PR-update behavior, a rerun as a fully non-root/capability-restricted process, clean kill-switch teardown (~1s, no host residue, no partial remote state), and a hands-on token-scope negative test all passed under `LocalRuntime` with zero privilege escalation. The Docker Runtime fallback was not needed. One nuance surfaced, not a failure: PR creation happens via general-purpose shell/curl tool use rather than a dedicated native-provider action — see the git-forge interaction note in Decision above.

## Open Questions

- ~~Validation gated on a concrete PoC~~ — **Resolved 2026-08-30.** [Plan: OpenHands LocalRuntime Validation](../plans/openhands-localruntime-validation.md) was run end-to-end: sandbox boundary, install/config, the core coding-role loop, ADR-7's round-based update-PR behavior, no-privilege-escalation (including a rerun as a fully non-root/capability-restricted process), kill-switch/teardown integrity, and a hands-on token-scope negative test all passed under `LocalRuntime`. The conditional Docker Runtime fallback (Step 8) was not triggered. Per the plan's decision tree, this confirms LocalRuntime as-is; this ADR's Status moves from Proposed to Accepted accordingly. One correction surfaced by the PoC to the Decision section's git-forge interaction claim — see that section and the plan's Results log for detail.
- Which specific models back the coding vs. review/triage roles — deferred per ADR-0's model-selection scope exclusion, but the role-to-model wiring mechanism is a small implementation decision still to make.
- Framework diversification for review/triage (Goose as the leading fallback, OpenCode second) — revisit only if a real, observed scaffold-level monoculture problem emerges, not preemptively.
- Periodic re-check of OpenHands' governance trajectory, mirroring the lens already applied to Gitea vs. Forgejo — not a v1 blocker, but worth not forgetting about.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- [ADR-2: Agent Identity & Permission Model](week1-adr-2-agent-identity-and-permission-model.md)
- [ADR-6: Git Hosting Platform Selection](week1-adr-6-git-hosting-platform-selection.md)
- [ADR-7: Multi-Agent Review & Iteration Workflow](week1-adr-7-multi-agent-review-workflow.md)
- [Plan: OpenHands LocalRuntime Validation](../plans/openhands-localruntime-validation.md) — the PoC that validated this ADR's LocalRuntime decision (run 2026-08-30, all steps passed)
- OpenHands/OpenHands, Forgejo integration — PR #11111 (merged), closing issue #9354 — https://github.com/OpenHands/OpenHands
- OpenHands Docs, Runtime Architecture (Docker Runtime, LocalRuntime, RemoteRuntime) — https://docs.openhands.dev/openhands/usage/architecture/runtime
- OpenHands Docs, Headless Mode — https://docs.openhands.dev/openhands/usage/cli/headless
- Linux Foundation, "Announces the Formation of the Agentic AI Foundation," on Goose's donated governance — https://www.linuxfoundation.org/press/linux-foundation-announces-the-formation-of-the-agentic-ai-foundation
- OpenCode repository and license — https://github.com/opencode-ai/opencode
