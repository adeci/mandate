#!/bin/sh
# Installs mandate and its bundled connectors on a systemd Linux:
#
#   curl -fsSL https://github.com/adeci/mandate/releases/latest/download/install.sh | sh
#
# MANDATE_RELEASE_URL overrides where the release's files are fetched from.
set -eu

base=${MANDATE_RELEASE_URL:-https://github.com/adeci/mandate/releases/latest/download}

case $(uname -m) in
x86_64 | amd64) arch=x86_64 ;;
aarch64 | arm64) arch=aarch64 ;;
*)
	echo "mandate: no release for $(uname -m)" >&2
	exit 1
	;;
esac
tarball=mandate-linux-$arch.tar.gz

as_root() {
	if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"

curl -fsSL -o SHA256SUMS "$base/SHA256SUMS"
curl -fsSL -o "$tarball" "$base/$tarball"
grep "  $tarball\$" SHA256SUMS >"$tarball.sha256" || {
	echo "mandate: SHA256SUMS lists no $tarball" >&2
	exit 1
}
sha256sum -c --quiet "$tarball.sha256"

mkdir unpacked
tar -xzf "$tarball" -C unpacked
as_root install -D -m 0755 unpacked/mandate /usr/local/bin/mandate
as_root install -d -m 0755 /usr/local/lib/mandate/connectors
as_root install -m 0755 unpacked/connectors/* /usr/local/lib/mandate/connectors/

echo "mandate is installed. Next: sudo mandate setup"
