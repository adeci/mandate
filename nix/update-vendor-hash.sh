#!/usr/bin/env bash
# Recompute nix/goVendorHash.txt after go.mod/go.sum changes.
set -euo pipefail
cd "$(dirname "$0")/.."
echo "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" >nix/goVendorHash.txt
if out=$(nix build .#sieve --no-link 2>&1); then
  echo "unexpected success with a fake hash" >&2
  exit 1
fi
hash=$(awk '/got:/ { print $2 }' <<<"$out")
if [[ -z $hash ]]; then
  echo "$out" >&2
  exit 1
fi
echo "$hash" >nix/goVendorHash.txt
echo "vendorHash: $hash"
