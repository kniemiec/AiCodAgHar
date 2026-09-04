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

### 2026-09-03/04 — Run 1 (complete: Prerequisites + Steps 1–9 done, Step 10 deliberately skipped)

**Environment set up for this run:**
- Linux host with `/dev/kvm` present and read/write. `firecracker`/`jailer` v1.16.1 (x86_64) in `~/firecracker-poc/bin/`.
- Guest kernel `vmlinux-6.18.44` and guest rootfs `ubuntu-24.04.ext4` (Ubuntu 24.04 CI-minimal image, customized via a chroot build script: `git`, `curl`, Python 3.12/`uv`, OpenHands installed via `uv tool install openhands --python 3.12`) in `~/firecracker-poc/rootfs/`. Build process documented in `~/firecracker-poc/rootfs/customize-rootfs.sh` (bind-mount `/dev /proc /sys`, `chroot` + `apt-get`, `mkfs.ext4 -d` to produce the final image directly from the chroot — no loop-mount needed for the build itself).
- Host is **pure cgroup v2** (`stat -f -c %T /sys/fs/cgroup` → `cgroup2fs`, no v1 hierarchy mounted at all) — directly relevant to Step 5.
- All Firecracker API interactions driven via `curl --unix-socket` against each run's API socket; guest console interaction via a host-side named FIFO wired to Firecracker's stdin.
- This session had no interactive TTY for `sudo` password entry (harness limitation, not host limitation) — from Step 5 onward, any step needing root (jailer's own privilege requirements) was executed by the user directly in their own terminal from scripts prepared in `~/firecracker-poc/run/`, with results read back from log files. Steps 1–4 needed `sudo` only for one-time host network setup (Step 2) and were run directly.

**Step 1 — Boot a bare microVM: PASS**
- Launched via `firecracker --api-sock`, configured `/boot-source` (`vmlinux-6.18.44`, `boot_args: console=ttyS0 reboot=k panic=1`), `/drives/rootfs`, `/machine-config` (2 vCPU / 1024 MiB), then `InstanceStart`.
- Boots to a root shell via serial-console autologin in **~1.3s** (kernel start to login prompt) — well inside ADR-4's cited ~125ms-class cold-start expectation is *not* met literally (that figure is Firecracker's own process-launch-to-kernel-boot latency, not kernel-to-login-prompt), but is fast in absolute terms and dominated by systemd unit startup, not Firecracker itself.
- `uname -a` confirmed a genuinely separate kernel (`Linux ... 6.18.44+`), distinct from the host's own kernel.
- Console log: `~/firecracker-poc/run/step1-console.log`.

**Step 2 — Networking + egress-allowlist approximation: PASS**
- Host-side tap device `tap0` at `172.16.0.1/30`, guest at `172.16.0.2`; `tinyproxy` listening on `172.16.0.1:8888` with `Allow 172.16.0.0/24`.
- A single nftables rule in a dedicated table (`inet fc_poc`, chain `forward`, hook `forward`) drops and logs all `iifname tap0` forwarded traffic. **Design point**: no NAT/MASQUERADE rule was needed at all — the guest's only legitimate path out is a direct connection to the host's own tap-gateway IP (proxy port), which is host-terminated traffic flowing through `INPUT`, never `FORWARD`; anything else necessarily traverses `FORWARD` and is unconditionally dropped+logged there. This is a materially simpler mechanism than a container-network-namespace NAT setup.
- Verified: guest reaches the proxy successfully; direct (proxy-bypassing) egress attempts are blocked and logged (`fc-poc-egress-blocked:` prefix in the kernel log).
- Setup script (idempotent, re-run safe): `~/firecracker-poc/run/setup-network.sh`.

**Step 3 — Credential injection via MMDS: PASS**
- Configured `/mmds/config` with `version: V2` before `PUT /mmds`, requiring the guest to first obtain a session token (`PUT http://169.254.169.254/latest/api/token` with a TTL header) and present it on the actual data `GET` — confirmed working end-to-end from inside the guest.
- **Token-in-logs finding from the ADR-8 PoC is resolved by this mechanism**: the token was not visible in `/proc/cmdline`, guest `dmesg`, the guest journal, or the host's own console log capture — it only ever exists in the MMDS response body the guest explicitly fetches at runtime, not in any boot-time or process-argument surface.
- Negative tests: a request with no token, and a request with a forged/garbage token, both correctly got `401 Unauthorized` — the guest cannot forge or bypass the token requirement.
- Token artifact: `~/firecracker-poc/run/step3-token.txt`; console log: `~/firecracker-poc/run/step3-console.log`.

