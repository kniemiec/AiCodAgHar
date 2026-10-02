# Firecracker MicroVM PoC — Study Guide (Steps 1–6)

This is a teaching companion to [`firecracker-microvm-launch-poc.md`](firecracker-microvm-launch-poc.md).
That document is the lab record (what was run, what passed, raw evidence). This
document explains the *why* behind each step, the underlying mechanisms, and
the concepts you need to understand to make sense of the results — written so
you can quiz yourself on it later.

---

## 0. Why this PoC exists at all

### The chain of documents

- **ADR-4** ("Blast Radius and Sandboxing") decided, at a policy level, that AI
  coding agents should run inside microVMs (Firecracker) with an egress
  allowlist, credential injection at launch (never baked into images), and a
  fast "kill switch" to revoke a runaway agent. It cited numbers like a
  "~125ms" boot time and a "sub-60-second p95" kill latency — but it never
  actually ran Firecracker to check.
- **ADR-9** ("orchestrator foundations") built the orchestrator's skeleton but
  explicitly *deferred* the actual microVM mechanics, because ADR-9's own dev
  loop ran on a Mac (no KVM available — Firecracker needs Linux + hardware
  virtualization) and used a substitute (Apple's Containerization framework)
  instead.
- **This PoC's job**: get onto a real Linux/KVM host and generate *hands-on
  evidence* for every mechanical claim ADR-4 assumed, so that a future
  **ADR-10** ("MicroVM Launch & Lifecycle Mechanics") can make real decisions
  instead of citing documentation. This is explicitly exploratory — unlike
  the earlier OpenHands/LocalRuntime PoC (ADR-8), which validated an
  already-decided approach, this PoC is *feeding options* into a
  not-yet-written decision.

### The two-perspective structure

The plan document is split into a **Developer perspective** (literally what
commands to run) and an **Architect perspective** (what each result means,
and a decision tree for what to do based on pass/fail). This mirrors the
ADR-8 PoC's convention. Keep both hats on when reading results: "it passed"
is not the same question as "what does passing tell us."

### Why a *microVM* instead of a container?

A container (Docker, etc.) shares the host's kernel — process isolation is
enforced by kernel namespaces/cgroups, all in *one* kernel. If an attacker
(or a misbehaving AI agent) finds a kernel exploit, it can escape straight to
the host. A **microVM** (Firecracker's design point) runs a genuinely
separate guest kernel on top of hardware virtualization (KVM) — the isolation
boundary is the hardware-enforced VM boundary, the same class of boundary
EC2 itself uses (Firecracker was built by AWS for Lambda/Fargate). It boots
fast enough (designed for ~125ms-class cold starts) to be usable
per-invocation rather than a heavyweight, long-lived VM.

**Key distinction to internalize**: containers = shared kernel, namespace
isolation. microVMs = separate kernel, hardware (KVM) isolation. Firecracker
sits in between "full VM" and "container" in overhead, while keeping the
VM-grade isolation boundary.

---

## 1. Core concepts glossary (read this before the steps)

| Term | What it is | Why it matters here |
|---|---|---|
| **KVM** | Linux's kernel-level hardware virtualization (`/dev/kvm`). Firecracker is a VMM (Virtual Machine Monitor) built on top of KVM, not a full hypervisor like QEMU. | Hard prerequisite — no KVM, no Firecracker. This is why this PoC needed a Linux host and couldn't run on the ADR-9 Mac dev loop. |
| **Firecracker** | A minimal VMM, purpose-built for microVMs (no BIOS, no legacy device emulation — just what's needed: virtio-net, virtio-blk, virtio-vsock, a serial console, and MMDS). | The actual sandbox boundary this whole PoC is validating. |
| **jailer** | A separate wrapper binary, also from the Firecracker project, that adds OS-level hardening *around* the Firecracker process: chroot, dedicated non-root uid/gid, cgroup placement. | Step 5's subject. Firecracker itself doesn't self-sandbox at the OS level — jailer is the recommended way to add that. |
| **API socket** | Firecracker exposes a REST-ish API over a Unix domain socket (not started running — you configure it via `PUT` requests, then send `InstanceStart` as an action). | Every step drives Firecracker via `curl --unix-socket <path> -X PUT ...`, never command-line flags for VM config. |
| **tap device** | A virtual, host-side network interface that looks like an Ethernet device to the guest. `ip tuntap add mode tap` creates it; Firecracker attaches to it via `/network-interfaces`. | The guest's entire network path runs through this — there's no other route in/out. |
| **MMDS** | "MicroVM Metadata Service" — Firecracker's own built-in metadata endpoint (modeled on cloud provider metadata services like AWS's `169.254.169.254`). The *host* `PUT`s data into it via the API socket **before boot**; the *guest* can only `GET` it over the network, from inside. | The credential-injection channel — see Step 3. |
| **vsock (virtio-vsock)** | A paravirtualized socket address family (`AF_VSOCK`) for host↔guest communication that does **not** go over the network stack — no IP, no tap device, no egress policy in the path. | The log-extraction channel — see Step 4. Deliberately independent of the network path so a network lockout can't also blind you to what the agent did. |
| **cgroups (v1 vs v2)** | Linux's resource-accounting/limiting mechanism. v1 = multiple parallel hierarchies (one mount per controller); v2 = one unified hierarchy. Most modern distros (this host included) run **cgroup v2 only** — v1 compatibility shims are not guaranteed. | Step 5's compatibility finding: jailer defaults to v1, which flatly fails on a v2-only host. |
| **chroot** | Changes a process's idea of `/` to some other directory — everything outside becomes invisible/unreachable *by pathname*. Weaker than a full container (no separate namespaces by default) but still meaningfully restricts filesystem access. | What jailer sets up around Firecracker in Step 5. |
| **ext4 journal** | ext4 (like most modern filesystems) keeps a write-ahead log ("journal") of pending metadata changes, so an unclean shutdown can be recovered by *replaying* the journal on next mount. Replaying requires **write** access. | Explains the Step 6 boot panic: a **read-only** mount can't replay a dirty journal, so it refuses to mount at all. |
| **Hardlink** | A second directory entry pointing at the *same inode* as an existing file — not a copy. Both names share all metadata, including **ownership**. Deleting one name doesn't delete the data (as long as another link remains); `chown`ing one name changes the *file*, visible under every other name too. | Root cause of a real bug hit in this PoC — see §8. |
| **SIGKILL vs graceful shutdown** | `SIGKILL` (`kill -9`) cannot be caught or ignored — the kernel just tears the process down. No cleanup code in the target process ever runs. | ADR-1's "destruction is the kill switch" model relies on exactly this — it's fast and unconditional, but it also means nothing gets a chance to flush buffers. See Step 6. |

---

## 2. Step 1 — Boot a bare microVM

**What we did**: Launched `firecracker` directly (no jailer), pointed it at an
API socket, and drove it through the minimum config sequence:

1. `PUT /boot-source` — kernel image path + kernel command line (`console=ttyS0 reboot=k panic=1`).
2. `PUT /drives/rootfs` — the guest root filesystem (an ext4 image), marked `is_root_device: true`.
3. `PUT /machine-config` — vCPU count and memory size.
4. `PUT /actions` with `{"action_type": "InstanceStart"}` — the trigger that actually boots the VM (everything before this is just configuration; nothing runs until this call).

**Why this ordering**: Firecracker's API is declarative-then-imperative —
you build up the machine's description first, entirely via `PUT` calls that
each return `204 No Content` on success, and only the final `InstanceStart`
action actually starts execution. This lets the API validate a fully-formed
config atomically rather than reacting to each field as it's set.

**What we verified, and why it's not trivial**:
- Guest booted to a root shell via serial-console autologin in **~1.3
  seconds**. Note this is *not* the same number as ADR-4's cited "~125ms" —
  that figure is (roughly) Firecracker's own process-launch-to-guest-kernel-start
  latency, a purely mechanical VMM number. The 1.3s we measured is
  kernel-boot-to-login-prompt, dominated by **systemd** bringing up units
  (see the boot log's stream of `[ OK ] Started ...` lines) — a different,
  much larger phase than "did the VMM start the vCPU." Conflating these two
  numbers is a common mistake; this PoC's whole point is not to conflate
  them.
- `uname -a` inside the guest showed a genuinely different kernel version
  than the host's own kernel — concrete proof this is a separate kernel, not
  a namespace trick.

**Self-check questions**: What HTTP status code does a successful Firecracker
API `PUT` return? What kernel command-line argument tells the guest kernel
where its console output goes? Why doesn't Firecracker start booting the
instant you `PUT /boot-source`?

---

## 3. Step 2 — Networking + egress-allowlist approximation

**The problem this solves**: ADR-4 says the agent's outbound network access
must be restricted to an allowlist (its own git host, package registries) —
not the open internet. For a microVM, unlike a container on a Docker bridge
network, you have to build this yourself out of fairly low-level Linux
primitives.

**What we built**:
1. A **tap device** (`tap0`, host IP `172.16.0.1/30`, guest IP `172.16.0.2`)
   — this is the only network path in or out of the guest. Firecracker
   attaches to it via `/network-interfaces`.
2. **tinyproxy** listening on the host side, at `172.16.0.1:8888`, configured
   to `Allow 172.16.0.0/24` — a plain HTTP forward proxy.
3. **One nftables rule**: in a dedicated table, chain `forward`, hooked at
   priority 0: `iifname tap0 log prefix "fc-poc-egress-blocked: " drop`. This
   drops (and logs) *every* packet arriving from `tap0` that reaches the
   `forward` chain.

**The design insight worth understanding**: no NAT/MASQUERADE rule was
needed *at all*. Why? Traffic the guest sends **directly to the host's own
IP** (172.16.0.1:8888, the proxy) is host-terminated — it's handled by the
kernel's `INPUT` chain, because the destination *is* the host, not something
the host needs to route onward to. Traffic to anywhere else necessarily needs
the host to *forward* it to a third destination, which is exactly what the
`forward` chain (and our unconditional drop rule there) intercepts. One rule,
zero NAT config, covers the entire "must go through the proxy" policy. This
is simpler than the equivalent container-network-namespace + NAT setup ADR-4
describes for containers.

**Verification**: guest could reach the proxy (and, through it, real
services); a direct connection attempt (bypassing the proxy) was dropped and
showed up in the kernel log with the `fc-poc-egress-blocked:` prefix — so the
policy is not just *present* but **observably enforced and auditable**.

**Self-check questions**: Why does host-destined traffic avoid the `forward`
chain? What would you have to add if the guest needed to reach a *third*
host directly (not via the proxy)? What's the practical difference between
`INPUT` and `FORWARD` in iptables/nftables terms?

---

## 4. Step 3 — Credential injection via MMDS

**The problem this solves**: ADR-1/ADR-2 require that credentials (e.g. a
scoped git-host token) are **injected at launch**, never baked into the VM
image (an image is a shared, reusable artifact — baking a secret into it
would leak it to every future invocation and anyone with read access to the
image). A prior PoC (ADR-8) flagged a real finding: when the token was passed
as a plain environment variable, it showed up incidentally in a shell/log
stream (`${FORGEJO_TOKEN}` interpolated visibly). MMDS is Firecracker's own
purpose-built answer to this class of problem.

**How MMDS actually works**:
- Before `InstanceStart`, the **host** `PUT`s the secret data into `/mmds`
  (the data itself) and configures `/mmds/config` (which network interfaces
  may serve it, and crucially — the **version**).
- Inside the guest, MMDS is reachable at a link-local address
  (`169.254.169.254`, the same convention AWS/GCP/Azure use for their own
  metadata services) — but **only** as an HTTP `GET`, over the guest's own
  network interface, i.e. this is a mechanism the *host* controls entirely;
  the guest is a pure consumer.
- **V2 (the version we used) requires a session-token handshake**: the guest
  must first `PUT` a request to `/latest/api/token` with a TTL header to
  obtain a short-lived session token, then present that token on the actual
  data `GET`. A request with no token, or a garbage/forged token, is
  rejected with `401 Unauthorized`. V1 (not used here) allowed plain
  unauthenticated `GET`s — V2 exists specifically to prevent, e.g., an SSRF
  bug in some unrelated guest process from casually leaking the metadata to
  an attacker who merely got code execution but doesn't know to go through
  the token dance (defense in depth, not really "guest can't get it" since
  the guest is trusted here — more about accidental disclosure).

**What we verified**: the token was not visible anywhere it shouldn't be —
not in `/proc/cmdline` (so not a kernel boot argument), not in guest
`dmesg`, not in the guest's own systemd journal, not in the host's captured
console log. It only ever exists in the MMDS HTTP response body the guest
explicitly fetches at runtime. This is a direct, positive resolution of the
ADR-8 finding: this channel doesn't have the "quietly ends up in a log
stream" failure mode that a plain env var did.

**Self-check questions**: What's the practical difference between MMDS V1
and V2? Why is "guest can only `GET`, host can only `PUT`" a meaningful
security property here, given the guest itself is the thing being trusted
with the credential anyway? Where, specifically, did earlier tooling
(ADR-8) leak a token, and why doesn't MMDS have that same failure mode?

---

## 5. Step 4 — Log/audit extraction via vsock

**The problem this solves**: however you extract structured logs/audit data
from the agent running inside the guest, that extraction path **must not
depend on the same network route the egress policy controls** — otherwise a
network lockout (intentional, from the egress policy, or a bug) also blinds
the orchestrator to what the agent was doing right before it got locked out.
You need a channel that is architecturally separate from "the network."

**What vsock is**: a virtio device (`virtio-vsock`) that gives you
`AF_VSOCK` sockets — a socket API that behaves like TCP (`connect`,
`sendall`, ordered/reliable byte stream) but is carried directly between host
and guest by the hypervisor, with **no IP stack, no tap device, no
nftables/routing involved at all**. It literally cannot be blocked by the
Step 2 egress policy, because it isn't network traffic in the first place.

**The host/guest wiring, which is easy to get backwards**: Firecracker's
vsock device is configured with a single `uds_path` (a Unix socket path on
the host). But there are *two different* connection directions with
*different* conventions:
- **Host-initiated** (host connects out to a guest-listening port): host
  connects to `uds_path` directly and sends a text line `CONNECT <port>\n`.
- **Guest-initiated** (guest connects out to the host — what we used):
  Firecracker instead opens an outbound connection to
  **`<uds_path>_<port>`** (the path with `_<port>` appended). The host must
  already have a listener **bound and accepting** at that exact derived
  path before the guest tries to connect, or the connection fails outright.

**What we actually ran**: a small guest-side Python agent (standing in for
OpenHands' own structured `--json` event log output) opened
`AF_VSOCK` to `(cid=2 [host], port=52)`, retrying the connect for up to 20
seconds (since there's no guarantee the host listener is up the instant the
guest boots), then wrote 20 sequenced JSON records and cleanly shut down its
write side.

**The critical extra check**: we booted this microVM with **no
`network-interfaces` and no `/mmds/config` at all**. Confirmed via `ip
-brief link` inside the guest that only `lo` (loopback) existed — there was
categorically no network device for the guest to even attempt to use. All 20
records still arrived at the host listener, **in order, with no gaps or
duplicates** (verified by checking the received `seq` field formed an
unbroken `0..19` sequence). This is the real proof that log extraction is
independent of the network/egress path, not just "vsock also happens to
work when networking is present."

**Self-check questions**: What's the exact host-side socket path convention
for a *guest-initiated* vsock connection, and how does it differ from a
host-initiated one? Why does testing this specifically *without* any network
device matter, rather than just testing it alongside a working network? What
would "in order and complete" actually catch if it failed (e.g., what guest
or host bug would show up as out-of-order or missing records)?

---

## 6. Step 5 — Jailer vs. bare Firecracker

**The problem this solves**: Firecracker itself has *no* built-in OS-level
sandboxing of its own process — it's just a VMM binary. If you run it
directly (as in Steps 1–4), it runs as whatever user/permissions launched
it, with the full filesystem visible to it (even though the *guest* is
isolated, the *host process* is not). **jailer** is the Firecracker
project's own answer: a separate wrapper that does privilege-dropping,
chrooting, and cgroup placement *before* handing off to Firecracker, so that
even if something escaped the VM boundary, it would land in a heavily
restricted host-side context rather than as an ordinary user process.

**How jailer actually works, mechanically**: You invoke `jailer` (which
**must run as root** — chrooting, cgroup setup, and dropping to another
uid/gid are all privileged operations) with `--exec-file <path-to-firecracker>`,
`--uid`/`--gid` (a dedicated, unprivileged id — does **not** need an
`/etc/passwd` entry, a bare number is fine), and `--chroot-base-dir`. jailer
then:
1. Creates a jail directory tree at `<chroot-base-dir>/<exec-file
   basename>/<id>/root/`.
2. **Hardlinks** the exec-file (Firecracker itself) into that jail root as
   `/firecracker` and populates a minimal `/dev` (just `urandom`, `net/tun`,
   `kvm` — exactly what Firecracker needs, nothing more).
3. `chown`s the jail root to the target uid/gid.
4. Calls `chroot()` into that directory, drops privileges to the target
   uid/gid, and **execs** Firecracker (replacing its own process image — so
   in `ps`, you only ever see `firecracker`, not a separate parent `jailer`
   process still running).

**What we verified, and why each check matters** (this is the part of the
plan that explicitly says "not just 'the jailer ran without erroring'"):
- **Unprivileged**: `ps -o uid,gid` and `/proc/<pid>/status` showed
  `uid=gid=123456` (our chosen test id) and `CapEff: 0000000000000000` — zero
  *effective* Linux capabilities, even though jailer itself ran as root to
  set this up. (Capabilities are Linux's fine-grained root-privilege
  breakdown — e.g. `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` — dropping to a plain
  uid with zero effective caps means this process can't do *any* of the
  things root-only operations require.)
- **Chrooted**: confirmed not by trusting `readlink /proc/<pid>/root`
  (which is a known procfs quirk — it resolves relative to the *target*
  process's own view of its root, so it just prints `/` and isn't useful
  cross-namespace), but by directly **listing the chroot directory itself**
  and seeing the self-contained jail: the hardlinked `/firecracker` binary,
  the minimal `/dev`, the API socket under `/run/` — and confirming the
  process's own `argv[0]` was literally `/firecracker` (the *in-jail* path,
  not the original host path), which only makes sense if the chroot actually
  took effect.
- **Dedicated cgroup**: `/proc/<pid>/cgroup` showed a jail-specific path
  (`0::/firecracker/step5`); reading `cgroup.procs` at that path showed
  **only** our one PID (not shared with unrelated host processes); and the
  `--cgroup cpu.weight=100` value we passed at launch was actually readable
  back from `cpu.weight` in that cgroup — i.e. the setting was genuinely
  *applied*, not silently ignored.

**The real compatibility finding**: jailer's **default** is
`--cgroup-version 1`. This host, like most modern Linux distros, runs
**cgroup v2 only** (`stat -f -c %T /sys/fs/cgroup` → `cgroup2fs`, no v1
hierarchy mounted anywhere). Running jailer with a real v1-style cgroup value
(`--cgroup cpu.shares=512`) and no `--cgroup-version` flag failed
immediately and cleanly with `Error:
CgroupHierarchyMissing("No hierarchy found for this cgroup version.")` —
Firecracker never even started. Passing `--cgroup-version 2` explicitly (with
a v2-style value, `cpu.weight=100`) succeeded. **Takeaway: on a modern host,
`--cgroup-version 2` is not optional — it's a required flag, not a default
you can skip.**

**A secondary, positive isolation finding** (not originally planned, found
while trying to drive the guest): the jail's directories are mode `0700`,
owned only by the dedicated uid/gid — so even the Firecracker **API socket
itself** cannot be reached by a *different* unprivileged host user (an
ordinary `curl --unix-socket` from another uid fails to even connect; only
`sudo` or a process running as that exact uid can reach it). This is a real
extra layer of host-side isolation jailer gives you essentially for free.

**Self-check questions**: What three specific setup steps does jailer
require that bare Firecracker doesn't (chroot-base-dir must pre-exist;
resource files aren't auto-copied in; cgroup version must be chosen
explicitly)? Why is `readlink /proc/<pid>/root` not trustworthy evidence of
a chroot? What Linux mechanism does "zero effective capabilities" actually
restrict, concretely?

---

## 7. Step 6 — Kill-switch / teardown

**The problem this solves**: ADR-1's whole "revocation" model for a
misbehaving or compromised agent is **destroy the microVM** — not "ask it
nicely to stop," not "revoke a token and hope." ADR-4 additionally claims a
sub-60-second p95 target for this. Step 6 checks: does a hard kill actually
behave the way that model assumes, and what — if anything — doesn't survive
it?

**First attempt (and why it failed) — an important detour**: The original
plan was to mount the guest's root filesystem **read-only** (to test, via
mount-mode, that the workspace/overlay drive is where writes go, not the
shared base image) plus a second, separate writable `workspace.ext4` drive.
This **panicked at boot**:
```
EXT4-fs (vda): INFO: recovery required on readonly filesystem
EXT4-fs (vda): write access unavailable, cannot proceed (try mounting with noload)
Kernel panic - not syncing: VFS: Unable to mount root fs on unknown-block(254,0)
```
**Why**: ext4 keeps a **journal** — recall from the glossary, a write-ahead
log of pending metadata changes, replayed on the next mount after any
unclean shutdown. The base rootfs image had been left with a "dirty"
(unreplayed) journal by earlier steps' non-graceful process kills. Replaying
a journal **requires write access** — so a strictly read-only mount can't
self-heal, and the kernel refuses to mount the filesystem at all rather than
risk an inconsistent view. This is completely standard ext4 behavior, not a
Firecracker bug — but it's exactly the kind of "documentation claim vs.
hands-on reality" gap this whole PoC exists to surface.

**The fix, and the redesign**: rather than fight the read-only mount, we
booted **normally** (root read-write, exactly like every prior step — this
let the guest's own ext4 driver transparently replay and clean the journal
on mount, no host-side repair needed) and instead verified the *actual*
requirement — **drive isolation** — directly, rather than through mount-mode
as an indirect proxy for it.

**The actual test**:
1. Attached `workspace.ext4` as a second, separate drive (not the root
   device), read-write.
2. Started a **background workload loop inside the guest**, left
   deliberately running (not stopped, not completed) — appending a
   timestamp to a file on the workspace drive every 200ms.
3. Wrote a unique marker string to the workspace drive, with an **explicit
   `sync`** call right after.
4. While that loop was still actively running — **`kill -9`'d the
   Firecracker process directly from the host** (mid-workload, matching the
   plan's explicit requirement, not a graceful shutdown).

**Result 1 — timing**: kill-to-confirmed-gone latency was **~26
milliseconds** (`kill -9` issued, then polled with `kill -0` in a tight loop
until the process no longer existed). Three orders of magnitude inside
ADR-4's sub-60-**second** p95 target. `SIGKILL` cannot be caught, blocked, or
ignored by the target process — the kernel tears it down unconditionally,
which is precisely why it's fast: there's no cleanup code path to wait for.

**Result 2 — drive isolation, confirmed directly**: the marker string was
read back out of `workspace.ext4` afterward using `debugfs` (a tool that
reads an ext4 image's contents directly from the file, **without mounting
it** — useful for exactly this kind of forensic/verification check). A raw
byte-level `grep` across the **entire base rootfs image** for that same
marker string found **zero matches**. This directly confirms: whatever the
guest wrote to the workspace drive never landed on the shared base image —
proven by searching for it, not inferred from read-only-mount semantics.

**Result 3 — the actually surprising finding**: of the background loop's
writes (running for roughly 14 seconds before the kill), only the **first
~0.8 seconds' worth (5 lines) were recoverable** afterward. The remaining
~13.6 seconds of appends were **gone** — they existed only in the guest's
in-memory page cache and were never flushed to the workspace image's backing
storage before `SIGKILL` tore the whole VM down (a plain `>>` append doesn't
call `fsync` per write; the kernel batches and flushes dirty pages on its own
schedule). The marker string, by contrast, **did** survive — specifically
*because* of the explicit `sync` call issued right after writing it.

**Why this matters for ADR-1's design**: "destruction is the kill switch"
is fast (confirmed: ~26ms) *precisely because* it does not wait for
anything — including the guest's own unflushed writes. If the orchestrator
ever needs some piece of state (a partial commit, the tail of an audit log,
anything) to reliably survive a kill-switch invocation, that data needs an
explicit `fsync`/`sync` at the moment it becomes "must survive," not merely
"has been written." This is a real design constraint to carry into ADR-10,
not a hypothetical edge case — we watched it happen with a 200ms write
interval and lost 94% of the writes.

**Result 4 — host-side cleanup, or the lack of it**: after the kill, the
**tap device (`tap0`) was still fully present and configured** — killing the
guest process does **not** tear down host-side network state. If a real
orchestrator wants a tap device to be strictly per-invocation, tearing it
down is the launcher's explicit responsibility, not something that happens
automatically. `tinyproxy` (correctly, since it's a shared host service, not
per-invocation) was unaffected. The API socket file and the stdin FIFO file
were likewise left behind on disk with no automatic cleanup.

**Self-check questions**: Why couldn't the read-only root mount attempt just
be retried as-is? What Linux mechanism specifically caused ~13 seconds of
writes to be lost, and what one-line guest-side change would have prevented
it for the workload loop too? Name three things that do *not* get cleaned up
automatically when you `kill -9` a Firecracker process. Why is "fast" and
"doesn't wait for unflushed writes" the same underlying fact about SIGKILL?

---

## 8. Cross-cutting findings (things that surfaced *between* steps, not from one step's own checklist)

These are arguably the most valuable findings in this run, precisely because
they weren't things the plan told us to look for — they're the kind of
lesson you only get from actually running the mechanics end-to-end.

### 8.1 Hardlink + chown mutates the *original* file (found in Step 5, surfaced as a Step 6 boot failure)

To get the kernel/rootfs images visible *inside* jailer's chroot (Step 5),
we **hardlinked** them into the jail directory (cheap — no multi-gigabyte
copy, instant, no extra disk — since a hardlink is just a second name for
the same underlying inode) and then `chown`ed that in-jail path to the
jail's dedicated test uid, so the jailed (unprivileged) Firecracker process
could read/write them.

The catch: **a hardlink is not a copy.** Both names refer to the exact same
inode, and an inode has exactly **one** owner — there's no such thing as
"owned by user A under this name, user B under that name." `chown`ing the
in-jail name therefore silently changed the ownership of the **original,
shared** rootfs and kernel images too — files reused across *every* step in
this PoC run. This wasn't discovered until **two steps later**, in Step 6,
when a boot attempt failed because the invoking user could no longer get
write access to its own base image.

**The general lesson**: hardlinking is a great trick for "make this large
file appear somewhere else without copying it" — but only as long as you
never need per-location metadata (ownership, in this case) to differ. The
moment you `chown`/`chmod` a hardlinked path expecting it to be scoped to
just that link, you're wrong — you've mutated the one shared file. A real
launcher should **copy** (not hardlink) any resource file it intends to
`chown`, or simply never `chown` a hardlinked resource at all (e.g. use
group permissions instead).

### 8.2 A dirty ext4 journal blocks read-only remount later

Related to 8.1 in this run's timeline, but a separate underlying mechanism
(see §7's "first attempt" above): repeatedly power-cycling a shared,
read-write-mounted image via non-graceful kills (which is exactly what
several earlier steps did, on purpose, as part of testing the kill switch
itself!) leaves its ext4 journal dirty. That's invisible and harmless as
long as you keep mounting it read-write (the guest kernel just replays the
journal transparently on each mount) — but it becomes a hard failure the
moment you need a strictly read-only mount, because journal replay needs
write access. Good general lesson: an image's "cleanliness" is stateful and
accumulates across runs — it's not something you can reason about from a
single boot in isolation.

### 8.3 Environment friction: no interactive TTY for `sudo` in this session

Not a Firecracker finding, but a real practical constraint that shaped how
Steps 5–6 were executed: the automation environment driving this PoC had no
real terminal for `sudo` to prompt for a password on. Every operation
needing root (jailer itself, chroot cleanup, reading another uid's `/proc`
entries) had to be handed to the user to run directly, in their own
terminal, with results read back from log files. Worth knowing if you ever
need to reproduce or extend this run yourself.

---

## 9. How Steps 1–6's results map back to ADR-10's decision tree

The plan document's Architect Perspective lays out a decision tree. With
Steps 1–6 all passing:

- **Firecracker itself**: viable, boots fast, API model is straightforward
  to drive.
- **Egress allowlist (Step 2)**: the tap-device + single-forward-drop-rule
  design translates cleanly, no redesign needed — this was flagged as a
  possible "hard part" in the decision tree, and it wasn't.
- **MMDS (Step 3)**: resolves the ADR-8 token-in-logs finding outright —
  this no longer needs to be carried forward as an open problem.
- **vsock (Step 4)**: resolves the "log extraction shouldn't depend on the
  network path" requirement, confirmed with networking entirely absent, not
  just "also works alongside a working network."
- **jailer (Step 5)**: worth adopting — the isolation guarantees
  (unprivileged, chrooted, cgrouped) are real and verified, not just
  documented, and the added setup cost is small and one-time-per-launch, not
  ongoing. The one hard requirement to carry forward: **always pass
  `--cgroup-version 2` explicitly** on any modern host.
- **Kill switch (Step 6)**: the sub-60s timing target is trivially met
  (~26ms) — but the **durability caveat** (unsynced writes can be lost) is a
  real design input, not just a footnote, for anything ADR-10 wants to
  guarantee survives a kill.

None of Steps 1–6 hit the decision tree's "treat as the primary open design
problem" branch (that was reserved for Step 2 or Step 4 failing/proving
hard — neither happened). Phase 2 (Steps 7–9, the real coding-role loop
under actual Firecracker + Forgejo) is where the *parity* question against
the earlier ADR-8 substitute PoC gets answered.

---

## 10. Quick-reference cheat sheet

- **Firecracker API config sequence**: `boot-source` → `drives` →
  `machine-config` → (optionally `network-interfaces`, `mmds/config`,
  `mmds`, `vsock`) → `actions` (`InstanceStart`). Every successful `PUT`
  returns `204`.
- **MMDS**: host `PUT`s via API socket before boot; guest `GET`s via
  `169.254.169.254` at runtime; V2 requires a session token obtained via a
  `PUT` to `/latest/api/token` first.
- **vsock guest-initiated connection**: guest connects to `(cid=2,
  port=N)`; host must listen on a Unix socket at `<uds_path>_<N>`.
- **jailer on a cgroup-v2 host**: `--cgroup-version 2` is mandatory, not a
  default.
- **jailer jail layout**: `<chroot-base-dir>/<exec-file-basename>/<id>/root/`
  — the exec file lands there as `/firecracker` (in-jail path).
- **`readlink /proc/<pid>/root`** is not reliable evidence of a chroot —
  inspect the directory contents instead.
- **SIGKILL** = instant, uncatchable, but guarantees nothing about
  unflushed writes. `sync`/`fsync` before a write is considered "durable" is
  the only way to guarantee it survives a hard kill.
- **Hardlinks share one inode** — `chown`/`chmod` on any linked name affects
  the file, not "that link."
- **ext4 journal replay needs write access** — a dirty image cannot be
  mounted strictly read-only.
