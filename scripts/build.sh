#!/bin/sh
# Build the release binaries for every supported platform into dist/.
# Usage: sh scripts/build.sh VERSION
set -eu
cd "$(dirname "$0")/.."
version=$1
flags="-s -w -buildid= -X main.version=$version"
export CGO_ENABLED=0
mkdir -p dist
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
    os="${target%/*}" arch="${target#*/}"
    if [ "$os" = windows ]; then
        sh scripts/windows_resources.sh "$version" "$arch" pipkin.exe Pipkin
        GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false -ldflags "$flags" \
            -o "dist/pipkin-$os-$arch.exe" .
        sh scripts/windows_resources.sh "$version" "$arch" pipkinw.exe "Pipkin helper"
        GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false -ldflags "$flags -H=windowsgui" \
            -o "dist/pipkinw-$os-$arch.exe" .
        rm rsrc_windows_*.syso
    else
        GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false -ldflags "$flags" \
            -o "dist/pipkin-$os-$arch" .
    fi
done
