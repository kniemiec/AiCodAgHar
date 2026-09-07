# ADR-4: Blast-Radius Controls & Execution Sandboxing

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-25 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-0](week1-adr-0-and-capability-map.md), [ADR-1](week1-adr-1-agent-execution-model.md) (isolation mechanism and network egress were explicitly deferred here), [ADR-2](week1-adr-2-agent-identity-and-permission-model.md) (protects the orchestrator's own minting credential), and [ADR-3](week1-adr-3-policy-enforcement-layer.md) (enforces the MCP/tool allowlist policy decides); revocation mechanics for the git-hosting-side token depend on ADR-6 |

## Context

ADR-1 fixed the execution shape (orchestrator + ephemeral one-shot container) but explicitly deferred three things here: which isolation mechanism the container actually uses, how network egress is restricted, and — via ADR-2 — how the orchestrator's own credential-minting secret is stored and rotated. ADR-0 also deferred a specific principle to this ADR: blast radius must be contained **regardless of whether the agent is buggy or hijacked** — i.e., every control here has to hold even in the worst case, not just the well-behaved one, since a prompt-injected or malfunctioning agent is exactly the scenario this ADR exists for.

That worst-case framing matters concretely for the isolation-technology choice: this harness's entire purpose is running LLM-directed, potentially attacker-influenced code inside every single invocation — that's not an edge case to escalate to on demand, it's the default workload. Current guidance on sandboxing AI agents is explicit that standard multi-tenant containers (shared host kernel, namespace/cgroup isolation) are not considered adequate containment for arbitrary LLM-generated code; stronger isolation (gVisor or a microVM) is the baseline recommendation, not an upgrade path reserved for flagged-risky tasks. That directly overturns the tiered "Docker by default, escalate for high-risk" framing floated as a possibility in ADR-1's open questions — see Decision.

## Decision Drivers

- **Containment & recoverability** — a fast, reliable kill switch, independent of the agent's own cooperation (ADR-0).
- **Security exposure, worst-case framing** — every control here must hold assuming the agent is fully compromised, not assuming it behaves as intended.
- **NHI credential lifecycle** — the orchestrator's own minting credential (ADR-2) is a concentrated high-value target and needs short-lived, auto-rotated storage, not a static secret.
- **Tool / MCP supply-chain risk** — this is where ADR-3's tool/MCP allowlist decision actually gets enforced, not just decided.
- **Cost containment** — isolation technology has a real resource/latency cost; the choice needs a cost argument, not just a security one.

## Options Considered

### Isolation mechanism

**1. Plain Docker (namespaces/cgroups) as the default, escalate to stronger isolation for flagged-risky tasks.** Rejected as the default. This was the direction ADR-1 speculated might be needed ("likely needs to vary by risk tier"), but current sandboxing guidance is specific: shared-kernel containers are not adequate isolation for a workload whose entire point is executing LLM-directed, potentially attacker-influenced actions — which describes every invocation this harness runs, not a risky subset of them. Treating strong isolation as an upgrade undersells ADR-0's own "contain regardless of whether the agent is buggy or hijacked" principle by making the default case the weakly-contained one.

**2. gVisor (user-space kernel intercepting syscalls) for everything.** Viable — meaningfully stronger than plain containers, doesn't require KVM, and integrates more simply into more deployment environments. Kept as the designated fallback rather than the default (see Decision) because it isolates in userspace rather than at the hardware boundary, and this harness can afford the stronger option's cost.

**3. Firecracker microVM (dedicated lightweight VM, own kernel, hardware-enforced via KVM) for every agent container — chosen as default.** Strongest available isolation — a genuine kernel/hardware boundary below the layer the agent can reason about or attempt to escape from computationally. Historically the objection to microVMs is boot latency, but Firecracker boots in roughly 125ms, which is negligible against the clone/dependency-install/LLM-inference latency that already dominates a one-shot invocation's wall-clock time (ADR-1). There's no meaningful cost argument left for a weaker default. **Measured directly, 2026-09-04** ([ADR-10](week2-adr-10-microvm-launch-and-lifecycle-mechanics.md)): the ~125ms figure measures a narrower window than "boot to a usable login shell," which this project's own host measured at ~1.3s (dominated by systemd unit startup, not Firecracker's own launch latency) — still negligible against the ~79–86s a real end-to-end coding-role invocation actually took, so this doesn't change the argument above, just sharpens the number.

