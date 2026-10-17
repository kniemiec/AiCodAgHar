# ContainerLauncher — developer guide

| | |
|---|---|
| **Status** | Living document — describes the current implementation; update it in the same change that makes it stale (see [`docs/components/README.md`](README.md)) |
| **Component** | `cmd/container-launcher` and the `internal/launcher/*` packages it's built from |
| **Design docs** | [`container-launcher-design.md`](../designs/completed/container-launcher-design.md) (lean v1 — implementation sequence §16: all eight steps landed); [`task-intent-propagation-design.md`](../designs/completed/task-intent-propagation-design.md) §6–7 (adds `TASK_DESCRIPTION` env → `templateData.Description` threading into `implement`/`implement-with-findings`/`review-with-commit`) |
| **Related ADRs** | [ADR-4](../architecture/week1-adr-4-blast-radius-and-sandboxing.md) (isolation/kill-switch/egress requirements this implements), [ADR-10](../architecture/week2-adr-10-microvm-launch-and-lifecycle-mechanics.md) (jailer/MMDS/vsock/egress mechanics), [ADR-8](../architecture/week1-adr-8-coding-agent-runtime-selection.md) (OpenHands headless invocation) |
| **Related plans** | [Firecracker MicroVM Launch PoC](../plans/firecracker-microvm-launch-poc.md) — the hands-on validation this implementation reuses directly rather than re-deriving |
| **Running it locally** | `mocks/launcher-mock` is the default local-dev backend ([`docs/local-development.md`](../local-development.md)); this document covers the real `cmd/container-launcher` binary, which needs `/dev/kvm` + root and can't run in that stack |

## 1. What it is

