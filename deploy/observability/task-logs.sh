#!/usr/bin/env bash
# Prints every control-plane and launcher log line for one task, from Loki,
# in time order (platform-observability design §11, LG-5):
#
#   deploy/observability/task-logs.sh <task_id>      # or: make task-logs TASK=<task_id>
#
# task_id is Loki structured metadata, not an index label, so the query is
# a stream selector on the two services plus a `| task_id=` filter over the
# last 30 days (Loki keeps 14). Lines come back as one stream per distinct
# label set, so they are merged and sorted here.
#
# LOKI_URL defaults to the loopback port deploy/docker-compose.yml publishes.
set -euo pipefail

task_id="${1:-}"
if [[ -z "$task_id" ]]; then
  echo "usage: $0 <task_id>" >&2
  exit 2
fi
# Task IDs are [A-Za-z0-9_-] (CLAUDE.md); anything else can't be one and
# must not be spliced into the LogQL query.
if [[ ! "$task_id" =~ ^[A-Za-z0-9_-]+$ ]]; then
  echo "$0: invalid task id: $task_id" >&2
  exit 2
fi

loki_url="${LOKI_URL:-http://localhost:3100}"
query="{service_name=~\"control-plane|container-launcher\"} | task_id=\"${task_id}\""
now_s=$(date +%s)
start_ns="$(( now_s - 30 * 24 * 3600 ))000000000"
end_ns="$(( now_s + 60 ))000000000"

if ! resp=$(curl -fsS -G "${loki_url}/loki/api/v1/query_range" \
  --data-urlencode "query=${query}" \
  --data-urlencode "start=${start_ns}" \
  --data-urlencode "end=${end_ns}" \
  --data-urlencode "limit=5000" \
  --data-urlencode "direction=forward"); then
  echo "$0: Loki query against ${loki_url} failed (is the compose stack up?)" >&2
  exit 1
fi

printf '%s' "$resp" | python3 -c '
import datetime, json, sys

resp = json.load(sys.stdin)
if resp.get("status") != "success":
    sys.exit("loki query failed: %s" % resp)
index = {"service_name", "host_name", "deployment_environment", "detected_level", "level", "service_version"}
rows = []
for stream in resp["data"]["result"]:
    labels = stream["stream"]
    extra = " ".join("%s=%s" % (k, v) for k, v in sorted(labels.items()) if k not in index)
    for ts, line in stream["values"]:
        rows.append((int(ts), labels.get("service_name", "?"), labels.get("detected_level", labels.get("level", "")), line, extra))
if not rows:
    sys.exit("no log lines found for this task in the last 30 days")
rows.sort(key=lambda r: r[0])
for ts, svc, level, line, extra in rows:
    when = datetime.datetime.fromtimestamp(ts / 1e9, datetime.timezone.utc).isoformat(timespec="milliseconds")
    print(when, svc, level.upper(), line, extra)
'
