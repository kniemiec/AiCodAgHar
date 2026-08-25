# ADR-5: Observability & Audit Trail

| | |
|---|---|
| **Status** | Proposed |
| **Date** | 2026-08-25 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Compliance/Legal |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-0](week1-adr-0-and-capability-map.md) (fills in placeholder success-criteria targets), [ADR-1](week1-adr-1-agent-execution-model.md) (orchestrator as sole audit source of truth), [ADR-2](week1-adr-2-agent-identity-and-permission-model.md) (delegation record is the core audit record), [ADR-3](week1-adr-3-policy-enforcement-layer.md) (PDP decision extends the record; fail-closed PDP dependency needs monitoring), and [ADR-4](week1-adr-4-blast-radius-and-sandboxing.md) (egress-proxy denial logs, kill-switch timing, OpenBao availability) |

## Context

Every prior ADR has been depositing a piece of what this one needs to consolidate: ADR-1 fixed that the orchestrator, not the agent, emits the audit record for each invocation (start, credential issuance, stop, outcome). ADR-2 fixed that record's core shape — the delegation record (`invocation_id`, `triggering_principal`, `orchestrator_instance_id`, `bot_account`, `target_repo`, `task_type`, `risk_tier`, `granted_scopes`, `issued_at`, `expires_at`) — and explicitly left its signing/tamper-evidence mechanism open. ADR-3 extends that record with the PDP's decision (`decision`, `granted_scopes`, `reason`, `policy_bundle_version`) and flags that PDP availability needs monitoring, since fail-closed makes it a hard dependency for running any agent at all. ADR-4 adds the same criticality for OpenBao, plus two things that need near-real-time attention rather than pure after-the-fact logging: egress-proxy denial events (a live hijack/bug signal) and kill-switch timing (first-pass targets that need to be measured against reality, not just asserted).

ADR-0 also left two things as literal placeholders this ADR needs to fill: "any incident answerable within `[target time]`" (audit success criterion) and "mean time to revoke ... target to be set in ADR-3" — that second one was actually set as a first-pass number in ADR-4 already; what's left for this ADR is to make sure it's *measured*, not to re-decide it.

## Decision Drivers

