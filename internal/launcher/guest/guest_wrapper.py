#!/usr/bin/env python3
"""container-launcher guest wrapper.

Runs at boot (systemd oneshot — see guest-wrapper.service in this same
directory) inside the golden image. Fetches its payload and credentials
from MMDS over the V2 session-token handshake (never a boot-time env var or
kernel arg — design §7), then dispatches to one of two modes based on which
key MMDS carries (design §5's mode table — v1 uses a single golden image
for every StepID, so this one script must tell the two apart at runtime):

  - MMDS "TASK" set   -> agent-stage mode: runs OpenHands headless against
    that task (run_openhands_mode).
  - MMDS "COMMAND" set -> deterministic-step mode: clones TARGET_REPO@BRANCH
    with the MMDS-delivered credential and runs the fixed command
    (run_command_mode).

Either way, results stream back to the host over vsock — independent of the
git/LLM network path, so a network-level failure during the run doesn't
also blind the host-side observer (Firecracker PoC Step 4 finding).
run_openhands_mode also detects the known instant-exit-0 OpenHands failure
signature (design §6) and reports it as an "error" envelope instead of
ever reading a stale/absent structured_result.json.

Host-side counterpart: internal/launcher/vsock (Go). Wire protocol (one
JSON object per line, newline-delimited):

    {"type": "event", "line": "<raw openhands --json line, or a command's raw output line>"}
    {"type": "structured_result", "data": {...}}          # agent-stage mode
    {"type": "command_result", "exit_code": <int>}         # command mode
    {"type": "error", "message": "<what went wrong>"}

MMDS carries the launch request's Env map verbatim, plus TASK/LLM_API_KEY/
LLM_MODEL (agent-stage) or COMMAND (deterministic-step), added by the
launcher itself (internal/launcher/mmds, cmd/container-launcher/launch.go)
— all in the same UPPER_SNAKE_CASE the launcher's own Env map already
uses, so every key but TASK/COMMAND can be exported into a subprocess's
environment unmodified (agent-stage also passes --override-with-envs so
OpenHands picks these up the same way).

Baking this script into the golden image (and wiring the systemd unit
below to run it) is manual for v1 — the golden-image build/update pipeline
itself is out of scope for this design (§15's known gaps), so there is no
automation here that copies this file into an image for you.

eth0's own static address (172.16.0.2/30, matching the host-side tap
gateway internal/launcher/egress always sets up at 172.16.0.1/30) is set
by this script itself (ensure_guest_network), not assumed to already be
configured by the image's boot-time network setup — the PoC's own
customize-rootfs.sh carries no netplan/systemd-networkd config that would
produce it, and there's no DHCP server on the tap side to assign one
dynamically, so depending on unexplained baked-in image state would be
fragile. Idempotent and best-effort, same posture as ensure_mmds_route.
"""
import http.client
import json
import os
import socket
import subprocess
import time

MMDS_HOST = "169.254.169.254"
GUEST_IFACE = "eth0"
GUEST_CIDR = "172.16.0.2/30"
VSOCK_PORT = 53
STRUCTURED_RESULT_PATH = "/root/result/structured_result.json"
OPENHANDS_BIN = "/root/.local/bin/openhands"
WORKDIR = "/root/workspace"
REPO_DIR = WORKDIR + "/repo"

# Instant-exit-0 detection (design §6): OpenHands headless mode has been
# observed to silently ignore LLM_API_KEY/LLM_MODEL and exit 0 with no work
# done unless --override-with-envs is also passed — we always pass that
# flag, but this check stays as defense-in-depth against the same failure
# mode from any other cause (e.g. a still-empty LLM_API_KEY). A real run
# emits many JSON event lines over many seconds; this bug prints one
# non-JSON message and exits almost immediately.
INSTANT_EXIT_THRESHOLD_SECONDS = 5
INSTANT_EXIT_MIN_EVENTS = 1

# Command mode's own guest-side timeout guard — not a replacement for the
# launcher's own agentInvocationTimeout (design §8), just a backstop so a
# wedged guest command doesn't hold the vsock connection open forever with
# no events at all.
COMMAND_TIMEOUT_SECONDS = 25 * 60


