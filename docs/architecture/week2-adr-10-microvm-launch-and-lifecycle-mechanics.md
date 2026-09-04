# ADR-10: MicroVM Launch & Lifecycle Mechanics

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-09-04 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Developer Experience |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Implements the concrete launch/lifecycle mechanics [ADR-4](week1-adr-4-blast-radius-and-sandboxing.md) deferred (Firecracker chosen there as the default isolation technology; this ADR covers *how* it's actually launched, credentialed, networked, and torn down); resolves the token-in-logs finding the [ADR-8 PoC](../plans/openhands-localruntime-validation.md) flagged as unresolved; is `ContainerLauncher`'s production implementation, explicitly deferred by [ADR-9](week2-adr-9-orchestrator-foundations.md#c-orchestrator-interfaces-single-implementation-test-seams-not-multi-backend-abstraction); validated end-to-end, including under real OpenHands (ADR-8), against the real local Forgejo instance (ADR-6), by the [Firecracker MicroVM Launch PoC](../plans/firecracker-microvm-launch-poc.md) |

## Context

ADR-4 decided Firecracker microVMs are the default isolation boundary for every agent invocation, citing a ~125ms boot-latency figure from published benchmarks as the reason boot cost is not a disqualifying trade-off. It left every operational detail of *how* a Firecracker microVM actually gets launched, credentialed, networked, logged, and torn down as future work — not because those details are unimportant, but because ADR-4's job was choosing the isolation technology, not operating it.

Those details turned out not to be safe to assume by analogy from ADR-4's container-shaped design or from the ADR-8 PoC's macOS Containerization-framework substitute. A microVM has no shared kernel with the host to hook policy into, no bind-mount to inject a credential file through, and no container-runtime CLI with a mature ecosystem of flags for cgroups/capabilities — every one of ADR-4's mechanisms (egress allowlist, credential injection, kill switch, audit-log extraction) needed a genuinely different, Firecracker-specific answer. The [Firecracker MicroVM Launch PoC](../plans/firecracker-microvm-launch-poc.md) ran all of this hands-on, on the actual Linux production target (not the macOS dev substitute ADR-8 used), including the real OpenHands coding-role loop (ADR-8) against the real local Forgejo instance (ADR-6). This ADR is written directly from that PoC's results, not from documentation or assumption.

## Decision Drivers

- **Hardware-enforced isolation must survive contact with the actual mechanics** (ADR-4) — a design that sounds right on paper but leaks a credential, fails to enforce egress, or loses audit data in practice is not the isolation ADR-4 decided on.
- **Credential injection must never touch a boot-time or image-baked surface** (ADR-1/ADR-2) — the ADR-8 PoC's own finding (a token visible via `${FORGEJO_TOKEN}` in a shell/log stream) is the concrete failure mode to close, not just avoid in theory.
- **The kill switch must be fast and unilateral, but its actual durability guarantees must be stated precisely** (ADR-1's "destruction is the kill switch," ADR-4's sub-60s p95 target) — not asserted without having watched what a hard kill actually does to in-flight writes.
- **Audit-log extraction must not depend on the same path egress policy controls** — a network-level lockout or failure must not also blind the orchestrator to what happened.
- **Reuse over re-invention, proportional to actual need** (ADR-0) — prefer Firecracker's own purpose-built mechanisms (MMDS, vsock, jailer) over hand-rolled equivalents, and don't build tooling (e.g. a wrapper library) this pilot doesn't need yet.
- **Host-compatibility assumptions must be checked on the actual target, not the reference documentation** — cgroup v1 vs v2 is exactly the kind of assumption that fails silently until run.

## Options Considered

### A. Process launch: bare Firecracker vs. jailer

**A1. Bare Firecracker (no privilege-dropping wrapper) — considered, not chosen for production.** Simpler to launch (no chroot/uid-drop setup, no cgroup-version handling), and the PoC used it for Steps 7–9 for exactly that reason — reducing retry friction while iterating on an LLM-driven task. But it runs as whatever uid launched it, in no additional chroot, under whatever cgroup (or none) the caller happens to be in — none of the defense-in-depth jailer adds beyond Firecracker's own KVM boundary.

**A2. `jailer`-wrapped Firecracker — chosen.** Verified via the PoC (Step 5), not assumed: the resulting Firecracker process runs as a dedicated non-root uid/gid with zero effective capabilities (`CapEff: 0000000000000000`), inside a real chroot (a self-contained jail directory containing only the Firecracker binary, a minimal `/dev`, and the API socket — confirmed via direct directory listing, not the misleading `readlink /proc/<pid>/root`), under a dedicated cgroup that actually enforces the configured resource value (`cpu.weight` read back as set, not silently ignored). A concrete, unplanned isolation benefit also surfaced: the jail's directory permissions (`0700`, owned only by the dedicated uid) block even the API socket from being reached by a different unprivileged host user — defense-in-depth beyond what bare Firecracker provides even before the guest boots.

### B. cgroup version handling

**B1. Rely on jailer's default (`--cgroup-version 1`) — rejected.** Fails hard and immediately on a pure-cgroup-v2 host (confirmed: this project's own host has no cgroup v1 hierarchy mounted at all): `Error: CgroupHierarchyMissing("No hierarchy found for this cgroup version.")`. This is not a hypothetical compatibility concern — it reproduced on the first real attempt.

**B2. Always pass `--cgroup-version 2` explicitly — chosen.** Verified working end-to-end on this host. Since modern Linux distributions increasingly ship cgroup-v2-only by default, this is not a defensive fallback for an edge case; it is the expected common case going forward and must be the default in any launcher, not an option.

### C. Credential injection channel

**C1. Environment variable at container/process start, as the ADR-8 PoC's macOS substitute used — rejected as the production mechanism.** This is the exact mechanism the ADR-8 PoC flagged as leaving a token visible via `${FORGEJO_TOKEN}` substitution in the agent's own visible action/observation stream — an artifact of the credential's origin being a plain env var the agent's shell tool naturally echoes back while constructing commands.

**C2. MMDS (Firecracker's Microvm Metadata Service), V2 session-token handshake — chosen.** The guest must `PUT` for a short-lived session token before it can `GET` any data — verified this blocks both an unauthenticated request and a forged-token request (`401` both times), so the guest cannot bypass or forge access. Verified the token is genuinely absent from every boot-time/process-argument surface checked: `/proc/cmdline`, guest `dmesg`, the guest journal, and the host's own console-log capture. The credential only ever exists in the MMDS response body the guest explicitly fetches at runtime.

### D. Log/audit extraction channel

**D1. Reuse the egress-network path (proxy or direct connection) to ship logs out — rejected.** Makes audit-log extraction dependent on the exact network path the egress policy polices; a lockout or network-level failure during a run would also blind the orchestrator to what happened, precisely when visibility matters most.

**D2. virtio-vsock, a channel independent of the guest's network stack — chosen.** Verified independent, not just theoretically separable: a microVM booted with *no* network interface and *no* MMDS config at all still delivered all 20 test log records to the host, in order and complete. In the real OpenHands run (Step 7), the `--json` event log streamed over this same channel captured the full action/observation sequence for independent, out-of-band verification — used directly to catch a silent OpenHands failure mode (see Consequences).

### E. Network egress design

**E1. A container-network-namespace-style NAT/MASQUERADE setup, mirroring ADR-4's original container framing — rejected as unnecessary.** A tap-device deployment doesn't need it: the guest's only legitimate path out is a direct connection to the host's own tap-gateway IP (where a forward proxy listens), which is host-terminated traffic through `INPUT`, never `FORWARD`.

**E2. A single unconditional nftables rule dropping (and logging) all `FORWARD`-chain traffic from the tap device, with no NAT rule at all — chosen.** Verified doing exactly what it should, including a case this design didn't originally anticipate: a guest connecting *directly* to the host's tap-gateway IP on Forgejo's published port (bypassing the proxy) still got blocked, because Docker's own port-publishing DNAT rewrites that connection's destination to the container's bridge IP and routes it through `FORWARD` — which superficially looks like "the host itself" but is not host-terminated traffic. **Concrete consequence for any launcher built on this design**: every host-side service the guest needs — including ones that look local, like a Dockerized Forgejo instance — must be reached via the proxy, not assumed reachable directly just because it's "on the host."

## Decision

Adopt the following as the production mechanics for every Firecracker-isolated agent invocation:

1. **Launch via `jailer`**, always with `--cgroup-version 2` explicit (never relying on the default), a dedicated per-invocation uid/gid, and a `--chroot-base-dir` the launcher pre-creates. Resource files (kernel, rootfs) referenced into the jail must be **copied**, not hardlinked, if the launcher will ever `chown` them post-link — hardlinking and then `chown`ing mutates the shared source file's ownership too, since a hardlink shares one inode with its source (a real bug hit and fixed mid-PoC).
2. **Inject every credential via MMDS**, V2 session-token handshake only (never V1's unauthenticated GET), and never as a boot-time argument, kernel command-line value, or process environment variable at the Firecracker/jailer invocation layer.
3. **Extract logs/audit data via vsock**, configured and read independently of whatever network-interface/MMDS configuration a given invocation has, so log visibility never depends on network health.
4. **Enforce egress via a single host-terminated-proxy-only path**: one tap device per invocation, a host-side forward proxy bound to the tap gateway IP, and one unconditional `FORWARD`-chain drop+log rule for that tap device — no NAT/MASQUERADE rule. Every destination the guest legitimately needs, including host-local services published via Docker, goes through the proxy; nothing is assumed to be reachable "directly" merely because it resolves to a host-owned address.
5. **Kill switch is a direct `SIGKILL`** to the Firecracker (or jailer-wrapped Firecracker) process — no guest cooperation required, ~26ms kill-to-gone measured. **Explicitly do not treat this as a durability guarantee for unflushed writes**: anything that must survive a kill-switch invocation (a partial commit marker, an audit-trail tail) needs an explicit `fsync`/`sync` at the point it becomes must-survive, not merely "written" — a hard kill was observed to lose ~13.6 seconds of unsynced appends to a workspace drive while preserving data that had been explicitly synced moments before.
6. **Host-side per-invocation resources are not self-cleaning on kill** and must be explicitly torn down by the launcher: the tap device, the API-socket file, and the console/stdin FIFO were all observed to survive a killed instance untouched. A shared resource (the forward proxy) is correctly unaffected by a single invocation's kill, by design.
7. **Each invocation gets a fresh copy-on-write layer over the golden rootfs image, not a shared mutable image reused across boots.** This was *not* how the PoC's own Steps 7–9 were run (all three boots shared one mutated `ubuntu-24.04.ext4`), and that gap was directly observed causing a stale-workspace collision in Step 8 that the coding agent happened to recover from on its own — a real launcher must not depend on that. This is a genuine implementation gap this ADR flags rather than papers over, not a resolved design point.
8. **Snapshot/restore (Firecracker's guest-state snapshotting) is not adopted for v1**, documented as a future optimization only. Measured cold-boot-to-login time (~1.3s) is roughly 1–2% of a real invocation's end-to-end wall clock (~79–86s, dominated by clone/dependency/LLM-inference cost per ADR-1) — even a near-zero-latency restore would not move the number that matters, and a snapshot cannot include the just-cloned repo regardless. Revisit only if guest boot time becomes an independently measured bottleneck (e.g. very high invocation concurrency).

## Consequences

**Positive**
- Every mechanism here is verified against real behavior on the actual Linux production target, not inferred from documentation or carried over from the ADR-8 PoC's macOS substitute — including one host-compatibility failure (cgroup v1 default) that would otherwise have reproduced on first real deployment.
- The ADR-8 PoC's token-in-logs finding is meaningfully improved, not merely reasserted as fixed: MMDS closes the boot-time/image-baked leak vector completely (verified absent from every checked surface), and the real end-to-end run (Step 7) showed the credential did not appear anywhere in the Forgejo-visible artifacts (PR title, body, diff, or commit messages) it produced. **Not fully closed** — see Open Questions.
- The kill switch, egress design, and log-extraction channel all held up under a real OpenHands coding-role loop against a real Forgejo instance, not just synthetic test payloads — including catching one silent OpenHands failure mode (headless mode ignoring `LLM_API_KEY`/`LLM_MODEL` without `--override-with-envs`, exiting 0 with no work done) precisely because the vsock-captured event log was independent of the agent's own self-report.
- The egress design's single-drop-rule simplicity held up under a case it wasn't originally designed for (a Dockerized host-local service), rather than needing rework — a sign the underlying design principle (host-terminated-only path) generalizes correctly.

**Negative / trade-offs**
- **Per-invocation fresh-image provisioning is not yet built** (Decision point 7) — this PoC ran against a shared, mutating rootfs image across "fresh" boots, which is not representative of a real launcher and already produced one observed collision. This is real, not-yet-done work before production, not a solved problem being restated.
- **The token-in-shell-stream nuance from the ADR-8 PoC is improved but not conclusively eliminated**: OpenHands' coding agent still constructs and logs shell commands referencing the credential symbolically (e.g. `${FORGEJO_TOKEN}` in a `git clone` command string); this PoC did not exhaustively audit the full vsock-captured event stream byte-for-byte for the literal token value, only the Forgejo-visible output artifacts. Treat as "not observed to leak in this PoC," not "proven impossible."
- **jailer requires a root-capable launch path.** jailer itself must run as root to perform the chroot/cgroup/uid-drop setup before exec'ing into Firecracker — this is inherent to its design, not a gap to close, but it means the orchestrator's `ContainerLauncher` implementation needs a privileged execution context somewhere in its own deployment, which needs to be accounted for in how the orchestrator itself is deployed (ADR-9's `ContainerLauncher` interface abstracts this away from orchestrator control-flow code, but the concrete production implementation still needs it).
- **Host-side per-invocation cleanup (tap device, socket/FIFO files) is launcher responsibility, not automatic** — omitting it leaks host resources across many invocations; this needs to be built as an explicit teardown step, not assumed to come for free from killing the guest process.
- The ~125ms boot figure ADR-4 cited turns out to measure something narrower than "boot to usable login" (which this PoC measured at ~1.3s, still fast in absolute terms and dominated by systemd unit startup rather than Firecracker's own launch latency) — not a contradiction of ADR-4's cost argument (~1.3s is still negligible against the ~79–86s dominant cost), but worth citing the PoC's own measured number going forward rather than the narrower published figure.

## Open Questions

- Design and build the per-invocation copy-on-write rootfs layer (Decision point 7) — the concrete next implementation task this ADR identifies as a genuine gap, not deferred speculation.
- Whether to exhaustively audit OpenHands' vsock-captured event stream for literal credential-value leakage (not just the Forgejo-visible output this PoC checked) before treating the ADR-8 token-in-logs finding as fully closed.
- Host-side teardown automation (tap device, API socket, console FIFO) as part of `ContainerLauncher`'s implementation — identified as necessary, not yet built.
- Where the jailer-requires-root constraint sits in the orchestrator's own deployment/privilege model — needs an answer before `ContainerLauncher`'s production implementation is written, not before this ADR.
- Revisit snapshot/restore (Decision point 8) only if guest boot time is independently measured as a real bottleneck, not preemptively.

## References

- [ADR-4: Blast Radius & Sandboxing](week1-adr-4-blast-radius-and-sandboxing.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- [ADR-2: Agent Identity & Permission Model](week1-adr-2-agent-identity-and-permission-model.md)
- [ADR-6: Git Hosting Platform Selection](week1-adr-6-git-hosting-platform-selection.md)
- [ADR-8: Coding-Agent Runtime Selection](week1-adr-8-coding-agent-runtime-selection.md)
- [ADR-9: Orchestrator Foundations](week2-adr-9-orchestrator-foundations.md)
- [Plan: Firecracker MicroVM Launch PoC](../plans/firecracker-microvm-launch-poc.md) — the PoC this ADR is written directly from (run 2026-09-03/04, Steps 1–9 passed, Step 10 deliberately skipped)
- [Plan: OpenHands LocalRuntime Validation (ADR-8 PoC)](../plans/openhands-localruntime-validation.md) — source of the token-in-logs finding this ADR addresses
- [Study guide: Firecracker MicroVM PoC](../plans/firecracker-poc-study-guide.md) — pedagogical write-up of Steps 1–6's mechanics
- Firecracker Docs, MMDS — https://github.com/firecracker-microvm/firecracker/blob/main/docs/mmds/mmds-user-guide.md
- Firecracker Docs, jailer — https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md
- Firecracker Docs, Snapshotting — https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md