- **Auditability** — the primary reason this ADR exists: "which agent, whose delegated authority, what action" answerable for any incident.
- **Security exposure** — several signals here need to be near-real-time detection, not retrospective log entries (egress denials, PDP/OpenBao outages, policy-gate bypass).
- **Cost containment** — spend/quota trend needs visibility, not just enforcement (ADR-3's quota rules).
- **Containment & recoverability** — the fail-closed hard dependencies (OPA, OpenBao) introduced by ADR-3/ADR-4 need their own availability monitoring, or an outage becomes a silent, undiagnosed halt.
- **Avoiding a single point of failure** — the audit trail's integrity must not depend on the thing it's auditing; the agent is never a writer to its own audit record (ADR-1).

## Options Considered

**1. One unified store for both audit records and operational telemetry** (e.g., dump everything into a single search/log system). Rejected — audit records need tamper-evidence, long retention, and exact-match queries; operational telemetry is high-volume, short-retention, and aggregate/time-series in shape. Conflating them either weakens the audit guarantees or forces one system to do two jobs it isn't well-suited to doing together.

**2. A full enterprise observability stack** — dedicated log-search cluster (e.g. OpenSearch) plus a separate metrics stack plus S3-compatible immutable object storage for audit records. Considered, rejected as disproportionate to actual expected volume. This harness produces one structured audit record per invocation, not big-data log volume, and the actual query pattern is exact-match lookup by `invocation_id`/principal/repo/time — not full-text search across petabytes. Standing up a distributed search cluster and object-lock storage now is infrastructure the current need doesn't justify.

**3. Two-store split, right-sized to what's actually needed — chosen.** An append-only, hash-chained relational table for audit records, and a standard metrics/alerting stack for operational telemetry. Detailed below.

## Decision

### Audit record store: append-only, hash-chained PostgreSQL table

- Every invocation produces exactly one consolidated audit record, keyed by `invocation_id`: ADR-2's delegation record, ADR-3's PDP decision, and ADR-1's execution outcome (container start/stop timestamps, exit status, PR URL created or updated), merged into a single row. Written once, by the orchestrator — never by the agent, consistent with ADR-1/ADR-2's "orchestrator is the source of truth."
- The table is append-only by database permission, not just convention: the orchestrator's DB role has `INSERT` only — no `UPDATE`, no `DELETE`.
- **Tamper-evidence via hash chaining**: each row includes a SHA-256 hash of the previous row's content. A retroactive edit, even through privileged database access, breaks the chain and is detectable. This resolves ADR-2's open question about a signing/schema mechanism without standing up full asymmetric-signature infrastructure — proportionate to the actual threat model here (detecting insider tampering or a compromised writer), not third-party cryptographic verifiability.
- **Query target** (filling ADR-0's placeholder): any invocation's full chain — principal → orchestrator instance → credential/scopes → policy decision → git action outcome — answerable within **5 minutes** of query, for any incident within the retention window. First-pass target, pending Compliance/Legal input on actual retention requirements (ADR-0 notes no compliance mandate currently applies, so this may remain self-imposed).
- If audit volume or a future regulatory requirement demands genuine WORM/immutable object storage or full-text search beyond indexed Postgres queries, that's a documented future upgrade — added alongside, not instead of, the relational audit table. Worth naming now which object-storage option that upgrade should *not* be: MinIO's open-source edition was effectively discontinued in 2026 — GitHub repo archived, console/admin functionality stripped out and pushed toward a commercial product (MinIO AIStor, priced around $96k/year at entry) — the same open-core pattern already avoided elsewhere in this design (Gitea's commercial stewardship, Vault's BSL relicense, OPA's commercial layer winding down). Garage or SeaweedFS remain genuinely free, self-hostable S3-compatible alternatives if that need materializes.

### Operational metrics & alerting: Prometheus + Grafana + Alertmanager

- **Hard-dependency availability**: OPA (ADR-3) and OpenBao (ADR-4) are both fail-closed dependencies — if either is unreachable, the orchestrator stops minting anything. Both expose native health endpoints; Prometheus scrapes them, and Alertmanager pages if either is unreachable beyond a short threshold (e.g. 30s). A silent halt is not an acceptable way to discover this outage.
- **Kill-switch timing**: actual per-invocation and global revocation latency, measured against ADR-4's first-pass targets (sub-60s / sub-5min), dashboarded, with alerting on meaningful drift past target.
- **Egress-proxy denials** (ADR-4): every denied connection attempt from inside a running container triggers a near-real-time alert, not just a log line — it's the strongest available signal of a hijacked or buggy agent while the container may still be running.
- **Quota/cost trend** (ADR-3's quota rules): dashboarded per triggering principal and per repo, feeding the cost-containment driver with a trend view, not just a hard limit.
- **Policy-gate consistency check**: periodically reconcile the count of containers actually launched (from the orchestrator/container runtime) against the count of PDP allow decisions logged (from ADR-3). Any mismatch — more launches than allows — means the enforcement point was bypassed somehow, which is exactly the failure ADR-0's "% of agent actions that pass through a policy gate" metric is meant to catch. Treated as an active bypass-detection check, not a static reported percentage.

## Consequences

**Positive**
- Audit and operational concerns each get infrastructure sized to what they actually need, instead of one oversized system doing both jobs adequately.
- Hash chaining gives tamper evidence cheaply, without full signing infrastructure.
- The policy-gate reconciliation check turns a passive success metric into an active bypass-detection mechanism.
- PDP/OpenBao health monitoring closes the loop on both ADRs' fail-closed hard dependencies — an outage becomes a page, not a silent halt someone notices hours later.
- Avoided repeating the MinIO open-core trap, consistent with the governance posture already applied to the git host, policy engine, and secrets manager choices.

**Negative / trade-offs**
- Two separate systems to operate (Postgres audit table, Prometheus/Grafana stack) instead of one, though each individually is simple.
- Hash chaining is tamper-*evident*, not tamper-*proof*, against a fully compromised database host capable of rewriting the entire chain — mitigated by periodic export/backup to a separate system, flagged below.
- No full-text search across audit content in v1 — acceptable given the dominant query pattern is exact-match, but a real gap if, say, searching PR descriptions for a specific string becomes a genuine investigative need.
- Retention period and query-time target are proposed defaults, not yet validated against Compliance/Legal input.

## Open Questions

- Actual retention period and query-time SLA, pending Compliance/Legal — ADR-0 notes no compliance mandate currently applies, so this may stay self-imposed rather than externally driven.
- Where and how periodic backup/export of the hash-chained audit table happens, to protect against a fully compromised database host rewriting the whole chain.
- Alert routing/on-call mechanics for PDP/OpenBao outage and egress-denial alerts — process detail, not decided here.
- Whether OpenSearch (not MinIO) is worth adding later if full-text search or genuinely large audit volume materializes.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- [ADR-2: Agent Identity & Permission Model](week1-adr-2-agent-identity-and-permission-model.md)
- [ADR-3: Authorization / Policy Enforcement Layer](week1-adr-3-policy-enforcement-layer.md)
- [ADR-4: Blast-Radius Controls & Execution Sandboxing](week1-adr-4-blast-radius-and-sandboxing.md)
- elest.io, "Self-Hosted Weekly: MinIO Is Dead" — https://blog.elest.io/self-hosted-weekly-week-9-2026-minio-is-dead-open-source-gets-a-00m-endowment-and-ai-slop-hits-maintainers/
- Futuriom, "MinIO Faces Fallout for Stripping Functions from Open Source Version" — https://www.futuriom.com/articles/news/minio-faces-fallout-for-stripping-features-from-web-gui/2025/06
- It's FOSS, "MinIO Is Done With Open Source, What Are Your Options?" — https://itsfoss.com/news/minio-moves-away-from-open-source/
- bigdataboutique, "OpenSearch vs Elasticsearch Compared (2026)" — https://bigdataboutique.com/blog/opensearch-vs-elasticsearch-compared
