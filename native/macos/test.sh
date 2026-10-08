#!/usr/bin/env bash
set -euo pipefail
if [[ "$(uname -s)" != Darwin ]]; then
    printf '%s\n' 'Swift/Metal validation requires a macOS host; no native test was performed.' >&2
    exit 1
fi
source_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
test_directory="$(mktemp -d "${TMPDIR:-/tmp}/mini-fabrics-swift.XXXXXX")"
trap 'rm -rf "$test_directory"' EXIT
if [[ "$(uname -m)" == arm64 ]]; then
    export MACOSX_DEPLOYMENT_TARGET=11.0
else
    export MACOSX_DEPLOYMENT_TARGET=10.15
fi
xcrun swiftc -O -warnings-as-errors -framework Foundation -framework Metal \
    "$source_directory/SystemInfo.swift" -o "$test_directory/fabrics-system-info"
python3 "$source_directory/verify.py" --helper "$test_directory/fabrics-system-info" "$@"
