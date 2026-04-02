#!/bin/bash
# acs-scan.sh — Discover ACS/StackRox Central, ensure registry integrations
# from the cluster pull-secret exist, and scan an image for CVEs.
#
# The script runs inside the skill sidecar which has curl, jq, oc, python3.
# It calls the ACS REST API directly via the Central K8s service (the sidecar
# can reach it over the cluster network) and uses oc exec only for roxctl
# (which lives at /stackrox/roxctl inside the Central pod).
#
# Usage: acs-scan.sh <image>
# Output: JSON with scan results or error info.
set -euo pipefail

IMAGE="${1:-}"
if [ -z "$IMAGE" ]; then
  echo '{"error":"Usage: acs-scan.sh <image-ref>"}'
  exit 1
fi

json_escape() { python3 -c "import json,sys; print(json.dumps(sys.stdin.read().strip()))"; }

# ── Step 1: Check for ACS CRDs ──────────────────────────────────────────────
if ! oc api-resources --api-group=platform.stackrox.io 2>/dev/null | grep -q centrals; then
  echo '{"acs_installed":false,"message":"ACS/StackRox CRDs not found on this cluster. Use pyxis-scan.sh as fallback for Red Hat images."}'
  exit 0
fi

# ── Step 2: Find Central instances ───────────────────────────────────────────
ACS_NS=$(oc get centrals.platform.stackrox.io -A \
  -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || true)
if [ -z "$ACS_NS" ]; then
  echo '{"acs_installed":true,"central_found":false,"message":"No Central instances found"}'
  exit 0
fi

