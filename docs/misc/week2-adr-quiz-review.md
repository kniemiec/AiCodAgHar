# ADR Quiz Review — Summary Notes

Personal study notes from a self-quiz covering ADR-0 through ADR-8 (2026-08-31/09-01).
Not a design document — a condensed "why" reference per ADR, plus the recurring
cross-cutting patterns and the specific gaps that came up during review.

## How to use this

Each ADR section below has: the decision in one line, the 2-4 justifications that
actually matter (not just "what" but "why this and not the alternative"), and any
detail that was easy to get wrong. The **Recurring Patterns** and **Weak Spots**
sections at the end are the highest-value parts to re-read before a real review.

---

## ADR-0 — Problem Statement & Scope

**Decision:** Fix scope before designing anything; treat agents as non-human identities (NHI).

- Agents follow **NHI lifecycle patterns**, not human-SSO patterns: short-lived/session-scoped
  credentials, **explicit delegation chains** ("acting on behalf of"), rotation and revocation.
  All three properties matter — not just "short-lived."
- Introduces the **risk-tier action table** every later ADR builds on.
- Deliberately **defers prompt-injection/goal-hijacking** out of the risk table — split across
  two later ADRs (see ADR-3/ADR-4 below): can't detect a hijacked *intent* mid-run (no
  checkpoint exists), but blast radius must be contained regardless of whether the agent is
  buggy or hijacked.
- Explicitly out of scope: model selection, prompt engineering, non-coding agents, build-vs-buy.

---

## ADR-1 — Agent Execution Model

**Decision:** Orchestrator (outside any sandbox) + ephemeral, one-shot container per invocation.
One invocation = one new task **or** one round of work on an already-open PR — not one per task.

- **Persistent direct-client agent rejected** for three separate reasons: (1) no natural credential
  expiry tied to a unit of work → drifts to durable standing access; (2) kill switch means locating
  and terminating an arbitrary long-running process, not just "don't relaunch"; (3) audit trail
  blurs across however many tasks the process handles before restart.
- **One-shot per round, not per task**: cost is that cold-start work (clone, install, build/index)
  repeats every round — flagged as a real trade-off to watch, not free. What "absorbs" it is not a
  technical mechanism — continuity is passed forward *explicitly as prompt context*, never relied
  on implicitly. Resumable workspaces logged as a future option if this becomes a measured problem.
- **Orchestrator built bespoke**, not on a CI-runner framework — because ADR-2/ADR-3 hadn't yet
  fixed what orchestration responsibilities (policy calls, credential minting, audit emission)
  actually need to be; a CI runner would bake in assumptions before requirements are known.

---

## ADR-2 — Agent Identity & Permission Model

**Decision:** Three identity layers — standing orchestrator NHI, triggering principal, and a small
pool of purpose-scoped bot accounts with per-invocation ephemeral tokens.

- **Per-invocation ephemeral *account* (create+delete each time) considered, not adopted**: strongest
  possible attribution, but real operational cost — full account lifecycle management, orphaned-account
  risk if the orchestrator crashes mid-invocation, namespace churn.
- **`invocation_id` vs `task_id`**: `task_id` didn't exist until ADR-7 introduced multi-stage work
  (review/security-triage as separate invocations against the same PR) — before that, one task was
  one invocation, so `invocation_id` alone was sufficient. `task_id` groups every invocation of one
  logical unit of work for audit/cost rollup.
- **Claims "inform but don't self-authorize"**: the delegation record is *input* to ADR-3's PDP —
  possessing valid claims does not itself mean "allowed." This avoids identity becoming a single
  point of failure (compromising/forging a claim ≠ automatic authorization); a separate policy
  decision is always required on top.
- Two-tier revocation: kill one invocation's token, or revoke the orchestrator's own minting
  credential (kills everything at once).

---

## ADR-3 — Authorization / Policy Enforcement Layer

**Decision:** Central PDP (OPA, called via the AuthZEN standard) + PEP (the orchestrator) +
layered policy data: a global baseline that per-repo overrides can only **narrow**, never widen.

