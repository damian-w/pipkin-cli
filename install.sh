#!/bin/sh
# Installs the Pipkin helper on macOS or Linux:
#   curl -fsSL https://pipkin.io/install.sh | sh
set -eu

fail() {
    printf 'Pipkin: %s\n' "$1" >&2
    exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required."
if command -v sha256sum >/dev/null 2>&1; then
    checksum=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    checksum=shasum
else
    fail "sha256sum or shasum is required to verify the download."
fi

case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) fail "this installer supports macOS and Linux. On Windows, run in PowerShell: irm https://pipkin.io/install.ps1 | iex" ;;
esac
case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) fail "unsupported processor: $(uname -m)." ;;
esac
asset="pipkin-$os-$arch"

base="https://github.com/${PIPKIN_REPOSITORY:-damian-w/pipkin-cli}"
tag=${PIPKIN_VERSION:-}
if [ -z "$tag" ]; then
    resolved=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$base/releases/latest") ||
        fail "could not find a published release."
    case "$resolved" in
        "$base/releases/tag/"*) tag=${resolved##*/} ;;
        *) fail "no published release found." ;;
    esac
fi
case "$tag" in
    '' | *[!a-zA-Z0-9._-]*) fail "invalid release version." ;;
esac
release="$base/releases/download/$tag"

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' 0
trap 'exit 1' INT TERM
curl -fsSL "$release/$asset" -o "$workdir/pipkin" || fail "could not download the helper."
curl -fsSL "$release/SHA256SUMS" -o "$workdir/SHA256SUMS" || fail "could not download checksums."

expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1 }' "$workdir/SHA256SUMS")
if [ "$checksum" = sha256sum ]; then
    actual=$(sha256sum "$workdir/pipkin" | cut -d ' ' -f 1)
else
    actual=$(shasum -a 256 "$workdir/pipkin" | cut -d ' ' -f 1)
fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] ||
    fail "the downloaded helper failed its checksum; nothing was installed."

chmod +x "$workdir/pipkin"
"$workdir/pipkin" install "$@"
