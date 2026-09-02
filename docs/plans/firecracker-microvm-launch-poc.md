# Plan: Firecracker MicroVM Launch & Lifecycle PoC (pre-ADR-10)

| | |
|---|---|
| **Status** | Draft |
| **Date** | 2026-09-02 |
| **Owner** | Krzysztof Niemiec |
| **Purpose** | Generate hands-on evidence for the microVM launch/lifecycle mechanics [ADR-4](../architecture/week1-adr-4-blast-radius-and-sandboxing.md) assumed but never specified — before drafting ADR-10 (MicroVM Launch & Lifecycle Mechanics), not yet written. Unlike the [OpenHands LocalRuntime PoC](openhands-localruntime-validation.md), this doesn't validate an already-decided approach; it's exploratory, feeding the options ADR-10 will choose between. |

## Goal

Answer, with evidence, the concrete questions ADR-4 left open and ADR-9 explicitly deferred: can the orchestrator launch a real Firecracker microVM (not the macOS dev-loop substitute already validated), inject a scoped credential into it without baking it into an image, run OpenHands' full coding-role loop inside it against Forgejo, extract logs/audit data back out, enforce the egress allowlist at the tap-device level, and tear it down as a genuine kill switch — and at what mechanical cost (jailer vs. bare Firecracker, image build process, snapshot/restore).

**Hard prerequisite, unlike every prior PoC in this series**: this requires a Linux host with hardware virtualization (`/dev/kvm` present and usable) — Firecracker cannot run on the macOS dev machine used for the ADR-8 PoC, nested virtualization or not. This is why this is a separate, later piece of work rather than something run alongside ADR-9.

This plan is split into two perspectives on purpose, matching the ADR-8 PoC's convention: **developer** is what to actually run, **architect** is how to read the results and what each outcome means for ADR-10's eventual decision.

## Prerequisites

- A Linux host (bare-metal, or a cloud instance with nested virtualization / bare-metal instance type explicitly enabling KVM — a standard shared-kernel cloud VM without KVM passthrough will not work) — confirm `ls -l /dev/kvm` and `[ -r /dev/kvm ] && [ -w /dev/kvm ]` before anything else.
- `firecracker` and `jailer` binaries for the host's architecture (x86_64 or arm64), from the [firecracker-microvm/firecracker](https://github.com/firecracker-microvm/firecracker) releases.
- A guest kernel image (uncompressed `vmlinux`) and a minimal guest rootfs (ext4 image) containing: a init process, `git`, `curl`, Python/`uv`, and OpenHands headless installed — this is the first real answer to ADR-1's still-open "image build/maintenance process" question, so document exactly how this image was built, not just that it works.
- A second, separate writable ext4 image to attach as a per-invocation workspace/overlay drive, kept distinct from the base rootfs.
- Local Forgejo instance reachable from the host (same instance/setup as the ADR-8 PoC, or a fresh equivalent) — plus the same two-bot-account pattern (write-scoped `bot-coder`, read-only `bot-reviewer`) and a disposable test repo.
- Host-side tooling to create a tap device, a bridge/NAT rule set, and a minimal forward proxy (even a throwaway `tinyproxy`/`mitmproxy` instance is fine for this PoC) to approximate ADR-4's egress-allowlist proxy.
- `strace`/`ps`/cgroup tooling on the host to inspect what the jailer actually restricts.

## Developer Perspective — PoC Execution Steps

### Phase 1 — Bare mechanics (no OpenHands yet)

#### 1. Boot a bare microVM
- Launch `firecracker` directly (no jailer yet) via its API socket: configure `/boot-source`, `/drives` (rootfs), `/machine-config`, start the instance.
- Confirm guest boots to a login/shell, `uname -a` shows a genuinely separate kernel, and boot-to-ready wall-clock time is recorded (this is the number ADR-1's cold-start concern and ADR-4's "~125ms" claim should be checked against directly, not just cited).

#### 2. Networking + egress-allowlist approximation
- Create a host-side tap device, attach it via `/network-interfaces`, bring up guest networking.
- Configure the guest (or the tap/bridge) so all outbound traffic must route through the host-side forward proxy; add an nftables/iptables rule on the host blocking any tap-device egress that doesn't go through the proxy, mirroring ADR-4's container-network design but at the tap-device layer instead of a container network namespace.
- Verify: guest can reach Forgejo + a package registry through the proxy; a direct connection attempt bypassing the proxy is blocked and logged — this is the concrete mechanism ADR-4 assumed without specifying for a microVM.