- **Hard-coded rules in the orchestrator rejected**: every tweak needs a redeploy reviewed by
  whoever owns orchestrator code (recreates ADR-0's single-team bottleneck), and there's no
  independently versioned record of "what policy was in effect when."
- **Single global bundle rejected too**, even though it decouples policy from orchestrator code:
  it recreates the same bottleneck one level up — every repo-specific tweak still needs the
  platform team. Layered narrow-only overrides give genuine self-service within a fixed ceiling.
- **Why "narrow only"**: the override *data* is itself a privileged artifact. If repos could widen,
  whoever can write to that data could grant their own repo broader access than the platform-level
  ceiling intends — a privilege-escalation path through the config mechanism itself, not a runtime
  attack.
- **OPA via AuthZEN, not an OPA-proprietary integration**: OPA has the deepest self-hosted track
  record, but its commercial steward (Styra) is winding down — real governance drift. AuthZEN
  abstracts the PDP↔PEP call so OPA can be swapped for Cedar/Cerbos later without reworking the
  orchestrator. **Same defensive "don't couple to one vendor's fate" pattern reused at OpenBao/Vault
  (ADR-4) and Forgejo/Gitea (ADR-6).**
- **Fail-closed**: PDP unreachable → deny. Makes PDP availability a hard dependency for running any
  agent at all — an outage halts the whole harness by design, which is why ADR-5 puts PDP/OpenBao
  health-check alerting on a short (~30s) threshold.
- Provenance-of-intent (was this action driven by genuine task or by a hijack mid-run?) is
  explicitly **not** achievable here — no mid-run checkpoint exists. What this layer *can*
  guarantee: no action can exceed the scope decided at mint time, regardless of what the agent is
  tricked into attempting.

---

## ADR-4 — Blast-Radius Controls & Execution Sandboxing

**Decision:** Firecracker microVM for *every* invocation uniformly (gVisor as documented fallback
only where KVM is unavailable) + default-deny network egress via allowlist proxy + network-policy
backstop + orchestrator's own minting credential stored in OpenBao.

- **Why uniform strongest isolation, not tiered-by-risk**: every invocation *is* the risky case —
  running LLM-directed, potentially attacker-influenced code is the default workload, not an edge
  case to escalate to. "No invocation is trusted by design." Uniform isolation also removes an
  entire misconfiguration class ("did we forget to mark this repo high-risk").
- **Firecracker over gVisor as default**: genuine hardware/kernel boundary (KVM) below what the
  agent can reason about or escape from computationally. Boot latency (~125ms) is negligible
  against clone/install/LLM-inference time that already dominates wall-clock — the traditional
  objection to microVMs doesn't apply here.
- **Two-layer egress control, neither sufficient alone**: the forward proxy enforces the allowlist
  and logs every attempt (including denials — a free, high-signal hijack/bug detector); the
  network-policy rule blocks *any* egress not through the proxy, closing the gap where a tool that
  ignores proxy env vars could otherwise leak out directly.
- **OpenBao over HashiCorp Vault**: Vault's OSS edition moved to the Business Source License (BSL)
  in 2023, restricting embedded/hosted use. OpenBao is a Linux Foundation–governed fork from
  Vault's last MPL-2.0 release, Vault-API-compatible, no commercial-use restriction — plus its
  native dynamic-secret leasing/rotation directly satisfies ADR-2's "short-lived, auto-renewed"
  requirement for the orchestrator's own credential.
- Two-tier kill switch: per-invocation (container kill + explicit token revocation, belt-and-
  suspenders, target sub-60s p95) and global (revoke orchestrator's OpenBao lease + circuit
  breaker, target sub-5min p95) — both first-pass estimates pending real measurement.

---

## ADR-5 — Observability & Audit Trail

**Decision:** Two right-sized stores, not one — append-only hash-chained Postgres for audit,
Prometheus/Grafana/Alertmanager for operational telemetry.

- **Why not one unified log-search system**: audit records need tamper-evidence, long retention,
  exact-match queries; operational telemetry is high-volume, short-retention, aggregate/time-series
  in shape. Forcing both into one system either weakens the audit guarantees or misuses the tool.
  (A full enterprise stack — OpenSearch + object-lock storage — was also rejected as disproportionate
  to actual expected volume: one record per invocation, exact-match lookups, not big-data search.)
- **Hash-chaining is tamper-*evident*, not tamper-*proof***: detects a single retroactive edit
  (breaks the chain), but does **not** protect against a fully compromised DB host recomputing the
  entire chain from scratch. Mitigated by periodic export/backup to a separate system (open
  question: exactly where/how).
- **Policy-gate consistency check**: reconciles containers *actually launched* against PDP `allow`
  decisions *logged*. Catches what the raw "% of actions through a gate" metric can't — a code path
  that bypasses the PDP call entirely wouldn't appear in the gate's own log, so the self-reported
  percentage would look clean even during a real bypass. Cross-referencing against an independent
  signal (actual launches) is what makes this an active bypass detector, not a passive number.
- Avoided the MinIO open-core trap (functionality stripped from OSS, pushed to a $96k/yr commercial
  tier) — same governance-risk lens applied again; Garage/SeaweedFS logged as genuinely free
  alternatives if WORM storage is ever needed.

---

## ADR-6 — Git Hosting Platform Selection

**Decision:** Forgejo, for **local dev + solo pilot only** — explicitly not a production decision
(production is expected to be GitLab, independently).

- **GitLab CE rejected for this decision** (not disqualified generally): meets every requirement,
  most enterprise-mature, but disproportionate operational weight for a solo local pilot, and
  redundant since production is headed to GitLab anyway. Scope of the ADR is explicitly narrow —
  this is *why* long-run enterprise maturity wasn't the deciding factor.
- **CVE deep-dive conclusion — the subtle one**: Gitea had far more 2026 disclosures than Forgejo,
  but the ADR treats this as "Gitea absorbed adversarial scrutiny Forgejo hasn't yet received," not
  "Forgejo is safer." Evidence: Forgejo confirmed inheriting a critical 2026 Gitea bug, and had its
  own RCE PoC found via "carrot disclosure," with its volunteer security team admitting they lack
  audit capacity. **Disclosure volume ≠ security posture** — explicitly de-prioritized as the
  deciding factor for this reason.
- **Actual tiebreaker**: governance. Forgejo → Codeberg e.V. (non-profit, community-governed) vs.
  Gitea → a for-profit company (Gitea Ltd.) — maps directly to ADR-0's "avoid single point of
  organizational failure."
- **No adapter/abstraction layer built now**, on purpose: with a single consumer and a single
  platform actually integrated, an adapter would be speculative design against a swap that isn't
  scheduled (ADR-0's stated bias against designing for hypothetical requirements). The ADR
  explicitly warns Forgejo-integration code should **not** be assumed portable to GitLab as-is —
  token/scope/webhook/bot-account shapes all need re-validation at migration time. The abstraction
  cost is deliberately deferred, not eliminated.

---

## ADR-7 — Multi-Agent Review & Iteration Workflow

**Decision:** Fixed core orchestrator loop (generate → fan out → aggregate → evaluate →
continue/stop) + a **declarative, config-driven stage list** — not hardcoded, not a full
event-driven registry.

- **Self-attribution bias**: models measurably favor their own output; reviewing code adjacent to
  how a model just reasoned about writing it reproduces the same blind spot. Concrete evidence:
  GitHub Copilot PRs are reviewed by Copilot's own reviewer bot ~91% of the time. Countered by (1)
  requiring review/security-triage to run in a genuinely separate context, ideally a **different
  underlying model** (cross-model = target, isolated-context-same-model = accepted floor), and (2)
  separate, narrowly-scoped bot identities per role (ADR-2).
- **Security-triage is scanner-first, LLM-second, not LLM-cold-review**: deterministic scanners
  (SAST/secret/dependency) detect; a separate triage agent judges *reachability, severity,
  remediation* of what the scanner already flagged. LLMs are explicitly unreliable at finding
  vulnerability classes unassisted — never asked to.
- **Hardcoded pipeline rejected** (extensibility requirement — adding/removing a stage shouldn't
  need an orchestrator redeploy). **Event-driven plugin registry also rejected — for now**: more
  dynamically extensible, but solves a scaling/dynamism problem this pilot doesn't have yet, and
  adds real complexity (pub/sub, harder-to-reason ordering). Declarative config is the "pipeline as
  data" middle ground — same shape every CI system already uses.
- **Round cap = 5**, grounded in actor-critic research (3-5 rounds eliminate 90%+ of issues, sharply
  diminishing returns after). Lives in **core orchestrator code, not per-stage config**, because
  it's a safety valve that must bound total iteration regardless of which stages happen to be
  configured — no stage-config combination should be able to create an unbounded loop. Hitting the
  cap is a **stop-escalate** (human notified with full history), never a silent failure.
- `task_id` (introduced back into ADR-2) is what ties a coding round + every review/triage round it
  triggers into one auditable, cost-attributable unit.

---

## ADR-8 — Coding-Agent Runtime Selection

**Decision:** OpenHands, single runtime for all three roles (coding/review/security-triage), in
headless mode with `LocalRuntime`.

- **Vendor-proprietary CLIs excluded** (Claude Code, Codex CLI, Gemini CLI): each ties the runtime
  to one model vendor, which would quietly reopen ADR-0's out-of-scope model-selection question.
  Notably, the ADR **discloses its own conflict of interest** — the series was authored using
  Claude Code — rather than letting that bias the comparison silently.
- **Why one framework serves three roles without diversifying**: ADR-7's bias research is about the
  *model* reviewing its own reasoning trace, not about which framework orchestrates that model. All
  three candidates (OpenCode, OpenHands, Goose) are model-agnostic, so cross-model independence is
  achievable through **configuration alone** — different backing model per role, same framework.
- **LocalRuntime over OpenHands' default Docker Runtime**: the default builds its *own nested
  Docker container* — Docker-in-Docker inside ADR-1's already-sandboxed container. LocalRuntime runs
  the same action-execution server directly as a process inside the container the orchestrator
  already owns, so ADR-1's container stays the *only* sandbox boundary. This is explicitly a
  **simplicity trade-off, not a security one** — even the Docker Runtime path would still be
  contained by the same Firecracker/gVisor hardware boundary regardless; LocalRuntime is just
  lighter operationally.
- **Governance risk accepted, not avoided**: OpenHands is company-steered (All Hands AI), unlike
  Goose (Linux Foundation AAIF) — a real inconsistency against the series' own governance lens
  (Forgejo-over-Gitea). Accepted here because the concrete integration/token-surface argument
  (native Forgejo support, no third-party MCP server handling the git credential) outweighed the
  governance concern for this specific decision. **This is the one case in the series where the
  governance-risk pattern was explicitly overridden.**
- **PoC correction (2026-08-30)**: assumed a dedicated native-provider action type for Forgejo
  interaction; PoC found the agent actually uses its general-purpose shell tool (`git` + hand-built
  `curl` against Forgejo's REST API) instead. Functionally correct, but the token flows through the
  visible shell action/observation log as `${FORGEJO_TOKEN}` — one PoC run even echoed a partial
  token prefix into the log. Not blocking (tokens are short-lived/revocable), but flagged as a
  concrete input to how invocation logs get stored/redacted later.

---

## Recurring Patterns (the connective tissue across all 9 ADRs)

1. **Avoid single points of failure/organizational bottleneck** — identity claims alone don't
   authorize (ADR-2/3); global policy bundle still needs per-repo self-service (ADR-3); gate
   criteria live in policy, not one LLM's judgment call (ADR-7); pilot sponsorship being one person
   flagged as unacceptable at scale (ADR-0).
2. **Don't couple architecture to one vendor/company's fate** — OPA via AuthZEN, OpenBao over
   Vault (BSL), Forgejo over Gitea (non-profit vs. for-profit steward), MinIO explicitly avoided as
   a future option. **The one deliberate exception: OpenHands over Goose in ADR-8**, where a
   concrete technical argument was judged to outweigh the governance concern.
3. **Worst-case / contain-regardless-of-hijack framing** — uniform Firecracker isolation (not
   tiered by "trust"), default-deny egress, fail-closed PDP, agent's own output never trusted as
   audit record.
4. **Defer concrete platform/tool bindings until requirements are known** — git host deferred to
   ADR-6, coding runtime deferred to ADR-8, both evaluated *against* earlier fixed constraints
   rather than the other way around. Same instinct behind not building a Forgejo→GitLab adapter now.
5. **Disclosure volume / CVE count ≠ security posture** — the Gitea/Forgejo analysis (ADR-6)
   treats scrutiny asymmetry as the real variable, not raw counts.
6. **The orchestrator, never the agent, is the source of truth** — for audit records (ADR-1/5),
   for test verification (ADR-7's gate criteria doesn't trust the coding agent's self-report).

---

## Weak Spots From This Review (worth re-reading before a real test)

- **ADR-3's per-repo override risk** (Q10): the danger of allowing "narrowing→widening" isn't a
  hijacked agent probing at runtime — it's that the *override policy data itself* is a privileged
  write target; whoever can write it could self-grant broader access than the platform ceiling.
- **ADR-3's fail-closed PDP consequence** — remember it's a hard *availability* dependency for the
  entire harness, not just a per-request denial.
- **AuthZEN/OPA governance mitigation** (Q11) and the fact this exact pattern repeats 3-4 times
  across the series — good to have as a ready reference, not re-derived each time.
- **ADR-6's CVE reasoning** (Q21): re-read the "disclosure volume ≠ security posture, scrutiny
  asymmetry" paragraph directly — this is subtle and generalizes well beyond this ADR.
- **ADR-7's round cap rationale** (Q26) — why it's core-loop code and not stage config (a safety
  valve must bound *any* stage combination).
- **ADR-8's LocalRuntime vs. Docker Runtime** (Q29) — this is about avoiding nested sandboxing
  (Docker-in-Docker inside Firecracker), not about matching some other isolation technology's
  strength. Explicitly a simplicity trade-off, not a security boundary.
- **Sequencing discipline** (Q32): the PDP policy call happens *before* credential minting, as a
  distinct step — don't collapse "policy decision" and "token issuance" into one mental step.
