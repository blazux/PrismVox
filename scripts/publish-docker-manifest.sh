#!/usr/bin/env bash
# Publish complete native AMD64/ARM64 images after validating both digests.
set -euo pipefail
shopt -s nullglob
refs=()
for arch in amd64 arm64; do
  digests=("$DIGESTS_DIR/$arch/"*)
  [[ ${#digests[@]} == 1 ]] || { echo "::error::Expected one $arch digest"; exit 1; }
  digest=${digests[0]##*/}
  [[ -f "${digests[0]}" && "$digest" =~ ^[0-9a-f]{64}$ ]]
  refs+=("$IMAGE@sha256:$digest")
done
image_tags=()
args=()
while IFS= read -r tag; do
  [[ -n "$tag" ]] || continue
  [[ "$tag" == "$IMAGE:"* ]] || { echo '::error::Unexpected image tag'; exit 1; }
  image_tags+=("$tag")
  args+=(--tag "$tag")
done <<< "$TAGS"
[[ ${#image_tags[@]} -gt 0 ]]
validate_index() {
  python3 - "$1" <<'CHECK'
import json,sys
with open(sys.argv[1]) as source:
    data=json.load(source)
platforms=[]
for item in data.get('manifests',[]):
    platform=item.get('platform',{})
    pair=(platform.get('os'),platform.get('architecture'))
    if pair != ('unknown','unknown'):
        platforms.append(pair)
assert sorted(platforms)==[('linux','amd64'),('linux','arm64')], 'Expected exactly linux/amd64 and linux/arm64'
CHECK
}
# Validate the merged platforms before changing any published tag.
docker buildx imagetools create --dry-run "${refs[@]}" > "$RUNNER_TEMP/manifest-candidate.json"
validate_index "$RUNNER_TEMP/manifest-candidate.json"
docker buildx imagetools create "${args[@]}" "${refs[@]}"
for tag in "${image_tags[@]}"; do
  docker buildx imagetools inspect "$tag" --raw > "$RUNNER_TEMP/manifest-published.json"
  validate_index "$RUNNER_TEMP/manifest-published.json"
done
