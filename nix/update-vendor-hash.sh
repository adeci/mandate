#!/usr/bin/env bash
# Recompute nix/vendor-hashes.json for the given packages, or all of them,
# after any go.mod/go.sum change, and after changing the root module's
# packages, which the connectors vendor. Remote builders come from nix's own
# configuration, e.g. NIX_CONFIG.
set -euo pipefail
cd "$(dirname "$0")/.."
file=nix/vendor-hashes.json
fake="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
if [[ $# -eq 0 ]]; then
  mapfile -t pkgs < <(jq -r 'keys[]' "$file")
else
  pkgs=("$@")
fi
set_hash() {
  jq --arg p "$1" --arg h "$2" '.[$p] = $h' "$file" >"$file.tmp" && mv "$file.tmp" "$file"
}
for pkg in "${pkgs[@]}"; do
  set_hash "$pkg" "$fake"
  # path: so that files git doesn't track yet count too.
  if out=$(nix build "path:$PWD#$pkg.goModules" --no-link 2>&1); then
    echo "$pkg: unexpected success with a fake hash" >&2
    exit 1
  fi
  hash=$(awk '/got:/ { print $2 }' <<<"$out")
  if [[ -z $hash ]]; then
    echo "$out" >&2
    exit 1
  fi
  set_hash "$pkg" "$hash"
  echo "$pkg: $hash"
done
