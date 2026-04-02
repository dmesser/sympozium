#!/bin/bash
# newer-image.sh — Find newer semver tags for a container image.
# Uses skopeo list-tags + Python semver comparison.
# Authenticates via the OpenShift global pull-secret when available.
#
# Usage: newer-image.sh <image:tag>
# Output: JSON with patch/minor/major upgrades available.
set -euo pipefail

IMAGE="${1:-}"
if [ -z "$IMAGE" ]; then
  echo '{"error":"Usage: newer-image.sh <image:tag>"}'
  exit 1
fi

# ── Parse image reference ───────────────────────────────────────────────────
if echo "$IMAGE" | grep -q '@sha256:'; then
  echo '{"error":"Digest references not supported; provide <image>:<tag>","image":"'"$IMAGE"'"}'
  exit 1
fi

TAG="${IMAGE##*:}"
WITHOUT_TAG="${IMAGE%:*}"
if [ "$TAG" = "$IMAGE" ]; then
  echo '{"error":"No tag specified in image reference","image":"'"$IMAGE"'"}'
  exit 1
fi

REGISTRY="${WITHOUT_TAG%%/*}"
REPO="${WITHOUT_TAG#*/}"

if [ -z "$REPO" ] || [ "$REPO" = "$REGISTRY" ]; then
  echo '{"error":"Could not parse repository from image reference","image":"'"$IMAGE"'"}'
  exit 1
fi

# ── Prepare auth ────────────────────────────────────────────────────────────
AUTH_FLAG=""
AUTHFILE=$(mktemp /tmp/newer-image-auth.XXXXXX)
trap 'rm -f "$AUTHFILE"' EXIT

PULL_SECRET_JSON=$(oc get secret pull-secret -n openshift-config \
  --template='{{index .data ".dockerconfigjson" | base64decode}}' 2>/dev/null || true)

if [ -n "$PULL_SECRET_JSON" ]; then
  echo "$PULL_SECRET_JSON" > "$AUTHFILE"
  AUTH_FLAG="--authfile=$AUTHFILE"
fi

# ── List tags via skopeo ────────────────────────────────────────────────────
TAGS_JSON=$(skopeo list-tags $AUTH_FLAG "docker://${WITHOUT_TAG}" 2>/dev/null || true)

if [ -z "$TAGS_JSON" ] || ! echo "$TAGS_JSON" | jq -e '.Tags' >/dev/null 2>&1; then
  echo '{"error":"Failed to list tags","image":"'"$IMAGE"'","registry":"'"$REGISTRY"'"}'
  exit 1
fi

# ── Semver comparison in Python ─────────────────────────────────────────────
echo "$TAGS_JSON" | python3 -c "
import json, re, sys

data = json.load(sys.stdin)
tags = data.get('Tags', [])
current_tag = sys.argv[1]
image = sys.argv[2]

# Parse a tag into (major, minor, patch, pre-release-suffix) or None
SEMVER_RE = re.compile(r'^v?(\d+)\.(\d+)(?:\.(\d+))?(?:[-.](.+))?$')

def parse(tag):
    m = SEMVER_RE.match(tag)
    if not m:
        return None
    major, minor = int(m.group(1)), int(m.group(2))
    patch = int(m.group(3)) if m.group(3) is not None else 0
    suffix = m.group(4) or ''
    return (major, minor, patch, suffix)

current = parse(current_tag)
if current is None:
    print(json.dumps({'error': 'Current tag is not semver', 'tag': current_tag, 'image': image}))
    sys.exit(0)

cur_maj, cur_min, cur_pat, _ = current
has_v_prefix = current_tag.startswith('v')
cur_has_patch = '.' in current_tag.replace('v','',1) and current_tag.replace('v','',1).count('.') >= 2

# Only consider clean semver tags (no build suffixes like -source, -arm64, -timestamp)
CLEAN_SUFFIX = re.compile(r'^(|rc\d*|beta\d*|alpha\d*)$', re.IGNORECASE)

newer_patch = []
newer_minor = []
newer_major = []

for tag in tags:
    parsed = parse(tag)
    if parsed is None:
        continue
    maj, mi, pat, suffix = parsed
    if not CLEAN_SUFFIX.match(suffix):
        continue
    if suffix:  # skip pre-releases
        continue
    # Must match the v-prefix convention of the current tag
    if has_v_prefix and not tag.startswith('v'):
        continue
    if not has_v_prefix and tag.startswith('v'):
        continue

    if (maj, mi, pat) <= (cur_maj, cur_min, cur_pat):
        continue

    entry = {'tag': tag, 'major': maj, 'minor': mi, 'patch': pat}

    if maj == cur_maj and mi == cur_min and pat > cur_pat:
        newer_patch.append(entry)
    elif maj == cur_maj and mi > cur_min:
        newer_minor.append(entry)
    elif maj > cur_maj:
        newer_major.append(entry)

def sort_key(e): return (e['major'], e['minor'], e['patch'])

newer_patch.sort(key=sort_key)
newer_minor.sort(key=sort_key)
newer_major.sort(key=sort_key)

latest_patch = newer_patch[-1]['tag'] if newer_patch else None
latest_minor = newer_minor[-1]['tag'] if newer_minor else None
latest_major = newer_major[-1]['tag'] if newer_major else None

result = {
    'image': image,
    'current_tag': current_tag,
    'current_version': {'major': cur_maj, 'minor': cur_min, 'patch': cur_pat},
    'newer_patch': {
        'count': len(newer_patch),
        'latest': latest_patch,
        'all': [e['tag'] for e in newer_patch],
    },
    'newer_minor': {
        'count': len(newer_minor),
        'latest': latest_minor,
        'all': [e['tag'] for e in newer_minor[-5:]],
    },
    'newer_major': {
        'count': len(newer_major),
        'latest': latest_major,
        'all': [e['tag'] for e in newer_major[-3:]],
    },
}

print(json.dumps(result))
" "$TAG" "$IMAGE"
