# Eylu pre-commit gate: reproduces the GitHub Actions "test" and "quality" jobs
# on the developer machine.
#
#   scripts/verify.ps1 [-SkipSmoke] [-SkipStatic] [-SkipExtras]
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

[CmdletBinding()]
param(
    [switch]$SkipSmoke,
    [switch]$SkipStatic,
    [switch]$SkipExtras,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'

function Show-Usage {
    @'
usage: scripts/verify.ps1 [-SkipSmoke] [-SkipStatic] [-SkipExtras]

Phases, in order:
  1. gofmt   committed content (index bytes) and CRLF-normalised working tree
  2. verify  go mod verify, go vet ./...
  3. static  staticcheck ./...                    (-SkipStatic)
  4. extras  third-party notices, actionlint, govulncheck   (-SkipExtras)
  5. test    go test ./...
  6. smoke   go build + smoke.ps1 + smoke.sh      (-SkipSmoke)

govulncheck needs the vulnerability database, so it reaches the network on a
cold cache; -SkipExtras is the offline path.

The race detector and the bounded fuzz search are CI-only: run
`go test -race ./...` and `go test -run '^$' -fuzz <target> -fuzztime 30s` on the
CI matrix. The corpus under testdata/fuzz is replayed here by `go test ./...`.
'@
}

if ($Help) {
    Show-Usage
    exit 0
}

$root = Split-Path -Parent $PSScriptRoot
Set-Location -LiteralPath $root

$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$onWindows = $env:OS -eq 'Windows_NT'
$hostExe = (Get-Process -Id $PID).Path

function Write-Step([string]$Message) {
    Write-Host ''
    Write-Host "==> $Message"
}

function Invoke-Native([string]$What, [scriptblock]$Command) {
    & $Command
    if ($LASTEXITCODE -ne 0) {
        throw "$What failed with exit code $LASTEXITCODE"
    }
}

# Runs gofmt over a materialised tree and prints the offending paths relative to
# the repository root.  Throws when anything would be reformatted.
function Test-FormatTree([string]$Label, [string]$Tree) {
    $output = @(gofmt -l "$Tree")
    if ($LASTEXITCODE -ne 0) {
        throw "gofmt failed with exit code $LASTEXITCODE"
    }
    if ($output.Count -eq 0) {
        return
    }
    $leaf = [regex]::Escape((Split-Path -Leaf $Tree))
    Write-Host ''
    Write-Host "gofmt would reformat these files ($Label):"
    foreach ($line in $output) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $relative = ($line -replace '\\', '/') -replace "^.*$leaf/", ''
        Write-Host "  $relative"
    }
    Write-Host ''
    Write-Host 'run: gofmt -w <file>'
    throw "$Label is not gofmt-clean"
}

# Copies the index blobs into $Destination, byte for byte.
function New-CommittedTree([string]$Destination) {
    New-Item -ItemType Directory -Force -Path $Destination | Out-Null
    $prefix = ($Destination -replace '\\', '/') + '/'
    Invoke-Native 'git checkout-index' {
        git -c core.autocrlf=false -c core.eol=lf checkout-index -a -f --prefix="$prefix"
    }
}

# Copies every tracked and untracked Go file into $Destination with CR removed.
function New-WorktreeTree([string]$Destination) {
    New-Item -ItemType Directory -Force -Path $Destination | Out-Null
    $files = @(git ls-files --cached --others --exclude-standard -- '*.go')
    if ($LASTEXITCODE -ne 0) {
        throw "git ls-files failed with exit code $LASTEXITCODE"
    }
    foreach ($path in $files) {
        if ([string]::IsNullOrWhiteSpace($path)) { continue }
        $relative = $path -replace '/', [System.IO.Path]::DirectorySeparatorChar
        $source = Join-Path $root $relative
        $target = Join-Path $Destination $relative
        $parent = Split-Path -Parent $target
        if ($parent) {
            New-Item -ItemType Directory -Force -Path $parent | Out-Null
        }
        $text = [System.Text.Encoding]::UTF8.GetString([System.IO.File]::ReadAllBytes($source))
        [System.IO.File]::WriteAllText($target, $text.Replace("`r", ''), $utf8NoBom)
    }
}

