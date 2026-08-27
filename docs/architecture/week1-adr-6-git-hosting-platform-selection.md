# ADR-6: Git Hosting Platform Selection

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-27 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Developer Experience |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-0](week1-adr-0-and-capability-map.md); binds the git-forge capability requirements assumed by [ADR-1](week1-adr-1-agent-execution-model.md) (execution model) and [ADR-2](week1-adr-2-agent-identity-and-permission-model.md) (identity/permission model); informed by [ADR-3](week1-adr-3-policy-enforcement-layer.md)–[ADR-5](week1-adr-5-observability-and-audit-trail.md) wherever they touch the git host |

## Context

ADR-1 and ADR-2 both deferred naming a concrete git hosting platform to this ADR, while fixing what it must provide: repository hosting with clone/push access; a pull/merge-request workflow reachable via API that records author and description; an authentication mechanism the orchestrator can mint fine-grained, expiring, single-repository-scoped tokens against; dedicated bot/service accounts distinct from human users; webhooks; and — called out explicitly in ADR-2 — token-scope enforcement that is *actually correct*, not merely documented, since scope-based isolation is the credential-layer expression of this harness's whole blast-radius model.

**Scope of this decision.** This ADR selects the git host for **local development and the single-person pilot only** (per ADR-0's Open Questions, the pilot team is currently just me). The organization's production git hosting is expected to be GitLab, independent of this initiative. That materially changes what this ADR is for: it is not selecting a platform meant to carry forward into production, so a platform's long-run enterprise maturity is not the deciding consideration — low friction to stand up and run locally is. When/if this harness moves toward a GitLab-hosted production environment, the git-host-specific assumptions baked into ADR-1/ADR-2 (token/scope shape, webhook payloads, bot-account mechanics) will need their own follow-up validation against GitLab specifically — flagged as a consequence below, not solved here.

**Candidates surveyed.** Free/self-hostable forges with a PR/MR API were surveyed broadly: Gogs (predecessor project, fine-grained/expiring tokens never fully caught up), SourceHut (email/patch workflow, not API-driven PRs — wrong interaction model for ADR-1's agent-opens-a-PR flow), OneDev, RhodeCode CE, and Gitness/Harness Open Source (all niche, thin public security track record, small ecosystems). None of these were carried into deeper evaluation. GitLab CE meets every capability requirement and is the most enterprise-mature option, but was excluded from this specific decision: it requires a materially heavier stack (Rails + PostgreSQL + Redis + Sidekiq workers, several GB RAM minimum) than a solo local pilot needs, and standing it up locally would be redundant work given production is already headed toward GitLab. That left **Gitea** and **Forgejo** — Forgejo is a community-governed (Codeberg e.V., a non-profit) fork of Gitea, drop-in schema/config/API compatible, versus Gitea itself, which is now steered by a for-profit company (Gitea Ltd.).

**CVE/advisory history deep-dive.** Gitea underwent a large, concentrated wave of security disclosures in 2026 — 85 GitHub Security Advisories between April and August alone (OSV.dev's broader aggregation across NVD/GHSA/GitLab shows 162 total historically, of which 104+ landed in 2026, dwarfing the combined 2021–2025 total of 58). A large share are token-scope enforcement bypasses — archive downloads, RSS/Atom feeds, OAuth introspection, org/team endpoints, LFS — described by researchers as a structural pattern: scope checks are enforced at the route/middleware layer but not consistently at every downstream data path, so each new endpoint risks reintroducing the exact bug class ADR-2 flagged. Forgejo, by contrast, has only 7 NVD-listed CVEs specifically naming it since its 2023 fork, but this is not strong evidence of a safer codebase: it confirmed inheriting at least one of Gitea's critical 2026 issues (CVE-2026-20896, a Docker default letting any source IP impersonate any user), and in April 2026 a researcher's "carrot disclosure" chained multiple bug classes into a working RCE PoC against Forgejo specifically — Forgejo's all-volunteer security team publicly stated they lack the capacity to run the holistic audit that would be needed to find what else might be there. **Conclusion carried into this decision:** the CVE gap between the two is largely an artifact of Gitea having just absorbed adversarial scrutiny Forgejo hasn't yet received, not evidence Forgejo's shared authorization architecture is meaningfully sounder. Per this ADR's scope note above, this comparison is not the deciding factor for a local-only pilot platform — but the underlying finding (platform-side token-scope enforcement is a demonstrated, live risk class in this codebase family) is real and is carried forward as a consequence for ADR-3/ADR-4 regardless of which of the two is chosen here.

## Decision Drivers

- **Local setup friction** — the dominant driver for this specific decision, given its solo-pilot/local-only scope: time to get a working instance with token/webhook/bot-account support configured.
- **Capability fit** — must satisfy ADR-1's and ADR-2's assumed platform requirements (PR/MR API, scoped/expiring tokens, bot accounts, webhooks).
- **Avoiding a single point of organizational failure** (ADR-0) — applies to governance of the platform itself, not just the harness: a project whose direction a single for-profit company controls is a different risk shape than one under non-profit/community governance.
- **Explicitly de-prioritized for this decision**: comparative CVE/security posture between Gitea and Forgejo — relevant in general, but not decision-driving here since this platform never reaches production (see Context).

## Options Considered

**1. GitLab CE.** Rejected for this decision (not for later use) — correct capabilities, but disproportionate operational weight for a local, solo pilot, and redundant given production is independently headed toward GitLab already.

**2. Gogs, SourceHut, OneDev, RhodeCode CE, Gitness.** Rejected — weak fit against ADR-2's token/scope requirements, wrong interaction model, or too thin an ecosystem/security track record to evaluate with confidence.

**3. Gitea.** Meets every capability requirement. Installable locally via Homebrew (bottled binary, `brew services start gitea`) or a single static binary with embedded SQLite — trivial to run locally. Governance now sits with a for-profit company (Gitea Ltd.); underwent the large 2026 disclosure wave described above.

**4. Forgejo — chosen.** Meets every capability requirement identically to Gitea, being a drop-in-compatible fork (same `app.ini` config shape, same API surface, same CLI invocation pattern). Confirmed via Homebrew that local setup ergonomics are indistinguishable from Gitea's: same bottled-binary formula shape, same `brew services start forgejo`, same `<binary> web --work-path ...` invocation. With local-setup friction and capability fit tied, this ADR breaks the tie on governance: Forgejo is steered by Codeberg e.V., a non-profit, community-governed body — a better fit for ADR-0's "avoid a single point of organizational failure" principle than a single for-profit steward. This is the exact tiebreaker requested for this decision.

## Decision

Run **Forgejo** locally (via Homebrew, or the equivalent single-binary/Docker install) as the git hosting platform for local development and the pilot phase of this harness. Configure it to provide what ADR-1/ADR-2 assume: a dedicated pool of purpose-scoped bot accounts, fine-grained expiring repo-scoped access tokens, and webhooks wired to the orchestrator.

This decision is explicitly scoped to the pilot phase. It does **not** decide the production git host — that is expected to be GitLab, and is out of scope for this ADR. Before any production rollout, ADR-1's and ADR-2's git-host-facing assumptions (token/scope model shape, webhook payload shape, bot-account mechanics) need a dedicated follow-up validation pass against GitLab's actual API, not an assumption that Forgejo's shape carries over unchanged.

## Consequences

**Positive**
- Zero-cost, fast to stand up locally (Homebrew install + `brew services start`), meeting every capability ADR-1/ADR-2 assumed without any platform-specific rework of those ADRs.
- Governance choice is consistent with ADR-0's stated aversion to single points of organizational failure.
- Avoids the wasted effort of running a full GitLab CE stack locally for a solo pilot when production will run GitLab independently anyway.

**Negative / trade-offs**
- Forgejo has had materially less independent adversarial security scrutiny than Gitea to date (see the carrot-disclosure episode and its volunteer-only security team) — treat this as "less tested," not "more secure." Per the CVE deep-dive, platform-side token-scope enforcement in this codebase family is a demonstrated, live risk class; ADR-3/ADR-4 should not treat it as a sufficient boundary on its own for the highest risk tiers, regardless of the fact that Gitea vs. Forgejo wasn't decided on this basis.
- This decision does not transfer to production. A follow-up ADR is required at the point this harness moves toward GitLab, re-validating ADR-1's and ADR-2's git-host assumptions against GitLab's actual token/scope/webhook/bot-account shapes rather than assuming Forgejo-equivalence.
- Running two different git hosting platforms across the harness's lifecycle (Forgejo for pilot, GitLab for production) means integration code written against Forgejo's API during the pilot should not be assumed portable as-is. **Decided: no adapter/abstraction interface is built for this now** — with a single consumer (this pilot) and a single platform actually integrated, an adapter layer would be speculative design against a swap that isn't scheduled, contrary to ADR-0's bias against designing for hypothetical future requirements. The Forgejo→GitLab transition, if and when it's scheduled, is the right time to introduce that boundary — informed by GitLab's actual API rather than a guessed-at shape now.

## Open Questions

- Which Forgejo version/patch level to pin locally, and who is responsible for tracking its security-announcements feed given the demonstrated patch cadence across this codebase family.
- Whether to validate GitLab CE's capability parity now (cheap, since much of this ADR's evaluation transfers) or defer entirely until the actual production transition is scheduled.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- [ADR-2: Agent Identity & Permission Model](week1-adr-2-agent-identity-and-permission-model.md)
- GitHub Security Advisories, `go-gitea/gitea` — https://github.com/go-gitea/gitea/security/advisories
- OSV.dev, `code.gitea.io/gitea` (Go ecosystem) — https://osv.dev
- Forgejo security-announcements tracker — https://codeberg.org/forgejo/security-announcements/issues
- LWN.net, "Forgejo 'carrot disclosure' raises security questions" — https://lwn.net/Articles/1071499/
- dustri.org, "Carrot disclosure: Forgejo" (jvoisin) — https://dustri.org/b/carrot-disclosure-forgejo.html
- SecurityWeek, "Gitea Vulnerability Exposed 30,000 Deployments to Attacks" — https://www.securityweek.com/gitea-vulnerability-exposed-30000-deployments-to-attacks/
- The Hacker News, "Gitea Vulnerability Exposes Private Container Images without Authentication" — https://thehackernews.com/2026/05/gitea-vulnerability-exposes-private.html