## Decision

### Isolation: Firecracker microVM by default, gVisor as the documented fallback

Every agent container runs inside a Firecracker microVM, uniformly — no risk-tier-based tiering of the isolation mechanism itself (ADR-3's risk tiers still drive policy decisions like approval-required and quota, just not which sandbox technology is used; isolation strength doesn't get to be the thing that's cheaper for "trusted" tasks, because no invocation is trusted by design). Where the orchestrator's own host can't provide KVM/nested virtualization (a real constraint on deployment target, not a hypothetical one), gVisor is the documented fallback, accepting weaker-than-hardware isolation as a explicit, logged exception rather than silently degrading to plain containers.

### Network egress: default-deny plus an explicit, logged allowlist proxy

Every container's network path is default-deny. The orchestrator injects a per-invocation allowlist — the git hosting platform's endpoint (ADR-6), the specific package registries the repo's build tooling actually needs, and any MCP/tool-server endpoints ADR-3's policy decision allowlisted for this invocation — enforced via a forward proxy the container is configured to use, backed by a network-policy rule that blocks any egress that doesn't go through the proxy (so a tool or library that ignores proxy environment variables still can't reach the open internet). The proxy logs every attempted connection, including denied ones — a denied-connection attempt from inside a running container is treated as a strong signal of either a bug or a hijack attempt, and feeds directly into ADR-5's alerting once that layer exists.

### Orchestrator's own minting credential: OpenBao

The orchestrator's credential (ADR-2, the thing capable of minting per-invocation scoped tokens) is stored and leased through **OpenBao** rather than HashiCorp Vault. Vault's open-source edition moved to the Business Source License in 2023, which restricts competing hosted/embedded offerings; OpenBao is a Linux Foundation–governed fork taken from Vault's last MPL-2.0 release, is Vault-API-compatible, and remains genuinely free/open-source with no commercial-use restriction — the same governance lens already applied to Forgejo-over-Gitea and to calling the policy engine via AuthZEN rather than coupling to OPA specifically. OpenBao's native dynamic-secret leasing and rotation directly satisfy ADR-2's requirement that this credential be short-lived and auto-renewed rather than a static secret — the harness doesn't need to hand-roll that machinery.

**Local development note (non-production):** the unseal process and HA topology this ADR flags as operational overhead are production concerns only. OpenBao ships a built-in `-dev` server mode (`bao server -dev`, or the official Docker image with `BAO_DEV_ROOT_TOKEN_ID` set) that is auto-initialized and auto-unsealed — a single process, no cluster, no unseal ceremony — purpose-built for exactly this kind of local testing. It's explicitly unsuitable for production (in-memory, data lost on restart), but that ephemerality is a feature for a throwaway dev loop, not a workaround. It also exercises the real leasing/rotation API, unlike lighter alternatives (a `.env` file, or SOPS+age for encrypted-file-at-rest with no server process) — those are only useful before credential rotation itself is what's being tested.

### Kill switch: two granularities, matching ADR-2's revocation model

