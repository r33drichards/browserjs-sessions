#!/bin/sh
# Fetches libcomputeruse.a for this machine from an SDK release.
#
#   install-lib.sh <version> [directory]      e.g. install-lib.sh 0.1.0 ./lib
#
# Then: CGO_LDFLAGS="-L<directory>" go build ./...
set -eu

REPO="r33drichards/computer-use"

version="${1:?usage: install-lib.sh <version> [directory]}"
directory="${2:-./lib}"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "no prebuilt library for $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "no prebuilt library for $(uname -m)" >&2; exit 1 ;;
esac

archive="libcomputeruse-$version-${os}_$arch.tar.gz"
base="https://github.com/$REPO/releases/download/sdk-v$version"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

curl -fsSL -o "$work/$archive" "$base/$archive"
curl -fsSL -o "$work/SHA256SUMS" "$base/SHA256SUMS"
expected="$(grep " $archive\$" "$work/SHA256SUMS" | cut -d' ' -f1)"
if command -v sha256sum > /dev/null 2>&1; then
  actual="$(sha256sum "$work/$archive" | cut -d' ' -f1)"
else
  actual="$(shasum -a 256 "$work/$archive" | cut -d' ' -f1)"
fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  echo "checksum mismatch for $archive" >&2
  exit 1
fi

mkdir -p "$directory"
tar -xzf "$work/$archive" -C "$directory" libcomputeruse.a
echo "$directory/libcomputeruse.a" >&2
echo "export CGO_LDFLAGS=\"-L$(cd "$directory" && pwd)\""
