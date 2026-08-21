# ADR-0: AI Coding Agent Harness — Problem Statement & Scope

| | |
|---|---|
| **Status** | Proposed |
| **Date** | 2026-08-12 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Compliance/Legal, FinOps, Developer Experience, [1–2 affected product teams as pilot candidates] |
| **Informed** | Engineering leadership, platform team, affected product team leads |
| **Supersedes / Related** | — (this is the root ADR; ADR-1 through ADR-4 build on it) |

## Context

AI coding agents (e.g. Claude Code, Copilot, internally built agent tooling) are already in use — informally, by individual developers, with no tracking, approval process, or audit trail. This is a greenfield initiative: there is no existing agent-governance process to extend and no prior platform investment this harness needs to stay compatible with. There is currently no unified control plane governing what these agents are permitted to do, which identity they act under, how their actions are audited, or how their blast radius is limited when something goes wrong.

This is a general industry gap, not unique to us: as of mid-2026, industry surveys of large-enterprise security leaders found that the large majority lack full visibility into their AI identities and do not enforce access policies for them, even though many already have AI systems with access to core business platforms. Left unaddressed, this becomes a "shadow AI" governance problem — similar in shape to shadow IT, but with agents that can autonomously take actions rather than just store data.

Agents are, in effect, a category of **non-human identity (NHI)**. The identity model this initiative builds (ADR-1) should follow NHI lifecycle patterns — short-lived/session-scoped credentials, explicit delegation chains ("acting on behalf of"), rotation and revocation — rather than reusing human-SSO patterns as-is.

The goal of this ADR is **not** to design the full harness — that's ADR-1 through ADR-4. It's to fix scope, so those later decisions are made against a shared, agreed problem statement rather than each being litigated from scratch.

## Decision Drivers

- **Security exposure** — ungoverned agent access to source, secrets, and infrastructure is a growing attack surface with no current audit trail.
- **Developer adoption already happening** — agents are in use whether or not the platform team governs them; the harness needs to reduce friction relative to today's ungoverned state, or developers will route around it.
- **Auditability & compliance** — no formal regulatory mandate currently applies (no SOC 2 or industry-specific requirement identified); auditability is pursued as a security/engineering best practice, not a compliance deadline. Worth tracking NIST's 2026 AI Agent Standards Initiative (identity/authorization guidance expected Q4 2026) since it may shape what "good" looks like later, even without a hard deadline today.
- **Cost containment** — agent usage (compute, API spend, environment provisioning) needs visibility before it needs limits.
- **Avoiding a single point of organizational failure** — the harness must not become a bottleneck that teams route around (see ADR-1/ADR-2 permission-intersection model).
- **Tool / MCP supply-chain risk** — agents get most of their real-world capability through MCP servers and third-party tools; a compromised or malicious tool (poisoned tool descriptions, tool-output injection) is a distinct attack surface from the action-permission model and needs its own vetting/allowlisting approach.
- **Containment & recoverability** — once a misbehaving agent is identified, there needs to be a fast, reliable way to revoke its access ("kill switch") independent of the agent's own process, not just a record of what it did after the fact.

## Scope

### In scope for this initiative
1. Agent identity & permission model (ADR-1) — including the identity→policy claims interface ADR-2 depends on; this is fixed early in ADR-1 so ADR-2 isn't blocked waiting on it.
2. Authorization / policy enforcement layer (ADR-2)
3. Blast-radius controls & execution sandboxing (ADR-3) — including kill-switch / revocation mechanics.
4. Observability & audit trail (ADR-4)
5. MCP / tool server governance (vetting, allowlisting, treating tool descriptions and output as untrusted input) — folds into ADR-2 (policy) and ADR-3 (sandboxing).

### Explicitly out of scope for this phase
- Model selection / evaluation of which LLM to use
- Prompt engineering standards or agent instruction design
- Non-coding agent use cases (e.g. customer-support agents, sales agents) — may reuse this harness later, but not designed for it now
- Build-vs-buy evaluation — this initiative is scoped as a build, not a vendor evaluation.

## Agent Action Categories & Risk Tiers

This table is the reference both ADR-1 (identity) and ADR-2 (policy) will build on — every downstream permission and policy decision maps back to a row here.

| Category | Example action | Risk tier | Illustrative policy |
|---|---|---|---|
| Read-only exploration | Browse repo, read docs, search code | Low | Broad default allow |
| Tool / MCP server access | Agent connects to or invokes an MCP server / external tool | Medium | Requires vetted/allowlisted registry; tool descriptions and outputs treated as untrusted input, not instructions |
| Code generation / PR creation | Open a PR against a non-critical repo | Medium | Auto-allowed; human review required before merge |
| Test execution | Run test suite in an ephemeral sandbox | Medium | Auto-allowed within sandbox; no prod credentials available |
| Deploy — non-prod | Push to staging/dev environment | Medium-High | Requires policy gate check; logged |
| Deploy — production | Push to prod, infra changes | High | Requires explicit human approval gate; full audit |
| Access to customer / regulated data | Query prod DB, read PII | High | Requires explicit human approval + data classification check |

**Note:** fill in actual repos/environments/data classifications relevant to your org — this table is a starting skeleton, not a finished policy.

**Deferred:** prompt-injection / goal-hijacking risk — an authorized-looking action driven by attacker-controlled input rather than genuine intent — is deliberately not modeled in this table. It's picked up in ADR-2 (can policy consider the provenance of an instruction, not just the action?) and ADR-3 (blast radius should be contained regardless of whether the agent is buggy or hijacked).

## Success Criteria

How you'll know the harness is working, not just built:

- % of agent actions that pass through a policy gate (target: as close to 100% as possible for medium/high risk tiers)
- Mean time to provision an agent-scoped ephemeral environment
- Audit trail completeness — can you answer "which agent, whose delegated authority, what action" for any incident within [target time]?
- Developer adoption / friction — is time-to-first-PR faster or slower than today's ungoverned baseline?
- Zero unreviewed production changes originating from an agent
- Mean time to revoke a misbehaving agent's access (kill switch) once identified — target to be set in ADR-3

## Open Questions

- who is the actual executive sponsor / budget owner for this initiative? me
- is there a legal/compliance deadline driving urgency, or is this proactive? - no
- which team(s) volunteer as the pilot before org-wide rollout? mine

**Note:** sponsorship, budget ownership, risk acceptance, and the pilot team are currently all the same person (me). Acceptable for the pilot phase; must be broadened before org-wide rollout, given this ADR's own driver of avoiding a single point of organizational failure.

## References

- Industry visibility/access-policy statistics on AI agent identity (2026 CISO AI Risk Report context)
- [add internal links: existing IAM docs, existing CI/CD architecture, existing service catalog]

---

## Technology Choices

This initiative builds new infrastructure rather than extending or replacing existing systems, so a reuse/gap inventory doesn't apply here. Concrete technology choices (identity provider, policy engine, CI/CD integration, audit sink, environment provisioning, etc. — candidates include AWS IAM, OPA, Gitea, CloudWatch, CloudFormation) are made as explicit decisions in ADR-2, with rationale, rather than inventoried in this document.
