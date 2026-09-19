#!/usr/bin/env bash
#
# Eylu pre-commit gate: reproduces the GitHub Actions "test" and "quality" jobs
# on the developer machine.
#
#   scripts/verify.sh [--skip-smoke] [--skip-static] [--skip-extras]
#
# Formatting is judged on file *content*, never on the raw working tree.  With
# core.autocrlf=true every checked-out file carries CRLF line endings and
# `gofmt -l` reports such files as unformatted even when nobody touched them.
# A naive `gofmt -l .` therefore fails on a clean tree.  Two sources are used
# instead:
#
#   committed  the index blob byte for byte - exactly what CI checks out.
#              The command forces core.autocrlf off so no smudge filter
#              rewrites the line endings on the way out.
#   worktree   the checked-out bytes with CR removed.  This still catches a
#              real formatting error in an uncommitted edit while a CRLF
#              checkout artifact is not reported.
#
# Exits non-zero on the first failing phase.
set -euo pipefail

usage() {
    cat <<'EOF'
usage: scripts/verify.sh [--skip-smoke] [--skip-static] [--skip-extras]

Phases, in order:
  1. gofmt   committed content (index bytes) and CRLF-normalised working tree
  2. verify  go mod verify, go vet ./...
  3. static  staticcheck ./...                    (--skip-static)
  4. extras  third-party notices, actionlint, govulncheck   (--skip-extras)
  5. test    go test ./...
  6. smoke   go build + scripts/smoke.sh + scripts/smoke.ps1   (--skip-smoke)

govulncheck needs the vulnerability database, so it reaches the network on a
cold cache; --skip-extras is the offline path.

The race detector and the bounded fuzz search are CI-only: run
`go test -race ./...` and `go test -run '^$' -fuzz <target> -fuzztime 30s` on the
CI matrix. The corpus under testdata/fuzz is replayed here by `go test ./...`.
EOF
}

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

skip_smoke=0
skip_static=0
skip_extras=0
while [ $# -gt 0 ]; do
    case "$1" in
        --skip-smoke) skip_smoke=1 ;;
        --skip-static) skip_static=1 ;;
        --skip-extras) skip_extras=1 ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            printf 'verify.sh: unknown argument: %s\n\n' "$1" >&2
            usage >&2
            exit 2
            ;;
    esac
    shift
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

step() { printf '\n==> %s\n' "$*"; }
fail() {
    printf '\nFAIL: %s\n' "$*" >&2
    exit 1
}

# report_format <label> <tree>
# Runs gofmt over a materialised tree and prints the offending paths relative to
# the repository root.  Returns non-zero when anything would be reformatted.
report_format() {
    local label="$1" tree="$2" out line rel
    out="$(gofmt -l "$tree" 2>&1 || true)"
    [ -n "$out" ] || return 0
    printf '\ngofmt would reformat these files (%s):\n' "$label" >&2
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        line="${line//\\//}"
        rel="${line#*"${tree##*/}"/}"
        printf '  %s\n' "$rel" >&2
    done <<<"$out"
    printf '\nrun: gofmt -w <file>\n' >&2
    return 1
}

step "gofmt: committed content"
mkdir -p "$work/fmt_committed"
git -c core.autocrlf=false -c core.eol=lf \
    checkout-index -a -f --prefix="$work/fmt_committed/" ||
    fail "could not materialise the index"
report_format "committed content" "$work/fmt_committed" ||
    fail "the committed content is not gofmt-clean; CI would reject it"

step "gofmt: working tree (CRLF normalised)"
while IFS= read -r path; do
    [ -n "$path" ] || continue
    mkdir -p "$work/fmt_worktree/$(dirname -- "$path")"
    tr -d '\r' <"$path" >"$work/fmt_worktree/$path"
done < <(git ls-files --cached --others --exclude-standard -- "*.go")
report_format "working tree" "$work/fmt_worktree" ||
    fail "the working tree is not gofmt-clean (CRLF line endings are already ignored)"

step "go mod verify"
go mod verify

step "go vet ./..."
go vet ./...

if [ "$skip_static" -eq 0 ]; then
    step "staticcheck ./... (honnef.co/go/tools/cmd/staticcheck@v0.7.0)"
    go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...
fi

if [ "$skip_extras" -eq 0 ]; then
    step "third-party notices"
    go run ./scripts/generate-third-party-notices -check

    step "actionlint (github.com/rhysd/actionlint/cmd/actionlint@v1.7.12)"
    go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12

    # Pinned rather than @latest: the newest govulncheck already wants a newer Go
    # than go.mod asks for, and a gate that silently changes toolchain is a gate
    # nobody can reproduce.
    step "govulncheck ./... (golang.org/x/vuln/cmd/govulncheck@v1.1.4)"
    go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
fi

step "go test ./..."
go test ./...

if [ "$skip_smoke" -eq 0 ]; then
    exe=""
    case "$(uname -s)" in
        MINGW* | MSYS* | CYGWIN*) exe=".exe" ;;
    esac

    step "go build -trimpath -o dist/eylu$exe ."
    mkdir -p dist
    go build -trimpath -o "dist/eylu$exe" .

    step "scripts/smoke.sh"
    bash scripts/smoke.sh "./dist/eylu$exe"

    if [ -n "$exe" ] && command -v pwsh >/dev/null 2>&1; then
        step "scripts/smoke.ps1"
        pwsh -NoProfile -File scripts/smoke.ps1 -Binary "./dist/eylu$exe"
    fi
fi

skipped=""
[ "$skip_smoke" -eq 0 ] || skipped="$skipped --skip-smoke"
[ "$skip_static" -eq 0 ] || skipped="$skipped --skip-static"
[ "$skip_extras" -eq 0 ] || skipped="$skipped --skip-extras"
if [ -n "$skipped" ]; then
    printf '\nOK: every gate passed (skipped:%s).\n' "$skipped"
else
    printf '\nOK: every gate passed.\n'
fi