$work = Join-Path ([System.IO.Path]::GetTempPath()) ('eylu-verify-' + [System.Guid]::NewGuid().ToString('N'))

try {
    New-Item -ItemType Directory -Force -Path $work | Out-Null

    Write-Step 'gofmt: committed content'
    New-CommittedTree (Join-Path $work 'fmt_committed')
    Test-FormatTree 'committed content' (Join-Path $work 'fmt_committed')

    Write-Step 'gofmt: working tree (CRLF normalised)'
    New-WorktreeTree (Join-Path $work 'fmt_worktree')
    Test-FormatTree 'working tree (CRLF line endings are already ignored)' (Join-Path $work 'fmt_worktree')

    Write-Step 'go mod verify'
    Invoke-Native 'go mod verify' { go mod verify }

    Write-Step 'go vet ./...'
    Invoke-Native 'go vet' { go vet ./... }

    if (-not $SkipStatic) {
        Write-Step 'staticcheck ./... (honnef.co/go/tools/cmd/staticcheck@v0.7.0)'
        Invoke-Native 'staticcheck' { go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./... }
    }

    if (-not $SkipExtras) {
        Write-Step 'third-party notices'
        Invoke-Native 'third-party notices' { go run ./scripts/generate-third-party-notices -check }

        Write-Step 'actionlint (github.com/rhysd/actionlint/cmd/actionlint@v1.7.12)'
        Invoke-Native 'actionlint' { go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 }

        # Pinned rather than @latest: the newest govulncheck already wants a newer
        # Go than go.mod asks for, and a gate that silently changes toolchain is a
        # gate nobody can reproduce.
        Write-Step 'govulncheck ./... (golang.org/x/vuln/cmd/govulncheck@v1.1.4)'
        Invoke-Native 'govulncheck' { go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./... }
    }

    Write-Step 'go test ./...'
    Invoke-Native 'go test' { go test ./... }

    if (-not $SkipSmoke) {
        $binary = if ($onWindows) { 'dist/eylu.exe' } else { 'dist/eylu' }

        Write-Step "go build -trimpath -o $binary ."
        New-Item -ItemType Directory -Force -Path (Join-Path $root 'dist') | Out-Null
        Invoke-Native 'go build' { go build -trimpath -o $binary . }

        Write-Step 'scripts/smoke.ps1'
        Invoke-Native 'scripts/smoke.ps1' { & $hostExe -NoProfile -File scripts/smoke.ps1 -Binary "./$binary" }

        $bash = $null
        if ($env:ProgramFiles) {
            $candidate = Join-Path $env:ProgramFiles 'Git\bin\bash.exe'
            if (Test-Path -LiteralPath $candidate) { $bash = $candidate }
        }
        if (-not $bash) {
            $found = Get-Command bash -ErrorAction SilentlyContinue
            if ($found) { $bash = $found.Source }
        }
        if ($bash) {
            Write-Step 'scripts/smoke.sh'
            Invoke-Native 'scripts/smoke.sh' { & $bash scripts/smoke.sh "./$binary" }
        }
    }

    $skipped = @()
    if ($SkipSmoke) { $skipped += '-SkipSmoke' }
    if ($SkipStatic) { $skipped += '-SkipStatic' }
    if ($SkipExtras) { $skipped += '-SkipExtras' }
    Write-Host ''
    if ($skipped.Count -gt 0) {
        Write-Host ("OK: every gate passed (skipped: {0})." -f ($skipped -join ' '))
    }
    else {
        Write-Host 'OK: every gate passed.'
    }
    exit 0
}
catch {
    Write-Host ''
    Write-Host "FAIL: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}
finally {
    if (Test-Path -LiteralPath $work) {
        Remove-Item -Recurse -Force -LiteralPath $work -ErrorAction SilentlyContinue
    }
}
