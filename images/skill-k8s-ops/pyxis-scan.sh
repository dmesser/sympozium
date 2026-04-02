#!/bin/bash
# pyxis-scan.sh — Query the Red Hat Ecosystem Catalog (Pyxis) API for CVEs.
# Works for images hosted on registry.redhat.io and registry.access.redhat.com.
#
# Usage: pyxis-scan.sh <image>
# Output: JSON with vulnerability data.
set -euo pipefail

IMAGE="${1:-}"
if [ -z "$IMAGE" ]; then
  echo '{"error":"Usage: pyxis-scan.sh <image-ref>"}'
  exit 1
fi

PYXIS_BASE="https://catalog.redhat.com/api/containers/v1"

# ── Parse image reference ───────────────────────────────────────────────────
if echo "$IMAGE" | grep -q '@sha256:'; then
  echo '{"error":"Pyxis API does not support digest-only references; provide a tagged image","image":"'"$IMAGE"'"}'
  exit 1
fi

TAG="${IMAGE##*:}"
WITHOUT_TAG="${IMAGE%:*}"
if [ "$TAG" = "$IMAGE" ]; then TAG="latest"; WITHOUT_TAG="$IMAGE"; fi

REGISTRY="${WITHOUT_TAG%%/*}"
REPO="${WITHOUT_TAG#*/}"

if [ -z "$REPO" ] || [ "$REPO" = "$REGISTRY" ]; then
  echo '{"error":"Could not parse repository from image reference","image":"'"$IMAGE"'"}'
  exit 1
fi

# registry.redhat.io is an alias; Pyxis indexes under registry.access.redhat.com
case "$REGISTRY" in
  registry.redhat.io)        PYXIS_REGISTRY="registry.access.redhat.com" ;;
  registry.access.redhat.com) PYXIS_REGISTRY="registry.access.redhat.com" ;;
  *)
    echo '{"error":"Pyxis API only covers Red Hat registries (registry.redhat.io, registry.access.redhat.com)","registry":"'"$REGISTRY"'","image":"'"$IMAGE"'"}'
    exit 1
    ;;
esac

ARCH="amd64"

# ── Look up Pyxis image via filter-based search ────────────────────────────
# The path-based endpoint (/repositories/registry/.../repository/.../images)
# breaks for repos with slashes (e.g. openshift-pipelines/controller-rhel9).
# The filter-based /images endpoint handles this correctly.
FILTER="repositories.registry==${PYXIS_REGISTRY}"
FILTER="${FILTER};repositories.repository==${REPO}"
FILTER="${FILTER};repositories.tags.name==${TAG}"
FILTER="${FILTER};architecture==${ARCH}"

ENCODED_FILTER=$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=''))" "$FILTER")

IMAGES_JSON=$(curl -sS "${PYXIS_BASE}/images?filter=${ENCODED_FILTER}&page_size=1" 2>/dev/null || true)

IMAGE_ID=$(echo "$IMAGES_JSON" | jq -r '.data[0]?._id // empty' 2>/dev/null)

if [ -z "$IMAGE_ID" ]; then
  echo '{"error":"Image not found in Red Hat Catalog","registry":"'"$REGISTRY"'","pyxis_registry":"'"$PYXIS_REGISTRY"'","repo":"'"$REPO"'","tag":"'"$TAG"'"}'
  exit 1
fi

# ── Fetch vulnerabilities ───────────────────────────────────────────────────
VULN_JSON=$(curl -sS "${PYXIS_BASE}/images/id/${IMAGE_ID}/vulnerabilities" 2>/dev/null || true)

echo "$VULN_JSON" | jq -c "{
  source: \"Red Hat Ecosystem Catalog (Pyxis)\",
  image: \"$IMAGE\",
  registry: \"$REGISTRY\",
  repo: \"$REPO\",
  tag: \"$TAG\",
  image_id: \"$IMAGE_ID\",
  total_vulns: ([.data[]?] | length),
  critical:  ([.data[]? | select(.severity == \"Critical\")]  | length),
  important: ([.data[]? | select(.severity == \"Important\")] | length),
  moderate:  ([.data[]? | select(.severity == \"Moderate\")]  | length),
  low:       ([.data[]? | select(.severity == \"Low\")]       | length),
  cves: [.data[]? | {
    cve_id,
    severity,
    advisory_id,
    cvss3_score: .cvss3_scoring_vector
  }] | sort_by(.severity) | .[0:30]
}" 2>/dev/null
