#!/bin/sh
# Exercise the installer without network access or installing a real service.
set -eu
repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
testdir=$(mktemp -d)
trap 'rm -rf "$testdir"' 0
mkdir -p "$testdir/bin" "$testdir/assets"
export TEST_INSTALL_DIR="$testdir"
export PATH="$testdir/bin:$PATH"
unset PIPKIN_VERSION PIPKIN_REPOSITORY

cat > "$testdir/assets/pipkin" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" > "$TEST_INSTALL_DIR/installed"
EOF
if command -v sha256sum >/dev/null 2>&1; then
    sum=$(sha256sum "$testdir/assets/pipkin" | cut -d ' ' -f 1)
else
    sum=$(shasum -a 256 "$testdir/assets/pipkin" | cut -d ' ' -f 1)
fi
printf '%s  pipkin-linux-arm64\n' "$sum" > "$testdir/assets/SHA256SUMS"
cat > "$testdir/bin/uname" <<'EOF'
#!/bin/sh
case "$1" in
    -s) printf '%s\n' Linux ;;
    -m) printf '%s\n' "${TEST_ARCH:-aarch64}" ;;
esac
EOF
cat > "$testdir/bin/curl" <<'EOF'
#!/bin/sh
if [ "$1" = -fsSLI ]; then
    printf '%s' https://github.com/damian-w/pipkin-cli/releases/tag/v1.2.3
    exit
fi
case "$2" in
    https://github.com/damian-w/pipkin-cli/releases/download/v1.2.3/pipkin-linux-arm64)
        cp "$TEST_INSTALL_DIR/assets/pipkin" "$4" ;;
    https://github.com/damian-w/pipkin-cli/releases/download/v1.2.3/SHA256SUMS)
        cp "$TEST_INSTALL_DIR/assets/SHA256SUMS" "$4" ;;
    *) exit 1 ;;
esac
EOF
chmod +x "$testdir/bin/"*
sh "$repo/install.sh"
[ "$(cat "$testdir/installed")" = install ]
rm "$testdir/installed"
printf 'corrupt\n' > "$testdir/assets/SHA256SUMS"
if sh "$repo/install.sh" > "$testdir/output" 2>&1; then
    echo 'Installer accepted an invalid checksum' >&2
    exit 1
fi
[ ! -e "$testdir/installed" ]
if TEST_ARCH=riscv64 sh "$repo/install.sh" > "$testdir/output" 2>&1; then
    echo 'Installer accepted an unsupported architecture' >&2
    exit 1
fi
echo 'Installer checks passed.'
