#!/usr/bin/env bash
# Recompute nix/vendor-hashes.json after any go.mod/go.sum change.
set -euo pipefail
cd "$(dirname "$0")/.."
file=nix/vendor-hashes.json
fake="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
for pkg in $(jq -r 'keys[]' "$file"); do
  jq --arg p "$pkg" --arg h "$fake" '.[$p] = $h' "$file" >"$file.tmp" && mv "$file.tmp" "$file"
  if out=$(nix build ".#$pkg" --no-link 2>&1); then
    echo "$pkg: unexpected success with a fake hash" >&2
    exit 1
  fi
  hash=$(awk '/got:/ { print $2 }' <<<"$out")
  if [[ -z $hash ]]; then
    echo "$out" >&2
    exit 1
  fi
  jq --arg p "$pkg" --arg h "$hash" '.[$p] = $h' "$file" >"$file.tmp" && mv "$file.tmp" "$file"
  echo "$pkg: $hash"
done
