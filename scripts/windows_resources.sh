#!/bin/sh
# Write version information and an application manifest for the next Windows
# build. Go links rsrc_windows_<arch>.syso from the repository root.
# Usage: sh scripts/windows_resources.sh VERSION ARCH FILENAME DESCRIPTION
set -eu
cd "$(dirname "$0")/.."
version=$1 arch=$2 filename=$3 description=$4
case $version in
    '' | *[!0-9.]*) printf 'invalid version: %s\n' "$version" >&2; exit 1 ;;
esac
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' 0
cat > "$workdir/winres.json" <<EOF
{
  "RT_MANIFEST": {
    "#1": {
      "0409": {
        "description": "$description",
        "minimum-os": "win10",
        "execution-level": "as invoker",
        "dpi-awareness": "system"
      }
    }
  },
  "RT_VERSION": {
    "#1": {
      "0000": {
        "fixed": {
          "file_version": "$version",
          "product_version": "$version"
        },
        "info": {
          "0409": {
            "CompanyName": "Damian Worsdell",
            "FileDescription": "$description",
            "FileVersion": "$version",
            "InternalName": "${filename%.exe}",
            "LegalCopyright": "Copyright 2026 Damian Worsdell",
            "OriginalFilename": "$filename",
            "ProductName": "Pipkin",
            "ProductVersion": "$version"
          }
        }
      }
    }
  }
}
EOF
go run github.com/tc-hib/go-winres@v0.3.3 make --in "$workdir/winres.json" --arch "$arch" --out rsrc
