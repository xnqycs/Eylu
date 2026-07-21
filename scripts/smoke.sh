#!/usr/bin/env bash
set -euo pipefail

binary="${1:?usage: smoke.sh <binary>}"
"$binary" version
root_help="$("$binary" --help)"
chat_help="$("$binary" chat --help)"
case "$root_help" in
  *"--resume string"*) ;;
  *) echo "root help is missing --resume <session-id>" >&2; exit 1 ;;
esac
case "$chat_help" in
  *"--resume string"*) ;;
  *) echo "chat help is missing --resume <session-id>" >&2; exit 1 ;;
esac
"$binary" sessions --help >/dev/null
"$binary" mcp --help >/dev/null

if "$binary" --resume >/dev/null 2>&1; then
  echo "bare --resume unexpectedly succeeded" >&2
  exit 1
fi
if "$binary" --continue >/dev/null 2>&1; then
  echo "removed --continue unexpectedly succeeded" >&2
  exit 1
fi
if "$binary" --session smoke --resume smoke >/dev/null 2>&1; then
  echo "--session and --resume unexpectedly succeeded together" >&2
  exit 1
fi