- **Per-invocation**: the orchestrator sends the container a kill signal (SIGKILL if it doesn't exit cleanly) and, in parallel, explicitly calls the git hosting platform's token-revocation endpoint (ADR-6) for that invocation's token — belt-and-suspenders, so a token that somehow already leaked outside the container doesn't remain valid on the strength of the container merely being dead. First-pass target: **sub-60-second p95** — this is just a container kill plus one revocation API call, no redeploy involved.
- **Global**: the orchestrator revokes/rotates its own OpenBao lease and flips a circuit-breaker flag it checks before launching any new container — no orchestrator redeploy required for this to take effect. Already-running containers still get killed individually. First-pass target: **sub-5-minute p95**.

Both targets are first-pass estimates to validate once real infrastructure exists, not measured numbers — flagged in Open Questions.

### Defense-in-depth, stated explicitly

None of the controls in this ADR assume the agent behaves correctly or honestly. The full stack — Firecracker's hardware isolation, the default-deny egress proxy, ADR-2's narrow credential scope, ADR-1's one-shot lifecycle, and this ADR's kill switch — is layered so that a fully compromised or prompt-injected agent process is still contained by mechanisms it cannot reason about or talk its way past, because none of them live at the level the agent operates at.

### Local development note (non-production)

Production deployment target is a Linux host, so the Firecracker decision above stands unmodified — this note doesn't change it. It exists only because Firecracker requires KVM, which macOS doesn't have, and local development happens on an Apple Silicon Mac. For that dev/test loop only, substitute Apple's Virtualization.framework-based tooling (Apple's own open-source Containerization framework, or libkrun via Podman Desktop) as a local stand-in for the isolation layer — not a second production path, just enough to run and test the orchestrator and container lifecycle locally before deploying to the real Linux target, where Firecracker is what actually runs.

## Consequences

**Positive**
- A single, uniform isolation posture removes an entire class of misconfiguration risk ("did we forget to mark this repo high-risk") that a tiered isolation model would have introduced.
- OpenBao gives ADR-2's short-lived/auto-rotated requirement for the orchestrator's credential natively, rather than requiring bespoke rotation logic.
- Two-tier kill switch is fast and doesn't require a redeploy at either granularity.
- The egress proxy's log of denied attempts is a free, high-signal detection mechanism for exactly the "agent tried to do something it shouldn't" scenario ADR-0 is most worried about.

**Negative / trade-offs**
- Firecracker requires KVM/nested virtualization on whatever host runs the orchestrator's container-launching infrastructure — a real constraint on deployment target, not available on an unmodified environment without nested virtualization support.
- OpenBao adds real operational overhead (unseal process, HA topology) relative to a simpler secret store; justified by ADR-2's rotation requirement, but a genuine cost to stand up.
- The egress proxy and its network-policy fallback are two mechanisms that must be kept in sync — a gap between them defeats the "can't bypass the proxy" guarantee.
- Kill-switch timing targets above are estimates, not measurements — they need validation once real infrastructure exists, and could be optimistic.

## Open Questions

- OpenBao unseal/HA topology for a self-hosted deployment — spike needed.
- Whether a lighter-weight secret store (e.g. SOPS + age) is worth using for early prototyping before OpenBao is stood up, given its operational overhead.
- Validate the sub-60-second / sub-5-minute revocation targets against real infrastructure once built.
- Exact egress-proxy mechanics (transparent proxy with iptables redirect vs. an explicit proxy environment variable) — implementation detail, but must pair with the network-policy fallback regardless of which is chosen, so a tool that ignores proxy env vars still can't reach anything.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- [ADR-2: Agent Identity & Permission Model](week1-adr-2-agent-identity-and-permission-model.md)
- [ADR-3: Authorization / Policy Enforcement Layer](week1-adr-3-policy-enforcement-layer.md)
- "Your Container Is Not a Sandbox: The State of MicroVM Isolation in 2026" — https://emirb.github.io/blog/microvm-2026/
- "How to sandbox AI agents in 2026: Firecracker, gVisor, runtimes & isolation strategies" — https://manveerc.substack.com/p/ai-agent-sandboxing-guide
- Northflank, "Firecracker vs gVisor: Which isolation technology should you use?" — https://northflank.com/blog/firecracker-vs-gvisor
- Northflank, "Firecracker vs Docker: key differences and when to use each" — https://northflank.com/blog/firecracker-vs-docker
- Edera, "Kata, gVisor, or Firecracker? Container Isolation Guide" — https://edera.dev/stories/kata-vs-firecracker-vs-gvisor-isolation-compared
- wetheflywheel, "OpenBao vs HashiCorp Vault: the Open-Source Fork, Compared (2026)" — https://wetheflywheel.com/en/comparisons/openbao-vs-hashicorp-vault/
- DuoKey, "HashiCorp Vault BSL License Change: Migrate to OpenBao" — https://duokey.com/en/resources/hashicorp-vault-bsl-license-change-openbao-migration