`ContainerLauncherService`'s production implementation: a gRPC+mTLS server, wire-compatible with `mocks/launcher-mock`, that runs each step invocation inside its own jailer-wrapped Firecracker microVM — real hardware-enforced isolation, not a container and not a mock. Per invocation it boots a real guest kernel/rootfs copied from a golden image, injects credentials and task data via Firecracker's MMDS device (never on the boot-time/image surface — ADR-1/ADR-2's requirement), captures the guest's structured result and event log over `vsock` (decoupled from the guest's own network path, so a network-level failure doesn't also blind the host), enforces a single-invocation egress allowlist via a host tap device + forward proxy + nftables rule, and tears the microVM down on completion, timeout, or an explicit `Kill`.

v1 runs **one invocation at a time**, and `launcherServer` enforces that itself: concurrent `Launch` calls queue for a single VM slot instead of colliding ([§4](#4-api--contracts)). [§8](#8-known-gaps) covers what that trades away.

## 2. Modularity

| Package | Responsibility |
|---|---|
| `cmd/container-launcher` | gRPC+mTLS entrypoint (`main.go`, runs `logging.Setup` and `egress.Setup` once at startup; `health.go` serves `/healthz` and `/readyz` on `LAUNCHER_HEALTH_ADDR`); `launcherServer` (`server.go`) implements the RPC surface, validates the invocation ID, resolves `PayloadRef` to a rendered prompt/command, serializes boots through its `vmSlot`, and tracks in-flight (running or queued) invocations for `Kill`; `firecrackerLauncher` (`launch.go`) is the real `vmLauncher` — boots the jailed microVM and drives it through to a result; `status.go` derives `RunResult.Status` and the fail-closed defaults from a `vsock` result |
| `internal/launcher/jailer` | `jailer` argv/path construction — `--chroot-base-dir`, `--cgroup-version 2` explicit (required on cgroup-v2-only hosts, PoC Step 5 finding) |
| `internal/launcher/fcapi` | Firecracker's own HTTP-over-unix-socket API client: `/boot-source`, `/drives`, `/machine-config`, `/network-interfaces`, `/mmds/config`, `/mmds`, `/vsock`, `/actions` |
| `internal/launcher/mmds` | V2 session-token-required MMDS config/injection on top of `fcapi` — the only mode exposed, since V1 has no guest-side authentication at all |
| `internal/launcher/vsock` | Host-side `vsock` listener, guest event-envelope decoding, and `Redact` — the literal-secret scrub every captured byte stream runs through before it can leave the launcher |
| `internal/launcher/prompts` | `PayloadRef` (== `StepID`, unchanged) → an OpenHands task prompt (`Render`, "openhands-prompt" mode) or a fixed guest shell command (`RenderCommand`, "command" mode) |
| `internal/launcher/egress` | Static, idempotent host-level tap device + nftables rule + forward-proxy setup, run once at launcher startup — not per invocation |
| `internal/launcher/guest` | `guest_wrapper.py` + its systemd unit — the guest-side half of every real invocation; baked into the golden image manually (there's no build automation for this — [§6](#6-running-it)) |

## 3. Lifecycle of one invocation

```mermaid
flowchart TB
    Launch["Launch RPC"] --> Render["PayloadRef -> prompt or command\n(prompts.Render / RenderCommand)\nunmapped -> InvalidArgument"]
    Render --> Slot["wait for vmSlot\n(one boot at a time;\nKill while queued -> killed)"]
    Slot --> Jailer["jailer starts: chroot + cgroup v2 + uid drop"]
    Jailer --> Stage["stage golden kernel/rootfs\n(plain copy, never hardlink+chown\n- PoC Step 5 ownership-mutation finding)"]
    Stage --> BootCfg["boot-source / drives / machine-config"]
    BootCfg --> Net["network-interfaces\n(the one static tap device egress.Setup owns)"]
    Net --> VsockCfg["vsock\n(only when a task or command was rendered)"]
    VsockCfg --> MmdsCfg["mmds/config + mmds\n(TASK/LLM_* or COMMAND, + HTTP(S)_PROXY;\nnever boot args)"]
    MmdsCfg --> Start["InstanceStart"]
    Start --> Wait{"task or command\nrendered?"}
    Wait -->|no| Grace["grace-period alive-check\n(the bare-boot trivial payload)"]
    Wait -->|yes| Guest["guest_wrapper.py:\nMMDS fetch -> OpenHands or command"]
    Guest --> Result["vsock: structured_result\nor command_result"]
    Grace --> Teardown["kill (if not already dead)\n+ remove jail directory tree"]
    Result --> Teardown
```

The guest's only route out is the host forward proxy on the tap gateway, so `launch.go` adds `HTTP_PROXY`/`HTTPS_PROXY` (`egress.Config.ProxyURL()`, i.e. `http://172.16.0.1:8888`) to every agent-stage and command MMDS payload. Callers don't supply it. Without it OpenHands fails every LLM call on name resolution and exits 0 with no structured result.

Every stage before `InstanceStart` fails closed — any error tears the microVM down and `Launch` returns `status: "error"`. Once running, an invocation ends one of four ways (`RunResult.Status`): `completed` (the guest reported a real result), `timed_out` (the launcher's own ceiling fired first), `killed` (an explicit `Kill` RPC pre-empted it — `context.WithCancelCause` distinguishes this from any other cancellation), or `error` (guest crash, or a missing/malformed result). The last three all fail closed — `has_blocking_finding: true` for agent-stage, a synthetic non-zero exit code for deterministic-step — never a false-clean default.

`LaunchResponse.outcome` refines that into the closed set the control-plane classifies failures by (LA-1): `COMPLETED`, `TIMEOUT`, `KILLED`, and `error` split by whether the guest was ever running — `BOOT_FAILURE` for anything up to and including `InstanceStart` (and a bare-boot guest dying in its grace period), `GUEST_ERROR` once `waitForAgentCompletion`/`waitForCommandCompletion` is waiting on it. A request killed while queued is `KILLED` with zero boot/run time. Every response also carries `queue_wait` (waiting for `vmSlot`), `boot` (jail setup → `InstanceStart`), `guest_run` (`InstanceStart` → completion) and `effective_timeout_seconds` (the caller's positive `TimeoutSeconds`, else `LAUNCHER_DEFAULT_TIMEOUT_SECONDS`).

`vsock.Listen` `chown`s the host-side `<uds_path>_<port>` socket file to the jailed process's own uid/gid (`LAUNCHER_JAILER_UID`/`GID`) — the same treatment jailer itself gives `/dev/kvm`/`/dev/net/tun`. Without it the socket file is owned by whoever the launcher process runs as (typically root), and Firecracker's own `connect(2)` to it on the guest's behalf — issued as the unprivileged jailed uid — fails with `EACCES` (`AF_UNIX` connect requires write permission on the target for the connecting process). This doesn't surface as a visible error: the guest's own `vsock_connect()` retries just keep failing and it gives up, which looks host-side like `WaitForResult`'s `Accept()` never firing at all — confirmed against a real KVM boot, not theoretical.

When an invocation doesn't complete cleanly, the launcher logs the last 4 KiB of the guest's event log (redacted, ANSI escapes stripped) at `warn`, marked `logging.Sensitive()` so it stays on stderr and never reaches a central log store. The control-plane doesn't call `ExtractLogs` for failed launches, so this log line is often the only place the failure cause shows up. The guest wrapper reports OpenHands' `ConversationErrorEvent` (code + detail) as the failure reason, instead of only "structured result unreadable".

Teardown removes the invocation's jail directory tree (which covers the API socket, the `vsock` UDS, and the staged image copies — all nested under it) and kills the guest process if it isn't already dead. The tap device is **not** part of per-invocation teardown: [§10](../designs/completed/container-launcher-design.md#10-egress-v1-single-static-setup-not-pooled-automation) of the design treats egress as a static, process-lifetime setup, matching what the PoC itself observed (killing a guest doesn't tear its tap device down).

## 4. API / contracts

Implements `api/proto/launcher/v1/launcher.proto`'s `ContainerLauncherService` — the exact contract `mocks/launcher-mock` implements, so nothing on the orchestrator side changes to point at this binary instead of the mock (`LAUNCHER_ADDR` + the mTLS cert SANs — see [`docs/components/orchestrator.md`](orchestrator.md) §6).

`PayloadRef` → real behavior mapping (`PayloadRef == StepID`, unchanged):

| `StepID` | Mode | Behavior |
|---|---|---|
| `implement` | `openhands-prompt` | Fresh-round or with-prior-findings template, chosen by whether `PRIOR_FINDINGS_JSON` is present; both variants also splice in `TASK_DESCRIPTION` — the human-authored task description, quoted as explicitly-untrusted context — when present |
| `review-with-commit` | `openhands-prompt` | Review template; also receives `TASK_DESCRIPTION` as untrusted background context when present — the first context ever injected into a review prompt (task-intent-propagation design §3/§7) |
| `unit-tests` | `command` | A fixed guest shell command (`internal/launcher/prompts`'s small, closed `StepID`→command map — no path yet from a repo's own `.harness/stages.yaml` to a real per-repo test command, [§8](#8-known-gaps)) |
| *(anything else)* | — | `codes.InvalidArgument` — fails closed, matching the mock's "unknown payload_ref" behavior |

`ExitCode` semantics differ by `StepType`: **agent-stage**'s is the guest wrapper's own exit code (`0` even when `has_blocking_finding: true` — a finding is information, not a launcher failure, matching the mock's existing behavior); **deterministic-step**'s is the guest command's real exit code, propagated directly (`DeterministicStepExecutor` derives `HasFinding` straight from `ExitCode != 0`).

`InvocationId` must match `[A-Za-z0-9_-]`, since it's embedded in jail paths and jailer's `--id`. Anything else gets `codes.InvalidArgument`. The orchestrator's `executor.InvocationID` produces `<task_id>-r<round>-<step_id>`, which satisfies this. `launcher-mock` doesn't validate the ID, so only the real launcher catches a bad format.

**Serialized execution.** Every microVM shares the one tap device, guest CID and MAC (`egress.DefaultConfig`, `launch.go`'s `guestCID`/`guestMAC`), so a second concurrent boot would fail with `EBUSY` on `tap0`. `launcherServer.Launch` therefore holds a single-slot `vmSlot` channel for the whole boot-to-teardown of an invocation. The orchestrator's `coreloop.fanOut` still dispatches a phase's steps concurrently (e.g. `review-with-commit` + `unit-tests`), and the extra `Launch` calls just block in the launcher until the slot frees up. If a wait lasts more than a second, `vm slot acquired` is logged with the wait time. The invocation timeout (`resolveTimeout`) starts only once the slot is acquired, so queue time doesn't count against it. Callers do see a longer RPC, though.

`Kill` cancels a `Launch` call via a per-invocation `context.CancelCauseFunc`, registered as soon as the request is accepted. That covers an invocation that's running and one that's still queued for the slot. A queued one returns `status: "killed"` without ever booting. Called after the invocation already finished, `Kill` returns `Killed: false`. That's not an error: it's a legitimate race, not a caller mistake.

**Failed launches still report.** gRPC sends no response body with a non-OK status, so a failed `Launch` returns `codes.Internal` with the whole `LaunchResponse` (status, outcome, timings) attached as a status detail. `launcherclient` turns that into a `*LaunchError`; an old client ignores the detail, and a new client against an old launcher sees none and falls back to the gRPC code — compatible both ways. `launcher-mock` does the same: `COMPLETED`, or `GUEST_ERROR` when a fixture fails, zero durations, and the caller's `TimeoutSeconds` echoed as the effective timeout.

**Metrics.** Exported over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set (no scrape endpoint): `launcher_invocations_total{outcome,step_type}`, `launcher_queue_depth`, `launcher_queue_wait_seconds`/`launcher_boot_seconds`/`launcher_guest_run_seconds{step_type}`, `launcher_vm_seconds_total{step_type}` (boot + run), `launcher_redactions_total{step_type}` (from `vsock.RedactCounted`), and `harness_build_info`. `step_type` is `agent-stage`, `deterministic-step` or `other`. Non-sensitive log records go to the same endpoint.

## 5. Configuration

Environment variables `cmd/container-launcher/config.go` reads:

| Variable | Required | Purpose |
|---|---|---|
| `TLS_CA_CERT` / `TLS_SERVER_CERT` / `TLS_SERVER_KEY` | Yes | mTLS server identity — unchanged from the mock's contract |
| `LAUNCHER_CHROOT_BASE_DIR` | Yes | jailer's `--chroot-base-dir`; must already exist (jailer canonicalizes it and errors if it doesn't — it does not create the base dir itself). Must be on a real (non-`nodev`) filesystem — `/tmp` is commonly `tmpfs` mounted `nodev` on Linux, which makes jailer's own `/dev/net/tun`/`/dev/kvm` device nodes inside the chroot unusable (`PUT /network-interfaces` fails with "Open tap device failed: ... Permission denied" even though ownership/mode are correct); confirmed on this host (`findmnt -T /tmp` shows `nodev`). Must also be short: this path, joined with `firecracker/<invocation-id>/root/run/firecracker.socket`, is the real host-side Unix domain socket address (unlike the jailed process's own view, which only ever sees the tiny in-chroot path) and Linux caps `sockaddr_un.sun_path` at 108 bytes total — `jailer.Config.SocketPath` fails fast with a clear error if this would be exceeded, but keeping `LAUNCHER_CHROOT_BASE_DIR` under ~40 characters avoids it entirely even with a long (e.g. UUID) invocation id |
| `LAUNCHER_IMAGE_ROOT` | Yes | Directory holding the golden `vmlinux` kernel and `rootfs.ext4` — copied (never hardlinked) into each invocation's jail |
| `LAUNCHER_JAILER_BINARY` / `LAUNCHER_FIRECRACKER_BINARY` | Yes | Paths to the `jailer`/`firecracker` binaries (v1.16.1 x86_64 is what the PoC validated) |
| `ADDR` | No (default `:9443`) | gRPC listen address, matching the mock |
| `LAUNCHER_HEALTH_ADDR` | No (default `127.0.0.1:9444`) | Plain-HTTP listener for `GET /healthz` (always 200) and `GET /readyz` (503 listing failing check names). The gRPC port is mTLS-only, so probes need their own listener. Ready = egress setup finished, `LAUNCHER_JAILER_BINARY`/`LAUNCHER_FIRECRACKER_BINARY` executable, `LAUNCHER_IMAGE_ROOT` readable, `/dev/kvm` opens. There is no database check — the launcher has none. The listener starts before egress setup, so `/readyz` reports `egress` failing while setup runs |
| `LAUNCHER_JAILER_UID` / `LAUNCHER_JAILER_GID` | No (default `123150`/`123150`) | The dedicated non-root uid/gid jailer drops privileges to, and the uid the static tap device is created for ([`internal/launcher/egress`](#2-modularity)) |
| `LAUNCHER_DEFAULT_TIMEOUT_SECONDS` | No (default `1800`) | Hard ceiling applied whenever `LaunchRequest.TimeoutSeconds <= 0` — true of both executors today; a positive caller-supplied value overrides it |
| `LOG_LEVEL` / `LOG_FORMAT` | No (default `info`/`text`) | `internal/logging`, shared with `cmd/control-plane`. `debug` adds every boot stage, each vsock envelope and the rendered prompt's prefix (sensitive: stderr only). Every line of an invocation — including vsock and teardown — carries `task_id`/`step_id`/`invocation_id` from the context `Launch` builds from the request. `info` logs one line per RPC plus `launch accepted`/`microVM started`/`launch finished`. `LOG_FORMAT=json` switches to JSON output. Env values are never logged, only their keys |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | No (unset = no telemetry export) | OTLP/gRPC Collector endpoint for metrics and non-sensitive logs (`internal/telemetry`, shared with `cmd/control-plane`); standard `OTEL_*` variables apply. On `SIGINT`/`SIGTERM` the gRPC server stops — cancelling in-flight invocations, which tears their VMs down — and telemetry flushes for up to 5 s |
| `LAUNCHER_LLM_API_KEY` / `LAUNCHER_LLM_MODEL` | No | The coding agent's own LLM credentials — platform-level, not per-invocation (unlike `FORGEJO_TOKEN`, which arrives per-invocation in `LaunchRequest.Env`); only merged into MMDS for agent-stage invocations, so a deployment that only ever runs deterministic-step invocations doesn't need them |

A missing required var makes the process `log.Fatalf` immediately, same fail-fast convention as `cmd/control-plane`.

`internal/launcher/egress.DefaultConfig` fixes the tap device name/addressing (`tap0` at `172.16.0.1/30`, guest at `172.16.0.2/30`), the guest subnet the proxy allows (`172.16.0.0/24`), the proxy (tinyproxy on port `8888`), and the nftables table name — all hardcoded, matching the PoC's own validated values exactly rather than exposed as new configuration (design §10 frames this as a fixed, reused-as-is setup).

## 6. Running it

Needs `/dev/kvm`, root (jailer requires it to set up the chroot/cgroup/uid-drop before exec'ing Firecracker), and host-level tap/nftables access — it cannot run in the existing mock `docker-compose` stack (`deploy/docker-compose.yml` stays mock-only, portable to macOS and hosted CI). Design §13 recommends a separate `deploy/docker-compose.real-launcher.yml` overlay (`privileged: true`, `/dev/kvm` device-mapped, `network_mode: host`) or an equivalent host-level systemd unit; **neither is built yet** — see [§8](#8-known-gaps).

Two things this repo does not automate:

- **Golden image baking.** `internal/launcher/guest/guest_wrapper.py` and `guest-wrapper.service` run from the golden rootfs, not from this repo, so a change to either does nothing until it's baked in. They must be copied into the image and the systemd unit enabled. Locally that's done by `~/firecracker-poc/rootfs/bake-guest-wrapper.sh`, a host-side script outside this repo; see step 2 of the [manual-run runbook](../plans/coreloop-real-launcher-manual-run.md). The golden-image build/update pipeline itself is out of scope for the design.
- **The guest's own static network config.** `guest_wrapper.py` assumes `eth0` already has `172.16.0.2/30` configured by the image's own boot-time network setup, matching the PoC's image exactly (there's no DHCP server on the tap side to assign one dynamically).

Once those exist and the env vars in [§5](#5-configuration) point at them:

```sh
go build -o container-launcher ./cmd/container-launcher
sudo ./container-launcher   # jailer needs root
```

## 7. Testing

| Layer | Needs live deps? | Command | What it proves |
|---|---|---|---|
| Unit | No | `go test ./...` | `fcapi` against a fake HTTP-over-UDS server; `PayloadRef`→prompt/command mapping; `RunResult.Status` derivation and fail-closed defaults; one test per `LaunchOutcome` incl. `BOOT_FAILURE` vs `GUEST_ERROR`, the response as a status detail on failure, effective-timeout precedence and queue-wait measurement (`outcome_test.go`); instant-exit-0 detection; redaction and its count; `resolveTimeout`/`statusForCancellation`; `egress`'s command-building, proxy-config idempotency and `ProxyURL`; invocation-ID validation; VM-slot serialization (`TestLaunch_SerializesConcurrentInvocations`) and killing a queued invocation (`TestKill_PreemptsQueuedInvocation`); basic teardown |
| Integration (real KVM) | Yes — `/dev/kvm` + root | `make test-launcher-kvm` | `TestLaunchKVM_BareBootTrivialPayload`: a real jailed Firecracker microVM boots end-to-end through this package's own code (jailer/fcapi/mmds/vsock/egress) with a trivial payload, called in-process against the real `*launcherServer`. `TestLaunchKVM_AgentStageRealForgejo`: one real OpenHands agent-stage invocation against a real Forgejo repo and branch. It runs only when `LAUNCHER_LLM_*`, `FORGEJO_URL`/`FORGEJO_TOKEN`, `TARGET_REPO` and `BRANCH` are set, and skips otherwise |
| E2E (real KVM + control-plane + real Forgejo) | Yes | Manual: [`coreloop-real-launcher-manual-run.md`](../plans/coreloop-real-launcher-manual-run.md) | A real task driven through `control-plane` → real gRPC server → microVMs: branch creation, generate, PR, then review + unit-tests. Not automated ([§8](#8-known-gaps)) |

`make test-launcher-kvm` fails fast with a clear message if `/dev/kvm` isn't present and readable/writable, before ever invoking `go test` — it's gated to a self-hosted or nested-virtualization-enabled runner, never standard hosted CI or the macOS dev loop. The Go test itself (`cmd/container-launcher/launcher_kvm_test.go`, build-tagged `launcherkvm` so `go test ./...` never picks it up) skips — rather than fails — if `/dev/kvm` or any of the real-binary/golden-image env vars from [§5](#5-configuration) aren't set, so it degrades gracefully when run directly outside `make`.

## 8. Known gaps

Real, needed work the design names as deliberately out of scope for this lean v1 — see [`container-launcher-design.md`](../designs/completed/container-launcher-design.md) §15 for the full list and rationale. The concrete, currently-missing pieces:

- **No concurrency.** v1 runs one invocation at a time: a single fixed `guestCID`, MAC and tap device, and a per-invocation *full copy* of the image (not the reflink/COW mechanism named as the follow-up design's primary subject). Concurrent `Launch` calls no longer collide, because they queue on `vmSlot` ([§4](#4-api--contracts)). The cost is wall-clock time: a review phase's steps run back to back, not in parallel, and one long invocation holds up every other task's steps on the same launcher.
- **No orphan-sweep-on-restart, no retry-with-backoff on teardown failure.** Teardown is happy-path-and-explicit-kill only; a launcher crash mid-invocation can leak a jail directory.
- **`GrantedScopes` is accepted but not enforced.** The proxy currently allows a fixed CIDR, not a policy-driven per-invocation allowlist.
- **No automated end-to-end test through the control-plane.** A real OpenHands invocation is covered in-process by `TestLaunchKVM_AgentStageRealForgejo`, and the full control-plane → launcher → PR path only by the manual runbook ([§7](#7-testing)). There's no `make test-launcher-e2e`.
- **No `deploy/docker-compose.real-launcher.yml` or host-level systemd unit yet** — design §13's recommended deployment shape for running this binary isn't built.
- **Privilege confinement and the gVisor fallback** (ADR-4) are both out of scope — this is a single privileged process, and there is no documented, explicit fallback path.
