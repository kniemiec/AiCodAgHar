#!/usr/bin/env bash
# Print every central log line for one task, in time order (LG-5;
# platform-observability design §11). Runbook: docs/runbooks/observability.md.
#
#   deploy/observability/task-logs.sh <task_id> [days]     # or: make task-logs TASK=<task_id>
#
# Queries Loki for
#   {service_name=~"control-plane|container-launcher"} | task_id="<task_id>"
# over the last <days> (default 30), paging through the whole window. task_id
# is structured metadata, not a stream label (LG-4), so this is a filter, not
# a stream selector.
#
# Output, one line per record:
#   <RFC 3339 time, ms> <service> <LEVEL> <message> [key=value …]
#
# Only non-sensitive records reach Loki: lines marked logging.Sensitive()
# (prompt prefix, guest log tail, structured results) stay on each process's
# stderr.
#
# Env: LOKI_URL (default http://localhost:3100), TASK_LOGS_PAGE_SIZE (default
# 5000; records per Loki request). Needs curl and jq.
set -euo pipefail

usage() { echo "usage: $0 <task_id> [days]" >&2; exit 2; }
[[ $# -ge 1 && $# -le 2 ]] || usage
task_id=$1
days=${2:-30}
# Task IDs are [A-Za-z0-9_-] (orchestrator invariant); rejecting anything
# else also keeps the value from breaking out of the LogQL string.
[[ $task_id =~ ^[A-Za-z0-9_-]+$ ]] || { echo "$0: invalid task_id: $task_id" >&2; exit 2; }
[[ $days =~ ^[0-9]+$ && $days -ge 1 ]] || { echo "$0: days must be a positive integer" >&2; exit 2; }
command -v jq >/dev/null || { echo "$0: jq is required" >&2; exit 1; }

loki=${LOKI_URL:-http://localhost:3100}
query="{service_name=~\"control-plane|container-launcher\"} | task_id=\"${task_id}\""
limit=${TASK_LOGS_PAGE_SIZE:-5000}  # Loki's default max_entries_limit_per_query
[[ $limit =~ ^[0-9]+$ && $limit -ge 1 ]] || { echo "$0: TASK_LOGS_PAGE_SIZE must be a positive integer" >&2; exit 2; }
now=$(date +%s)
start="$(( now - days * 86400 ))000000000"
end="$(( now + 60 ))000000000"

# Record fields that are already in the fixed columns, or are SDK/pipeline
# bookkeeping, are not repeated as key=value pairs.
skip='["task_id","component","host","version","service_version","scope_name",
       "observed_timestamp","severity_text","severity_number","detected_level",
       "telemetry_sdk_language","telemetry_sdk_name","telemetry_sdk_version"]'

records=$(mktemp)
trap 'rm -f "$records"' EXIT

while :; do
  page=$(curl -sfG "$loki/loki/api/v1/query_range" \
    -H 'X-Loki-Response-Encoding-Flags: categorize-labels' \
    --data-urlencode "query=$query" \
    --data-urlencode "start=$start" --data-urlencode "end=$end" \
    --data-urlencode "limit=$limit" --data-urlencode "direction=forward") \
    || { echo "$0: Loki query failed at $loki (is the stack up? make compose-up)" >&2; exit 1; }

  # One compact JSON object per record: ts (ns string), service, metadata, line.
  # Timestamps are fixed-width 19-digit strings, so they sort as strings
  # (as numbers they would exceed jq's float precision).
  rows=$(jq -c '.data.result[] | .stream.service_name as $svc
                | .values[] | {ts: .[0], svc: $svc, line: .[1], md: (.[2].structuredMetadata // {})}' <<<"$page")
  n=0
  [[ -n $rows ]] && n=$(wc -l <<<"$rows" | tr -d ' ')

  if (( n < limit )); then
    [[ -n $rows ]] && printf '%s\n' "$rows" >>"$records"
    break
  fi
  # Full page: keep everything before the last timestamp and restart the next
  # page at that timestamp (inclusive), so records sharing it aren't split.
  last=$(jq -rs 'map(.ts) | max' <<<"$rows")
  if [[ $last == "$start" ]]; then
    echo "$0: more than $limit records share one timestamp; output truncated" >&2
    printf '%s\n' "$rows" >>"$records"
    break
  fi
  jq -c --arg last "$last" 'select(.ts != $last)' <<<"$rows" >>"$records"
  start=$last
done

if [[ ! -s $records ]]; then
  echo "$0: no log lines for task $task_id in the last $days days" >&2
  exit 1
fi

jq -rs --argjson skip "$skip" '
  sort_by(.ts) | .[]
  | ((.ts[0:10] | tonumber | strftime("%Y-%m-%dT%H:%M:%S")) + "." + .ts[10:13] + "Z") as $t
  | (.md.severity_text // .md.detected_level // "-" | ascii_upcase) as $lvl
  | ([.md | to_entries[] | select(.key as $k | $skip | index($k) | not)
      | "\(.key)=\(.value | tostring | gsub("\n"; "\\n"))"] | join(" ")) as $kv
  | [$t, .svc, $lvl, .line] + (if $kv == "" then [] else [$kv] end) | join(" ")
' "$records"
