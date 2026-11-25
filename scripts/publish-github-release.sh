#!/usr/bin/env bash
# Publish release notes only for an existing stable version tag.
set -euo pipefail
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if [[ $(bash "$script_dir/is-stable-release.sh" "$GITHUB_REF") != true ]]; then
  exit 0
fi
[[ "$GITHUB_REF" == "refs/tags/$RELEASE_TAG" ]]
notes="docs/releases/$RELEASE_TAG.md"
test -s "$notes"
body="$RUNNER_TEMP/prismvox-release.md"
sed '1{/^# /d;}' "$notes" > "$body"
if is_draft=$(gh release view "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --json isDraft --jq .isDraft 2>/dev/null); then
  if [[ "$is_draft" == true ]]; then
    gh release edit "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" \
      --draft=false --prerelease=false --title "PrismVox ${RELEASE_TAG#v}" \
      --notes-file "$body" --latest
  else
    echo "Release $RELEASE_TAG already exists."
  fi
else
  gh release create "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --verify-tag \
    --title "PrismVox ${RELEASE_TAG#v}" --notes-file "$body" --latest
fi