# ── Step 3: Find Central pod ────────────────────────────────────────────────
CENTRAL_POD=$(oc get pods -n "$ACS_NS" -l app=central \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [ -z "$CENTRAL_POD" ]; then
  echo "{\"acs_installed\":true,\"central_found\":true,\"namespace\":\"$ACS_NS\",\"error\":\"No running Central pod found\"}"
  exit 0
fi

# ── Step 4: Retrieve admin password ─────────────────────────────────────────
ADMIN_PASS=$(oc get secret central-htpasswd -n "$ACS_NS" \
  -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)
if [ -z "$ADMIN_PASS" ]; then
  echo "{\"acs_installed\":true,\"central_found\":true,\"namespace\":\"$ACS_NS\",\"pod\":\"$CENTRAL_POD\",\"error\":\"Could not read admin password from central-htpasswd secret\"}"
  exit 0
fi

# Central K8s service — reachable from any pod in the cluster
ACS_API="https://central.${ACS_NS}.svc:443"
ACS_CREDS="admin:${ADMIN_PASS}"

REGISTRIES_ADDED=()
REGISTRIES_SKIPPED=()

# ── Step 5: Sync registry integrations from the global pull-secret ─────────
# curl runs in the sidecar (not oc exec) and calls Central via the K8s service.
PULL_SECRET_JSON=$(oc get secret pull-secret -n openshift-config \
  --template='{{index .data ".dockerconfigjson" | base64decode}}' 2>/dev/null || true)

if [ -n "$PULL_SECRET_JSON" ]; then
  EXISTING_INTEGRATIONS=$(curl -sSk -u "$ACS_CREDS" \
    "${ACS_API}/v1/imageintegrations" 2>/dev/null || echo '{}')

  EXISTING_ENDPOINTS=$(echo "$EXISTING_INTEGRATIONS" | \
    jq -r '[.integrations[]? | select(.type == "docker") | .docker.endpoint] | .[]' 2>/dev/null || true)

  PULL_SECRET_REGISTRIES=$(echo "$PULL_SECRET_JSON" | \
    jq -r '.auths // {} | keys[]' 2>/dev/null || true)

  for REGISTRY in $PULL_SECRET_REGISTRIES; do
    CLEAN_REG=$(echo "$REGISTRY" | sed 's|https\?://||; s|/.*||')
    [ -z "$CLEAN_REG" ] && continue

    if echo "$EXISTING_ENDPOINTS" | grep -qxF "$CLEAN_REG"; then
      REGISTRIES_SKIPPED+=("$CLEAN_REG")
      continue
    fi

    REG_AUTH=$(echo "$PULL_SECRET_JSON" | jq -r ".auths[\"$REGISTRY\"].auth // empty" 2>/dev/null)
    [ -z "$REG_AUTH" ] && continue

    DECODED=$(echo "$REG_AUTH" | base64 -d 2>/dev/null || true)
    REG_USER="${DECODED%%:*}"
    REG_PASS="${DECODED#*:}"

    if [ -z "$REG_USER" ] || [ -z "$REG_PASS" ]; then
      continue
    fi

    PAYLOAD=$(python3 -c "
import json, sys
print(json.dumps({
    'name': sys.argv[1],
    'type': 'docker',
    'categories': ['REGISTRY'],
    'docker': {
        'endpoint': sys.argv[1],
        'username': sys.argv[2],
        'password': sys.argv[3],
        'insecure': False
    },
    'skipTestIntegration': True
}))
" "$CLEAN_REG" "$REG_USER" "$REG_PASS")

    ADD_RESULT=$(curl -sSk -X POST -u "$ACS_CREDS" \
      -H "Content-Type: application/json" \
      -d "$PAYLOAD" \
      "${ACS_API}/v1/imageintegrations" 2>&1 || true)

    if echo "$ADD_RESULT" | jq -e '.id' >/dev/null 2>&1; then
      REGISTRIES_ADDED+=("$CLEAN_REG")
    elif echo "$ADD_RESULT" | grep -qi "already exists"; then
      REGISTRIES_SKIPPED+=("$CLEAN_REG")
    fi
  done
fi

REGISTRY_SYNC_INFO=$(python3 -c "
import json, sys
added = sys.argv[1].split(',') if sys.argv[1] else []
skipped = sys.argv[2].split(',') if sys.argv[2] else []
print(json.dumps({'registries_added': added, 'registries_already_present': skipped}))
" "$(IFS=,; echo "${REGISTRIES_ADDED[*]:-}")" "$(IFS=,; echo "${REGISTRIES_SKIPPED[*]:-}")")

# ── Step 6: Scan via roxctl inside the Central pod ──────────────────────────
# roxctl lives at /stackrox/roxctl inside the Central image (not on $PATH).
SCAN_OUTPUT=$(oc exec -n "$ACS_NS" "$CENTRAL_POD" -c central -- \
  sh -c "ROX_ADMIN_PASSWORD='${ADMIN_PASS}' /stackrox/roxctl \
    --insecure-skip-tls-verify -e localhost:8443 \
    image scan --image='${IMAGE}' --output=json --force" 2>&1 || true)

if echo "$SCAN_OUTPUT" | jq -e '.result' >/dev/null 2>&1; then
  # roxctl >=4.10 uses .result.vulnerabilities[] with cveId/cveSeverity fields
  # and a .result.summary object. Older versions used .result.components[].vulns[].
  HAS_VULNS=$(echo "$SCAN_OUTPUT" | jq -e '.result.vulnerabilities' >/dev/null 2>&1 && echo "new" || echo "old")

  if [ "$HAS_VULNS" = "new" ]; then
    echo "$SCAN_OUTPUT" | jq -c --argjson sync "$REGISTRY_SYNC_INFO" "{
      acs_installed: true,
      central_found: true,
      namespace: \"$ACS_NS\",
      image: \"$IMAGE\",
      scan_source: \"acs\",
      registry_sync: \$sync,
      scan: {
        summary: .result.summary,
        total_vulns: [.result.vulnerabilities[]? | select(.cveId != null and .cveId != \"\")] | length,
        critical:  [.result.vulnerabilities[]? | select(.cveSeverity == \"CRITICAL\")]  | length,
        important: [.result.vulnerabilities[]? | select(.cveSeverity == \"IMPORTANT\")] | length,
        moderate:  [.result.vulnerabilities[]? | select(.cveSeverity == \"MODERATE\")]  | length,
        low:       [.result.vulnerabilities[]? | select(.cveSeverity == \"LOW\")]       | length,
        top_vulns: [.result.vulnerabilities[]? | select(.cveId != null and .cveId != \"\") |
          {cve: .cveId, severity: .cveSeverity, cvss: .cveCVSS, component: .componentName, version: .componentVersion, fixedBy: .componentFixedVersion}
        ] | group_by(.cve) | [.[] | sort_by(.cvss) | reverse | .[0]] | sort_by(.cvss) | reverse | .[0:15]
      }
    }"
  else
    echo "$SCAN_OUTPUT" | jq -c --argjson sync "$REGISTRY_SYNC_INFO" "{
      acs_installed: true,
      central_found: true,
      namespace: \"$ACS_NS\",
      image: \"$IMAGE\",
      scan_source: \"acs\",
      registry_sync: \$sync,
      scan: {
        total_components: (.result.components | length),
        total_vulns: [.result.components[]?.vulns[]?] | length,
        critical:  [.result.components[]?.vulns[]? | select(.severity == \"CRITICAL_VULNERABILITY_SEVERITY\")]  | length,
        important: [.result.components[]?.vulns[]? | select(.severity == \"IMPORTANT_VULNERABILITY_SEVERITY\")] | length,
        moderate:  [.result.components[]?.vulns[]? | select(.severity == \"MODERATE_VULNERABILITY_SEVERITY\")]  | length,
        low:       [.result.components[]?.vulns[]? | select(.severity == \"LOW_VULNERABILITY_SEVERITY\")]       | length,
        top_vulns: [.result.components[]?.vulns[]? |
          {cve, severity, component: .componentName, fixedBy}
        ] | group_by(.cve) | [.[] | sort_by(.severity) | .[0]] | sort_by(.severity) | reverse | .[0:15]
      }
    }"
  fi
  exit 0
fi

# ── Step 7: Fallback — roxctl image check (policy violations) ───────────────
CHECK_OUTPUT=$(oc exec -n "$ACS_NS" "$CENTRAL_POD" -c central -- \
  sh -c "ROX_ADMIN_PASSWORD='${ADMIN_PASS}' /stackrox/roxctl \
    --insecure-skip-tls-verify -e localhost:8443 \
    image check --image='${IMAGE}' --output=json --force" 2>&1 || true)

if echo "$CHECK_OUTPUT" | jq -e '.results' >/dev/null 2>&1; then
  echo "$CHECK_OUTPUT" | jq -c --argjson sync "$REGISTRY_SYNC_INFO" "{
    acs_installed: true,
    central_found: true,
    namespace: \"$ACS_NS\",
    image: \"$IMAGE\",
    scan_source: \"acs\",
    registry_sync: \$sync,
    check: {
      summary: .summary,
      alerts: [.results[]?.violatedPolicies[]? | {name, severity, description}] | .[0:20]
    }
  }"
  exit 0
fi

# ── Step 8: Both failed — return error; caller should try pyxis-scan.sh ─────
ESCAPED_SCAN=$(echo "$SCAN_OUTPUT" | head -20 | json_escape)
echo "{\"acs_installed\":true,\"central_found\":true,\"namespace\":\"$ACS_NS\",\"pod\":\"$CENTRAL_POD\",\"registry_sync\":$REGISTRY_SYNC_INFO,\"error\":\"roxctl scan and check both failed. Use pyxis-scan.sh as fallback for Red Hat images.\",\"scan_stderr\":$ESCAPED_SCAN}"
