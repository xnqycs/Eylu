#!/usr/bin/env bash
#
# Self-test for scripts/verify.sh.
#
# The formatting phase is the delicate part of the gate: it must tell a CRLF
# checkout artifact (harmless, must pass) apart from real uncommitted formatting
# damage (must fail), and it must still judge committed content the way CI does.
# The cases below pin all of that down, plus the staticcheck phase.
#
# Every case builds a throwaway git repository, so the Eylu working tree is
# never touched.
#
#   bash scripts/verify_selftest.sh
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$here/.." && pwd)"

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

repo="$scratch/repo"
passed=0
failed=0

ok() { printf 'ok   - %s\n' "$1"; passed=$((passed + 1)); }
no() {
    printf 'FAIL - %s\n' "$1" >&2
    failed=$((failed + 1))
}

reset_repo() {
    rm -rf "$repo"
    mkdir -p "$repo/scripts" "$repo/.github/workflows"
    cp "$here/verify.sh" "$repo/scripts/verify.sh"
    cp "$repo_root/.github/workflows/ci.yml" "$repo/.github/workflows/ci.yml"
    printf 'module selftest\n\ngo 1.25.8\n' >"$repo/go.mod"
    cat >"$repo/main.go" <<'EOF'
package main

func main() {}
EOF
    (
        cd "$repo"
        git init -q .
        git config core.autocrlf false
        git config user.email selftest@example.invalid
        git config user.name selftest
        git add -A
        git commit -qm "init"
    )
}

# Rewrites every Go file with CRLF line endings without changing anything else,
# which is what a core.autocrlf=true checkout produces.
crlf_everywhere() {
    local file temp line
    while IFS= read -r -d '' file; do
        temp="$file.crlf"
        : >"$temp"
        while IFS= read -r line || [ -n "$line" ]; do
            printf '%s\r\n' "$line" >>"$temp"
        done <"$file"
        mv "$temp" "$file"
    done < <(find "$repo" -name '*.go' -not -path '*/.git/*' -print0)
}

out=""
rc=0
run_verify() {
    rc=0
    out="$(cd "$repo" && bash scripts/verify.sh "$@" 2>&1)" || rc=$?
}

expect_rc() {
    if [ "$rc" -eq "$2" ]; then
        ok "$1"
    else
        no "$1 (exit $rc, want $2)"
        printf '%s\n' "$out" | sed 's/^/    | /' >&2
    fi
}

expect_contains() {
    case "$out" in
        *"$2"*)
            ok "$1"
            ;;
        *)
            no "$1 (output does not contain: $2)"
            printf '%s\n' "$out" | sed 's/^/    | /' >&2
            ;;
    esac
}

printf '\n== 1. clean tree passes\n'
reset_repo
run_verify --skip-smoke --skip-static --skip-extras
expect_rc "clean tree exits 0" 0

printf '\n== 2. CRLF checkout artifact is not a formatting error\n'
reset_repo
crlf_everywhere
if [ "$(gofmt -l "$repo" | wc -l)" -eq 0 ]; then
    no "fixture does not reproduce the CRLF false positive (plain gofmt -l reports nothing)"
else
    ok "fixture reproduces the CRLF false positive of plain gofmt -l"
fi
run_verify --skip-smoke --skip-static --skip-extras
expect_rc "CRLF working tree still exits 0" 0

printf '\n== 3. formatting error inside a CRLF working tree is reported\n'
reset_repo
printf 'package main\n\nfunc  main() {}\n' >"$repo/main.go"
crlf_everywhere
run_verify --skip-smoke --skip-static --skip-extras
expect_rc "unformatted working tree exits non-zero" 1
expect_contains "the unformatted file is named" "main.go"

printf '\n== 4. committed CRLF content is reported the way CI reports it\n'
reset_repo
crlf_everywhere
(cd "$repo" && git add -A && git commit -qm "crlf")
run_verify --skip-smoke --skip-static --skip-extras
expect_rc "committed CRLF exits non-zero" 1
expect_contains "the failure comes from the committed content" "committed content"

printf '\n== 5. staticcheck-only failure is reported\n'
reset_repo
cat >"$repo/main.go" <<'EOF'
package main

func value() int { return 1 }

func main() {
	x := value()
	x = 2
	_ = x
}
EOF
gofmt -w "$repo/main.go"
(cd "$repo" && git add -A && git commit -qm "dead assignment")
run_verify --skip-smoke --skip-extras
expect_rc "dead assignment exits non-zero" 1
expect_contains "the staticcheck reason is reported" "SA4006"

printf '\n----------------------------------------\n'
printf '%d passed, %d failed\n' "$passed" "$failed"
[ "$failed" -eq 0 ] || exit 1