**Step 4 — Log/audit extraction via vsock: PASS**
- Host-side listener (`~/firecracker-poc/run/vsock-host-listener.py`) binds a Unix socket at `<uds_path>_<port>` — the host-side endpoint Firecracker itself connects out to for a *guest-initiated* vsock connection (`guest connect(AF_VSOCK, (2, port))`; distinct from the host-initiated `CONNECT <port>\n` protocol on `uds_path` directly).
- Guest-side test agent (`vsock-log-test.py`, injected directly into the rootfs ext4 image via `debugfs -w` — no loop-mount/root needed since the image file is user-owned) wrote 20 sequenced JSON log records to `AF_VSOCK` port 52, retrying the connection until the host listener was ready.
- **Verified independent of the egress path, not just "also works"**: this microVM was booted with *no* `network-interfaces` and *no* `/mmds/config` at all — `ip -brief link` inside the guest showed only `lo`. All 20 records still arrived host-side, **in order and complete** (`seq` 0→19, no gaps/duplicates), confirming audit-log extraction does not depend on the same network route the egress policy polices.
- Host listener transcript: `~/firecracker-poc/run/step4-vsock-listener.log`; console log: `~/firecracker-poc/run/step4-console.log`.

**Step 5 — Jailer vs. bare Firecracker: PASS, with a concrete host-compatibility finding**
- **cgroup v1 (jailer's default) fails cleanly on this host**: `jailer --cgroup cpu.shares=512` (default `--cgroup-version 1`, no explicit flag) returned `Error: CgroupHierarchyMissing("No hierarchy found for this cgroup version.")` and never started Firecracker at all. This is a direct, reproducible confirmation that jailer's default assumes cgroup v1 availability, which this host (pure cgroup v2, per the Environment note above) does not have — **any real deployment on a modern cgroup-v2-only host must pass `--cgroup-version 2` explicitly; this is not optional.**
- With `--cgroup-version 2 --cgroup cpu.weight=100`, jailer succeeded. Verified via `ps` and `/proc/<pid>/status` from the host (not just "jailer exited 0"): the resulting Firecracker process runs as `uid=123456 gid=123456` (a dedicated non-root id chosen for this test, not present in `/etc/passwd` — jailer doesn't require one), with `CapEff: 0000000000000000` (zero effective capabilities, despite jailer itself running as root to set this up).
- Chroot confirmed structurally, not just by a possibly-misleading `readlink /proc/<pid>/root` (which printed `/` — a known procfs quirk where the magic root symlink resolves relative to the *target* process's own view, not usefully readable cross-namespace via plain `readlink`): a direct listing of the chroot base directory shows jailer minted a self-contained jail at `<chroot-base>/firecracker/<id>/root/` (mode `0700`, owned by the target uid/gid), containing the Firecracker binary **hardlinked in as `/firecracker`** (matching the process's own `argv[0]` of `/firecracker`, not the original host path), a minimal `/dev` populated with only `urandom`, `net/tun`, and `kvm` device nodes, and the API socket under `/run/`.
- Cgroup confirmed as genuinely dedicated, not just "some cgroup assigned": `/proc/<pid>/cgroup` → `0::/firecracker/step5`; `cgroup.procs` at that path contains **only** the jailed PID; `cpu.weight` at that path reads back `100`, matching the `--cgroup cpu.weight=100` value passed at launch — the value was actually applied, not silently ignored.
- **Isolation finding on the host side too**: the jail's directories (`0700`, owned only by the dedicated uid/gid) block even the API socket from being reached by a *different* unprivileged host user — an unprivileged `curl --unix-socket` from outside that uid failed to connect at all (curl's own connect error, not a Firecracker-level rejection); only `sudo curl` (root, which bypasses all permission checks) or a process running as the dedicated uid could reach it. This wasn't a designed part of the test but is a real, positive isolation property worth recording.
- Full boot sequence (`/boot-source`, `/drives`, `/machine-config`, `InstanceStart`) succeeded through the jailed socket exactly as in Step 1, reaching the same root-shell-via-autologin end state — no functional regression from jailing.
- **Setup cost versus bare Firecracker (Step 1)**: concretely, three things bare Firecracker doesn't need — (1) `--chroot-base-dir` must already exist before invocation (jailer canonicalizes the path and errors if it doesn't, it does not create the base dir itself, only the `<exec>/<id>/root` tree beneath it); (2) resource files (kernel, rootfs) are **not** auto-copied or auto-linked into the jail — they must be hardlinked (same filesystem, ~instant, no extra disk) or copied in by the caller *after* jailer creates the jail directory but *before* referencing them via the API, using the in-jail path (e.g. `/vmlinux`) in the API call bodies; (3) a cgroup-version mismatch (the v1-default-on-v2-host case above) is a hard failure requiring explicit `--cgroup-version 2` on any modern host. None of these are large costs — this is one-time per-launch scripting, not ongoing operational burden — but they are real integration work a bare-Firecracker launcher would not need.
- **Sharp edge found while cleaning up this test, worth carrying into ADR-10**: hardlinking a shared resource file (kernel/rootfs) into the jail and then `chown`ing it to the jail's dedicated uid/gid — the natural way to satisfy point (2) above without copying multi-GB images — **mutates the original file's ownership too**, since a hardlink shares the same inode as its source; there is only one owner, not one per link. This silently left the shared base rootfs and kernel images (reused across every step in this run) owned by the one-off test uid `123456`, blocking this project's own uid from writing to them until manually `chown`ed back (surfaced as a boot failure two steps later — see Step 6). **A real launcher must copy (not hardlink) any resource file it intends to `chown`, or never `chown` a hardlinked resource at all.**
- Root-privilege note specific to this environment: jailer itself must run as root (it performs the chroot/cgroup/uid-drop setup before exec'ing into Firecracker), so every jailer invocation in a real orchestrator needs a root-capable launch path — this is inherent to jailer's design, not specific to this host.
- Console log: `~/firecracker-poc/run/step5-jailer-console.log`; process/cgroup inspection: `~/firecracker-poc/run/step5-jailer-inspection.log`; chroot directory structure: `~/firecracker-poc/run/step5-chroot-structure.log`.

**Step 6 — Kill-switch / teardown: PASS, with a real durability finding**
- **First attempt hit the Step 5 ownership side-effect directly**: tried to boot with root mounted `is_read_only:true` (to test workspace/base isolation via mount mode) plus the separate `workspace.ext4` drive `is_read_only:false`. The guest kernel panicked at boot: `EXT4-fs (vda): INFO: recovery required on readonly filesystem` → `write access unavailable, cannot proceed (try mounting with noload)` → `Kernel panic - not syncing: VFS: Unable to mount root fs`. Root cause, once traced: the base rootfs image had a dirty (unreplayed) journal left over from earlier steps' non-graceful shutdowns, and a strictly read-only mount cannot replay a journal — this is expected ext4 behavior, not a Firecracker issue, but it meant this host-owned image could no longer be host-`chown`ed or repaired by this project's own uid, because of the Step 5 hardlink/chown side-effect (see above) — surfaced here, fixed with one `sudo chown` back to the original owner.
- **Redesigned the test around the actual requirement** rather than re-fighting the read-only mount: booted normally (`is_read_only:false` root, matching every prior step — this cleanly self-healed the dirty journal via the guest's own ext4 driver) with the separate `workspace.ext4` drive attached read-write, and checked *drive isolation* directly instead of relying on mount-mode as a proxy for it.
- Mid-workload (a background loop appending a timestamp to the workspace drive every 200ms, left running — not stopped or completed), wrote a unique marker string to the workspace drive with an explicit `sync`, then **SIGKILLed the Firecracker process directly** from the host while that loop was still active.
- **Kill-to-gone latency: ~26ms** (`kill -9` issued to confirmed process exit, polled via `kill -0`) — three orders of magnitude inside ADR-4's sub-60-second p95 target.
- **Drive isolation confirmed**: the marker string was recovered from `workspace.ext4` via `debugfs` (no mount needed) after the kill, and a raw byte-level `grep` of the entire base rootfs image for that same marker string found **zero matches** — writes to the workspace drive never touched the base image, confirmed directly rather than inferred from read-only-mount semantics.
- **Real finding on kill-switch durability**: of the mid-workload loop's writes, only the **first ~0.8 seconds' worth (5 lines) survived** the hard kill — the remaining ~13.6 seconds of appends, up to the moment of the kill, were lost. They were sitting in the guest's page cache and never flushed to the workspace image's backing file before SIGKILL tore down the VM; ext4 does not fsync on every `>>` append. The marker file survived *only* because of the explicit `sync` issued right after writing it. **Implication for ADR-1's "destruction is the kill switch" model**: a hard kill can silently lose recently-written, unsynced data on the workspace drive. Anything the orchestrator needs to survive a kill-switch invocation (partial commit state, audit trail tail, etc.) needs an explicit `fsync`/`sync` at the point it becomes "must survive," not just "written" — the kill-switch's speed (~26ms) comes at the direct cost of not waiting for any of that.
- **Host-side cleanup after kill**: the tap device (`tap0`) remained fully present and configured (`ip link show` unchanged) — host network state is **not** torn down by killing the guest and needs explicit cleanup by the launcher/orchestrator if a given tap device is meant to be per-invocation. `tinyproxy` (host-side, shared across invocations by design) was correctly unaffected. The API socket and stdin FIFO files were also left behind on disk (no graceful-shutdown cleanup path was invoked) — again, explicit launcher-side cleanup, not automatic.
- Console log: `~/firecracker-poc/run/step6-console.log` (successful rw-root run; the read-only-root panic trace was not preserved as a separate file, but the exact panic sequence is quoted above verbatim from the console output at the time).

**Phase 2 prerequisite — local Forgejo instance**: `codeberg.org/forgejo/forgejo:16` via Docker Compose at `~/forgejo/`, SQLite backend, data bind-mounted to `~/forgejo/data`, reachable at `http://localhost:3000` (host) / `172.16.0.1:3000` (guest, via the tap gateway). Admin, `bot-coder` (write-scoped: `write:repository,write:issue`), and `bot-reviewer` (read-scoped: `read:repository,write:issue`) accounts created via `forgejo admin user generate-access-token` (CLI token generation, not the `/users/.../tokens` API, which requires a browser password-change flow the CLI path skips). Disposable repo `forgejo-admin/adr9-poc-repo` seeded with the same trivial stdlib-only `calc.py`/`test_calc.py`/`run_tests.sh` shape the ADR-8 PoC used. Verified independently via the repo API that `bot-coder` carries `push:true` and `bot-reviewer` carries `push:false`, matching ADR-2's intended shape before any agent ever touched the repo.

**Step 7 — Core loop under real Firecracker: PASS**
- Booted bare Firecracker (Step 5 already separately validated jailer's isolation properties in isolation; Step 7's purpose is coding-loop parity, not re-proving hardening, so bare Firecracker was used here to avoid repeating the jailer chroot/hardlink dance on every retry).
- A guest-side agent (`step7-agent.py`, injected via the console rather than `debugfs` after a `debugfs`-injection/`e2fsck` interaction destroyed the first attempt — see finding below) fetches its task and credentials from MMDS (V2 handshake, Step 3's mechanism) rather than an env var or boot-time argument: the task prompt, `LLM_API_KEY`, `LLM_MODEL`, and the Forgejo token are all MMDS values, never baked into the image.
- OpenHands SDK CLI v1.21.0 (already present in the base image via `uv tool install openhands`, confirmed via `debugfs` before booting), invoked as `openhands --headless --json --override-with-envs -t "<task>"`, with `http_proxy`/`https_proxy` pointed at the Step 2 tinyproxy and `no_proxy=169.254.169.254` so MMDS calls aren't (and can't be, by design) proxied. Its `--json` event-log lines are streamed line-by-line over `AF_VSOCK` port 53 to the host (Step 4's log path), independent of the git/LLM network route.
- Task mirrored the ADR-8 PoC's round 1 exactly: clone `adr9-poc-repo`, add `subtract(a, b)` + a test, run `./run_tests.sh`, commit, push, open a PR via the Forgejo REST API — all using `bot-coder`'s MMDS-delivered token.
- **Observed sequence** (from the vsock-captured event log): clone → `str_replace` edits to `calc.py`/`test_calc.py` → `./run_tests.sh` (passed) → `git commit` → `git push` → `curl -X POST .../pulls`, all via the agent's own `terminal`/`file_editor` tool calls — same shape as the ADR-8 PoC's finding that PR creation is accomplished via general shell tool use, not a dedicated Forgejo-provider action type.
- **Independently verified via Forgejo's API** (not the agent's self-report): PR #1 open, `mergeable: true`, `user.login: bot-coder`, `head: add-subtract-function → base: main`, `changed_files: 2`. Grepped the PR's title, body, diff, and commit messages for the raw token value — zero matches, confirming the task's explicit "don't print the token" instruction held under real tool execution, not just in the model's stated summary.
- Wall clock (MMDS fetch to OpenHands exit): **~79 seconds**.
- **Two setup findings, both fixed and worth carrying forward**:
  1. Injecting the guest agent script via `debugfs write` and then running `e2fsck -fy` on the (still-dirty-journal, pre-existing) image **deleted the just-written file** — `e2fsck`'s pass 5 recomputed the inode bitmap and concluded the newly-written inode wasn't actually referenced, clearing its allocation bit. Root cause not fully isolated (likely an ordering/flush interaction between `debugfs`'s raw writes and `e2fsck`'s bitmap reconstruction), but the practical rule going forward is clear: **never run `e2fsck -fy` between a `debugfs write` and that file's first use** — either skip the repair, or write the file through the guest's own filesystem driver (console heredoc, as used for the retry) instead of raw `debugfs` when the image already has other pending state.
  2. OpenHands headless mode silently ignores `LLM_API_KEY`/`LLM_MODEL` env vars unless `--override-with-envs` is passed — without it, it printed "Headless mode requires existing settings" and exited 0 (not a crash, a silent no-op), which would be an easy false-negative to miss if the event log weren't being captured independently of the agent's own claimed success.
- Guest agent: `~/firecracker-poc/rootfs/squashfs-root` base image unaffected (agent script lives in the ext4 image, injected live); vsock listener transcript: `~/firecracker-poc/run/step7-vsock-listener.log`; console log: `~/firecracker-poc/run/step7-console.log`.

**Step 8 — Round-2 / update-PR behavior: PASS, with a "fresh microVM" scoping finding**
- Booted a second, separate Firecracker instance (fresh boot, fresh API socket, fresh vsock channel) with a round-2 task: check out the *existing* `add-subtract-function` branch, add `multiply(a, b)` + test (addressing simulated review feedback), push to the same branch, and confirm Forgejo updates PR #1 rather than opening a duplicate.
- **Finding**: this "fresh microVM" reused the *same* mutated `ubuntu-24.04.ext4` image Step 7 had already written into (this PoC never gave each invocation its own copy-on-write overlay of the golden image), so `/root/workspace/adr9-poc-repo` already existed from Step 7 and the agent's first `git clone` failed (`fatal: destination path ... already exists`). The agent recovered on its own — it inspected the failure, `cd`'d into the existing checkout, and proceeded — but this is a **process fidelity gap worth flagging for ADR-10**: a real per-invocation launcher must give each boot a fresh copy-on-write layer over the golden rootfs (or an explicitly wiped workspace), not rely on an LLM to paper over stale state left by a previous run. This run's success is not evidence the gap is safe to ignore.
- **Independently verified via Forgejo's API**: still exactly **1 open PR** (#1), now carrying **2 commits** (`subtract`, `multiply`) — no duplicate PR created, confirming Forgejo's own head-branch tracking updated the existing PR exactly as ADR-7's iteration-loop design assumes.
- Wall clock: **~86 seconds**.
- Vsock listener transcript: `~/firecracker-poc/run/step8-vsock-listener.log`; console log: `~/firecracker-poc/run/step8-console.log`.

**Step 9 — Token-scope negative test: PASS, with a re-confirmed egress-design finding**
- Booted a third fresh instance and, running as `bot-reviewer` (read+comment-only token), attempted: clone → new branch `add-power-function` → add `power(a, b)` → commit → push. Used direct git/curl commands rather than another full OpenHands round, since this step tests Forgejo's own server-side enforcement, not the agent's judgment — cheaper and equally valid evidence.
- **First attempt hung**, not failed: cloning/connecting directly to `172.16.0.1:3000` from the guest console (without the `http_proxy` env var Steps 7–8's MMDS-driven agent always set) timed out silently. Traced via `journalctl -k` on the host to the Step 2 egress rule actually firing: `fc-poc-egress-blocked: ... SRC=172.16.0.2 DST=172.18.0.2 ... DPT=3000` — Docker's own port-publishing DNAT rewrites a direct connection to the tap-gateway IP into a connection to Forgejo's *container* bridge IP (`172.18.0.2`) and routes it through the FORWARD chain (via `br-...`), which the tap0 blanket-drop rule correctly catches, **even though the destination looked like "the host itself."** This reconfirms Step 2's design is working exactly as intended — a guest that bypasses the proxy really is blocked, including for a destination that superficially looks host-local — but it also means **any guest process that wants to reach Forgejo (not just third-party registries) must go through the proxy too**, since Forgejo itself lives behind Docker's DNAT/bridge, not as a bare host process the way `tinyproxy` is. Re-ran with `http_proxy`/`https_proxy` set (matching Steps 7–8) and the clone succeeded immediately.
- **Push attempt failed cleanly**: `remote: Forbidden` / `The requested URL returned error: 403`, git exit code 128 — no retry loop, no partial state, no ambiguity in the failure.
- **Independently verified via Forgejo's API**: the `add-power-function` branch does **not** exist on the remote (404), and there is still exactly **1 open PR** (#1) — confirming nothing leaked through despite `bot-reviewer`'s token carrying `write:issue` scope (issue/PR-comment write, not code write).
- Per the plan's decision tree: Step 9 did **not** reproduce a token-scope bypass, so there is no new Forgejo/ADR-6 finding to escalate — Forgejo's push-permission enforcement behaved correctly here, consistent with the ADR-8 PoC's own equivalent result.
- Console log: `~/firecracker-poc/run/step9-console.log`.

**Cross-step notes for all of Phase 2**: every boot in this run was shut down *gracefully* (`sync && sync` then `poweroff` via the console, confirmed via `e2fsck -fn` showing a clean journal afterward) rather than `SIGKILL`ed — deliberately, after Step 6 already established that a hard kill can lose unflushed writes; Phase 2's goal was coding-loop parity, not re-testing the kill switch, so graceful shutdown was used to keep the shared image consistent between successive boots (see the Step 8 finding above for why "shared" is itself worth revisiting before production).

**Step 10 — Snapshot/restore latency: deliberately skipped, not run**
- Decision made after reviewing Steps 1–9's actual numbers rather than before: Step 1 measured cold boot-to-login at **~1.3s**, and Steps 7–9's dominant cost was overwhelmingly LLM inference + git operations (**~79–86s** end to end) — cold guest boot is already roughly 1–2% of a single invocation's wall clock, so even a snapshot/restore path shaving it to near-zero would not move the number that matters for ADR-1's per-round cost model. Per the plan's own framing, a snapshot also can't include the just-cloned repo, so this optimization only ever targets the smallest term.
- **Per the plan's decision tree, this defaults Step 10 to "documented future optimization," not "in v1."** This was a scope decision based on Steps 1–9's real measurements, not a failed or inconclusive test — worth revisiting only if guest boot time becomes a measured bottleneck in production (e.g. very high invocation concurrency where even a few seconds of cumulative boot time across many parallel microVMs matters more than per-invocation latency).

**Note on jailer vs. bare Firecracker across this run**: Steps 7–9 used **bare** Firecracker, not jailer, purely to reduce retry friction while iterating on an LLM-driven task (avoiding Step 5's chroot/hardlink setup on every re-run). This is *not* evidence that bare Firecracker is the right production choice — per the decision tree, Step 5 found jailer's setup cost one-time and moderate, with real hardening benefits (unprivileged uid, chroot, dedicated cgroup, host-side socket isolation), and found no reason to prefer bare Firecracker functionally. **ADR-10 should still adopt jailer-wrapped Firecracker** per the decision tree's default; Steps 7–9's use of bare Firecracker was a testing-convenience choice, not a finding against jailer.

## Success Criteria

- Clear pass/fail and timing recorded for Steps 1–9 (and 10, if run).
- Enough evidence to write ADR-10's Decision section from real measurements, not documentation claims.
- The token-in-logs finding from the ADR-8 PoC is either resolved by MMDS or explicitly carried forward as still-open.

## Out of Scope

- gVisor validation (ADR-4's documented fallback where KVM is unavailable) — separate exercise, not needed to answer this plan's Firecracker-specific questions.
- Production-scale multi-tenant host provisioning (how many microVMs per host, resource limits across concurrent invocations) — single-invocation validation only, matching the ADR-8 PoC's own scope limit.
- Building ADR-1's actual orchestrator or a real `ContainerLauncher` implementation — this plan hand-drives the steps a real launcher would automate.
- The deterministic-scanner pipeline for the security-triage role — unrelated to this plan's question.
