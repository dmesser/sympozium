#!/bin/bash
# watch-cr-status.sh — Poll a Kubernetes resource until it reaches a ready state.
#
# Usage: watch-cr-status.sh <resource-type> <name> -n <namespace> [--timeout <seconds>]
#
# Checks .status.state, .status.phase, and .status.conditions for readiness.
# Returns JSON with the final status.
set -euo pipefail

RESOURCE=""
NAME=""
NAMESPACE=""
TIMEOUT=300
INTERVAL=10

while [[ $# -gt 0 ]]; do
  case "$1" in
    -n|--namespace) NAMESPACE="$2"; shift 2 ;;
    --timeout)      TIMEOUT="$2";   shift 2 ;;
    --interval)     INTERVAL="$2";  shift 2 ;;
    *)
      if [ -z "$RESOURCE" ]; then
        RESOURCE="$1"
      elif [ -z "$NAME" ]; then
        NAME="$1"
      fi
      shift
      ;;
  esac
done

if [ -z "$RESOURCE" ] || [ -z "$NAME" ] || [ -z "$NAMESPACE" ]; then
  echo '{"error":"Usage: watch-cr-status.sh <resource-type> <name> -n <namespace> [--timeout N]"}'
  exit 1
fi

ELAPSED=0
LAST_STATE="unknown"

while [ "$ELAPSED" -lt "$TIMEOUT" ]; do
  STATUS_JSON=$(oc get "$RESOURCE" "$NAME" -n "$NAMESPACE" -o json 2>/dev/null || echo '{"error":"resource not found"}')

  if echo "$STATUS_JSON" | jq -e '.error' >/dev/null 2>&1; then
    sleep "$INTERVAL"
    ELAPSED=$((ELAPSED + INTERVAL))
    continue
  fi

  STATE=$(echo "$STATUS_JSON" | jq -r '.status.state // .status.phase // "unknown"' 2>/dev/null)
  LAST_STATE="$STATE"

  READY_CONDITION=$(echo "$STATUS_JSON" | jq -r '
    [.status.conditions[]? |
      select((.type == "Ready" or .type == "Available" or .type == "Running" or .type == "Succeeded") and .status == "True")
    ] | length' 2>/dev/null || echo "0")

  if echo "$STATE" | grep -qiE '^(Ready|Running|Active|Succeeded|Available|Complete)$' || [ "${READY_CONDITION:-0}" -gt 0 ]; then
    echo "$STATUS_JSON" | jq -c '{
      status: "ready",
      resource: "'"$RESOURCE"'",
      name: "'"$NAME"'",
      namespace: "'"$NAMESPACE"'",
      state: (.status.state // .status.phase // "Ready"),
      elapsed_seconds: '"$ELAPSED"',
      conditions: [.status.conditions[]? | {type, status, message}]
    }' 2>/dev/null
    exit 0
  fi

  FAILED_CONDITION=$(echo "$STATUS_JSON" | jq -r '
    [.status.conditions[]? |
      select((.type == "Failed" or .type == "Error" or .type == "Degraded") and .status == "True")
    ] | length' 2>/dev/null || echo "0")

  if echo "$STATE" | grep -qiE '^(Failed|Error)$' || [ "${FAILED_CONDITION:-0}" -gt 0 ]; then
    echo "$STATUS_JSON" | jq -c '{
      status: "failed",
      resource: "'"$RESOURCE"'",
      name: "'"$NAME"'",
      namespace: "'"$NAMESPACE"'",
      state: (.status.state // .status.phase // "Failed"),
      elapsed_seconds: '"$ELAPSED"',
      conditions: [.status.conditions[]? | {type, status, message}]
    }' 2>/dev/null
    exit 1
  fi

  sleep "$INTERVAL"
  ELAPSED=$((ELAPSED + INTERVAL))
done

echo "$STATUS_JSON" | jq -c '{
  status: "timeout",
  resource: "'"$RESOURCE"'",
  name: "'"$NAME"'",
  namespace: "'"$NAMESPACE"'",
  state: (.status.state // .status.phase // "unknown"),
  elapsed_seconds: '"$TIMEOUT"',
  conditions: [.status.conditions[]? | {type, status, message}]
}' 2>/dev/null
exit 1