#### 3. Credential injection via MMDS
- Before starting the instance, `PUT` a scoped Forgejo token into MMDS via the Firecracker API; configure the relevant network interface to allow MMDS requests.
- From inside the guest, `GET` the token from the MMDS link-local endpoint and confirm it matches — this is the mechanism check for ADR-1/ADR-2's "injected at launch, never baked into the image" requirement, using Firecracker's own purpose-built channel (host-determined, guest cannot tamper with or forge it) rather than an ad hoc env-var/file hack.

#### 4. Log/audit extraction via vsock
- Configure a virtio-vsock device; have a small guest-side agent (or OpenHands' own log output, piped) write structured log lines to the vsock port; confirm the host-side listener receives them in order and complete.
- Confirm this path works with networking fully absent (stop the tap device) — the point is that audit-record extraction should not depend on the same network path the egress proxy polices, so a network-level failure or lockout doesn't also blind the orchestrator.

#### 5. Jailer vs. bare Firecracker
- Repeat Step 1's boot under the `jailer` wrapper instead: dedicated non-root uid/gid, `--chroot-base-dir`, and confirm via `ps`/cgroup inspection that the resulting Firecracker process runs unprivileged, chrooted, and under its own cgroup — not just "the jailer ran without erroring."
- Record any additional setup cost (chroot directory structure, cgroup v1 vs v2 compatibility on this host) versus bare Firecracker from Step 1.

#### 6. Kill-switch / teardown
- Mid-boot or mid-workload, kill the Firecracker (or jailer-wrapped) process directly.
- Confirm: the tap device and any host-side proxy state are cleaned up (or need explicit cleanup — record which), the workspace/overlay drive's changes never touched the base rootfs image, and time the kill-to-gone latency against ADR-4's sub-60-second p95 target.

### Phase 2 — Full coding-role loop, real Firecracker

#### 7. Core loop under real Firecracker
- Repeat the ADR-8 PoC's Step 3 exactly (clone → edit → run tests → commit → push → open PR against Forgejo), but with OpenHands (`RUNTIME=process`/LocalRuntime, per ADR-8) running *inside* this Firecracker guest instead of the macOS Containerization-framework substitute.
- Use the MMDS-injected token from Step 3, the vsock log path from Step 4 for capturing the `--json` event log, and the egress-proxy setup from Step 2 for network reachability.
- This is the direct parity check the ADR-8 PoC's own "Out of Scope" section flagged as a separate, later exercise — confirm the coding-role loop's outcome (PR created, correct branch, correct auth) matches what was already observed under the macOS substitute.

#### 8. Round-2 / update-PR behavior
- Same as the ADR-8 PoC's Step 4: a second, fresh microVM (fresh boot, fresh clone) addressing feedback on the PR from Step 7, confirming update-not-duplicate under real Firecracker.

#### 9. Token-scope negative test
- Same as the ADR-8 PoC's Step 7, rerun here: `bot-reviewer`'s read-only token should fail the push cleanly. This is less about discovering something new (already confirmed once) and more about confirming no regression from the substitute-to-real-Firecracker environment change.

### 10. Stretch goal — snapshot/restore latency
- Snapshot the guest *after* the kernel has finished booting and OpenHands is idle-ready (snapshotting before that point is documented to risk crashing on restore) — take the snapshot once, then restore from it for a fresh Step 7 run instead of a cold boot.
- Compare restore-to-ready latency against Step 1's cold-boot number. Current upstream figures put snapshot-restore in the single-digit-to-tens-of-milliseconds range — worth confirming on this host rather than assuming the published numbers transfer, and worth weighing against ADR-1's already-dominant cold-start cost (clone + dependency install + LLM inference), which may make this optimization not worth its added complexity (a snapshot is a fixed point — it doesn't include the just-cloned repo, so this only ever shaves *guest boot* time, not the invocation's actual dominant cost).

### Deliverable
A results log — pass/fail and measurements per step — appended to this plan's **Results** section, following the same append-don't-rewrite convention as the ADR-8 PoC.

## Architect Perspective — Validation & Decision Criteria

### What each step is actually checking

| Step | Validates |
|---|---|
| 1 | Firecracker actually boots on this host and what real (not documented) boot latency looks like. |
| 2 | Whether ADR-4's egress-allowlist design translates to a tap-device network without redesign. |
| 3 | MMDS is a workable, tamper-resistant credential-injection channel matching ADR-1/ADR-2's requirement. |
| 4 | A log-extraction path exists that doesn't depend on the same network route the egress policy controls. |
| 5 | Whether jailer's added setup cost is worth it for the hardening it provides, or whether it's disproportionate for this deployment. |
| 6 | ADR-1's "destruction is the kill switch" and ADR-4's timing targets hold for a real microVM, not just a container substitute. |
| 7–9 | Parity: does everything the ADR-8 PoC already confirmed under the macOS substitute still hold under real Firecracker. |
| 10 | Whether snapshot/restore is worth adopting given ADR-1's cold-start trade-off is dominated by clone/install/inference, not guest boot. |

### Decision tree

- **Steps 1–9 all pass** → draft ADR-10 adopting: jailer-wrapped Firecracker (unless Step 5 found jailer materially costlier with no offsetting benefit — then bare Firecracker with the reasoning documented), MMDS for credential injection, vsock for log extraction, and the tap-device egress design from Step 2. Step 10's result decides whether snapshot/restore is in v1 or a documented future optimization.
- **Step 2 or 4 fails or proves substantially harder than expected** → this is the actual hard part of ADR-10; treat it as the primary open design problem rather than a footnote, and consider whether an existing higher-level wrapper (`firecracker-containerd`, Kata Containers) is worth adopting instead of hand-rolling this layer — the same "reuse vs. build bespoke" question ADR-1 already asked about the orchestrator itself, applied one layer down.
- **Step 7 fails where the equivalent ADR-8 PoC step passed** → the divergence is specifically attributable to the real-Firecracker environment (networking, MMDS-sourced credential, or vsock logging) — isolate which of Steps 2–4 it traces back to before concluding anything about OpenHands or Forgejo.
- **Step 3 (MMDS) fails to keep the token out of guest-visible logs cleanly** → revisit the ADR-8 PoC's flagged finding (the token appeared in a shell/log stream via `${FORGEJO_TOKEN}`) — if MMDS doesn't actually improve on that, that finding stands unresolved and needs its own mitigation regardless of which launch mechanism is chosen.

### Non-functional checks worth judgment, not just pass/fail

- Whether jailer's chroot/cgroup setup is a one-time host-provisioning cost or something that has to be redone per invocation — materially affects ADR-1's per-round provisioning-speed concern.
- Image build/maintenance burden observed while constructing the guest rootfs in Prerequisites — feed this directly back into ADR-1's still-open "image build/maintenance process" question.
- Whether a higher-level wrapper (firecracker-containerd, Kata) would have made Steps 2–6 meaningfully easier — informs the decision-tree branch above even if not run in this PoC.

### Documentation follow-through

1. Append results to this plan's **Results** section — don't rewrite the steps.
2. Draft ADR-10 (MicroVM Launch & Lifecycle Mechanics) from the results, using the same Options-Considered/Decision structure as the rest of the series.
3. If Step 3's MMDS test doesn't resolve the token-in-logs finding from the ADR-8 PoC, carry that forward explicitly into ADR-10 rather than treating it as closed.

## Results

_(none yet — pending execution on a Linux host with KVM)_

## Success Criteria

- Clear pass/fail and timing recorded for Steps 1–9 (and 10, if run).
- Enough evidence to write ADR-10's Decision section from real measurements, not documentation claims.
- The token-in-logs finding from the ADR-8 PoC is either resolved by MMDS or explicitly carried forward as still-open.

## Out of Scope

- gVisor validation (ADR-4's documented fallback where KVM is unavailable) — separate exercise, not needed to answer this plan's Firecracker-specific questions.
- Production-scale multi-tenant host provisioning (how many microVMs per host, resource limits across concurrent invocations) — single-invocation validation only, matching the ADR-8 PoC's own scope limit.
- Building ADR-1's actual orchestrator or a real `ContainerLauncher` implementation — this plan hand-drives the steps a real launcher would automate.
- The deterministic-scanner pipeline for the security-triage role — unrelated to this plan's question.