def ensure_guest_network():
    # Set eth0's static address explicitly rather than trust unverified
    # baked-in image state (see module docstring). Best-effort and
    # idempotent: harmless if it's already configured.
    try:
        subprocess.run(
            ["ip", "addr", "add", GUEST_CIDR, "dev", GUEST_IFACE],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        subprocess.run(
            ["ip", "link", "set", "dev", GUEST_IFACE, "up"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
    except OSError:
        pass


def ensure_mmds_route():
    # 169.254.169.254 is not on-link with the guest's own tap-gateway
    # subnet (172.16.0.0/30), so nothing routes it automatically even once
    # eth0 is up with its normal address — the Firecracker PoC hit exactly
    # this gap (Steps 7-8 console logs: "ip route add 169.254.169.254 dev
    # eth0" was needed by hand before the MMDS fetch worked). Best-effort
    # and idempotent: harmless if the route already exists.
    try:
        subprocess.run(
            ["ip", "route", "add", MMDS_HOST, "dev", GUEST_IFACE],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
    except OSError:
        pass


def mmds_get_all():
    ensure_guest_network()
    ensure_mmds_route()
    conn = http.client.HTTPConnection(MMDS_HOST, timeout=5)
    conn.request("PUT", "/latest/api/token", headers={"X-metadata-token-ttl-seconds": "21600"})
    resp = conn.getresponse()
    token = resp.read().decode()
    conn.close()

    conn = http.client.HTTPConnection(MMDS_HOST, timeout=5)
    conn.request("GET", "/", headers={"X-metadata-token": token, "Accept": "application/json"})
    resp = conn.getresponse()
    data = json.loads(resp.read().decode())
    conn.close()
    return data


def vsock_connect(retries=20, delay=0.5):
    for _ in range(retries):
        try:
            s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
            s.connect((2, VSOCK_PORT))  # CID 2 = host
            return s
        except OSError:
            time.sleep(delay)
    return None


def run_openhands_mode(env, mmds, emit):
    task = mmds["TASK"]

    os.makedirs(WORKDIR, exist_ok=True)
    os.makedirs(os.path.dirname(STRUCTURED_RESULT_PATH), exist_ok=True)

    try:
        proc = subprocess.Popen(
            [OPENHANDS_BIN, "--headless", "--json", "--override-with-envs", "-t", task],
            cwd=WORKDIR,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
        )
    except OSError as e:
        emit({"type": "error", "message": f"failed to start openhands: {e}"})
        return

    start = time.time()
    event_count = 0
    for line in proc.stdout:
        stripped = line.rstrip("\n")
        emit({"type": "event", "line": stripped})
        try:
            json.loads(stripped)
            event_count += 1
        except ValueError:
            pass
    rc = proc.wait()
    elapsed = time.time() - start

    if rc == 0 and elapsed < INSTANT_EXIT_THRESHOLD_SECONDS and event_count < INSTANT_EXIT_MIN_EVENTS:
        emit({
            "type": "error",
            "message": (
                f"openhands exited 0 in {elapsed:.1f}s with {event_count} JSON "
                "event(s) logged — this looks like the known instant-exit-0 "
                "failure (design §6), not a real completion"
            ),
        })
        return

    try:
        with open(STRUCTURED_RESULT_PATH) as f:
            result = json.load(f)
        emit({"type": "structured_result", "data": result})
    except (OSError, ValueError) as e:
        emit({"type": "error", "message": f"openhands exited {rc}, structured result unreadable: {e}"})


def run_command_mode(env, mmds, emit):
    command = mmds["COMMAND"]
    target_repo = env.get("TARGET_REPO")
    branch = env.get("BRANCH")
    forgejo_url = env.get("FORGEJO_URL")
    token = env.get("FORGEJO_TOKEN")
    if not (target_repo and forgejo_url and token):
        emit({"type": "error", "message": "command mode requires TARGET_REPO, FORGEJO_URL, and FORGEJO_TOKEN in env"})
        return

    os.makedirs(WORKDIR, exist_ok=True)
    scheme, _, rest = forgejo_url.partition("://")
    clone_url = f"{scheme}://x-access-token:{token}@{rest}/{target_repo}.git"
    clone_cmd = ["git", "clone", "--depth", "1"]
    if branch:
        clone_cmd += ["--branch", branch]
    clone_cmd += [clone_url, REPO_DIR]

    emit({"type": "event", "line": f"cloning {target_repo}@{branch or '(default branch)'}"})
    clone = subprocess.run(clone_cmd, cwd=WORKDIR, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    for line in clone.stdout.splitlines():
        emit({"type": "event", "line": line})
    if clone.returncode != 0:
        emit({"type": "error", "message": f"git clone failed with exit code {clone.returncode}"})
        return

    try:
        proc = subprocess.Popen(
            ["/bin/sh", "-c", command],
            cwd=REPO_DIR,
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
        )
    except OSError as e:
        emit({"type": "error", "message": f"failed to start command: {e}"})
        return

    for line in proc.stdout:
        emit({"type": "event", "line": line.rstrip("\n")})
    try:
        rc = proc.wait(timeout=COMMAND_TIMEOUT_SECONDS)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
        emit({"type": "error", "message": f"command exceeded {COMMAND_TIMEOUT_SECONDS}s guest-side timeout"})
        return

    emit({"type": "command_result", "exit_code": rc})


def main():
    vs = vsock_connect()

    def emit(envelope):
        if vs is None:
            return
        try:
            vs.sendall((json.dumps(envelope) + "\n").encode())
        except OSError:
            pass

    try:
        mmds = mmds_get_all()
    except Exception as e:  # noqa: BLE001 - guest-side best-effort reporting
        emit({"type": "error", "message": f"MMDS fetch failed: {e}"})
        return

    env = os.environ.copy()
    env.update({k: v for k, v in mmds.items() if k not in ("TASK", "COMMAND")})
    env["HOME"] = "/root"
    env["PATH"] = "/root/.local/bin:/usr/local/bin:/usr/bin:/bin"
    # MMDS is never reachable through a proxy, by construction of the
    # allowlist (design §10) — HTTP_PROXY/HTTPS_PROXY, once the egress step
    # delivers them via MMDS, only ever apply to everything else.
    if "HTTP_PROXY" in mmds:
        env["http_proxy"] = mmds["HTTP_PROXY"]
    if "HTTPS_PROXY" in mmds:
        env["https_proxy"] = mmds["HTTPS_PROXY"]
    env["no_proxy"] = MMDS_HOST

    if mmds.get("COMMAND"):
        run_command_mode(env, mmds, emit)
    elif mmds.get("TASK"):
        run_openhands_mode(env, mmds, emit)
    else:
        emit({"type": "error", "message": "MMDS has neither TASK nor COMMAND key set"})

    if vs is not None:
        vs.close()


if __name__ == "__main__":
    main()
