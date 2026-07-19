#!/usr/bin/env bash
set -euo pipefail

binary="${1:?usage: smoke.sh <binary>}"
"$binary" version
"$binary" --help >/dev/null
"$binary" chat --help >/dev/null
"$binary" sessions --help >/dev/null
"$binary" mcp --help >/dev/null
