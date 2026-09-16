# Self-test for scripts/verify.ps1.
#
# The formatting phase is the delicate part of the gate: it must tell a CRLF
# checkout artifact (harmless, must pass) apart from real uncommitted formatting
# damage (must fail), and it must still judge committed content the way CI does.
# The cases below pin all of that down, plus the staticcheck phase.
#
# Every case builds a throwaway git repository, so the Eylu working tree is
# never touched.
#
#   pwsh -NoProfile -File scripts/verify_selftest.ps1

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$here = $PSScriptRoot
$repoRoot = Split-Path -Parent $here
$hostExe = (Get-Process -Id $PID).Path

$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$scratch = Join-Path ([System.IO.Path]::GetTempPath()) ('eylu-verify-selftest-' + [System.Guid]::NewGuid().ToString('N'))
$repo = Join-Path $scratch 'repo'
$stdoutFile = Join-Path $scratch 'stdout.txt'
$stderrFile = Join-Path $scratch 'stderr.txt'

$script:Passed = 0
$script:Failed = 0
$script:VerifyExit = 0
$script:VerifyOutput = ''

function Write-Ok([string]$Name) {
    Write-Host "ok   - $Name"
    $script:Passed++
}

function Write-No([string]$Name) {
    Write-Host "FAIL - $Name"
    $script:Failed++
}

function Show-Output {
    foreach ($line in ($script:VerifyOutput -split "`r?`n")) {
        Write-Host "    | $line"
    }
}

function Reset-Repo {
    if (Test-Path -LiteralPath $repo) {
        Remove-Item -Recurse -Force -LiteralPath $repo
    }
    New-Item -ItemType Directory -Force -Path (Join-Path $repo 'scripts') | Out-Null
    New-Item -ItemType Directory -Force -Path (Join-Path $repo '.github\workflows') | Out-Null
    Copy-Item -LiteralPath (Join-Path $here 'verify.ps1') -Destination (Join-Path $repo 'scripts\verify.ps1')
    Copy-Item -LiteralPath (Join-Path $repoRoot '.github\workflows\ci.yml') -Destination (Join-Path $repo '.github\workflows\ci.yml')
    [System.IO.File]::WriteAllText((Join-Path $repo 'go.mod'), "module selftest`n`ngo 1.25.8`n", $utf8NoBom)
    [System.IO.File]::WriteAllText((Join-Path $repo 'main.go'), "package main`n`nfunc main() {}`n", $utf8NoBom)

    Push-Location $repo
    try {
        git init -q . | Out-Null
        git config core.autocrlf false
        git config user.email selftest@example.invalid
        git config user.name selftest
        git add -A
        git commit -qm 'init' | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'could not seed the scratch repository' }
    }
    finally {
        Pop-Location
    }
}

# Rewrites every Go file with CRLF line endings without changing anything else,
# which is what a core.autocrlf=true checkout produces.
function Set-CrlfEverywhere {
    $files = Get-ChildItem -Recurse -File -Path $repo -Filter '*.go' |
        Where-Object { $_.FullName -notmatch '[\\/]\.git[\\/]' }
    foreach ($file in $files) {
        $text = [System.Text.Encoding]::UTF8.GetString([System.IO.File]::ReadAllBytes($file.FullName))
        $text = $text.Replace("`r`n", "`n").Replace("`n", "`r`n")
        [System.IO.File]::WriteAllText($file.FullName, $text, $utf8NoBom)
    }
}

function Invoke-Verify([string[]]$Arguments) {
    foreach ($file in @($stdoutFile, $stderrFile)) {
        if (Test-Path -LiteralPath $file) { Remove-Item -Force -LiteralPath $file }
    }
    $all = @('-NoProfile', '-File', 'scripts/verify.ps1') + $Arguments
    $process = Start-Process -FilePath $hostExe -ArgumentList $all -WorkingDirectory $repo `
        -NoNewWindow -Wait -PassThru -RedirectStandardOutput $stdoutFile -RedirectStandardError $stderrFile
    $script:VerifyExit = $process.ExitCode
    $script:VerifyOutput = [System.IO.File]::ReadAllText($stdoutFile) + [System.IO.File]::ReadAllText($stderrFile)
}

function Assert-Exit([string]$Name, [int]$Expected) {
    if ($script:VerifyExit -eq $Expected) {
        Write-Ok $Name
    }
    else {
        Write-No "$Name (exit $($script:VerifyExit), want $Expected)"
        Show-Output
    }
}

function Assert-Contains([string]$Name, [string]$Needle) {
    if ($script:VerifyOutput -like "*$Needle*") {
        Write-Ok $Name
    }
    else {
        Write-No "$Name (output does not contain: $Needle)"
        Show-Output
    }
}

try {
    New-Item -ItemType Directory -Force -Path $scratch | Out-Null

    Write-Host ''
    Write-Host '== 1. clean tree passes'
    Reset-Repo
    Invoke-Verify @('-SkipSmoke', '-SkipStatic', '-SkipExtras')
    Assert-Exit 'clean tree exits 0' 0

    Write-Host ''
    Write-Host '== 2. CRLF checkout artifact is not a formatting error'
    Reset-Repo
    Set-CrlfEverywhere
    $naive = @(gofmt -l "$repo" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    if ($naive.Count -eq 0) {
        Write-No 'fixture does not reproduce the CRLF false positive (plain gofmt -l reports nothing)'
    }
    else {
        Write-Ok 'fixture reproduces the CRLF false positive of plain gofmt -l'
    }
    Invoke-Verify @('-SkipSmoke', '-SkipStatic', '-SkipExtras')
    Assert-Exit 'CRLF working tree still exits 0' 0

    Write-Host ''
    Write-Host '== 3. formatting error inside a CRLF working tree is reported'
    Reset-Repo
    [System.IO.File]::WriteAllText((Join-Path $repo 'main.go'), "package main`n`nfunc  main() {}`n", $utf8NoBom)
    Set-CrlfEverywhere
    Invoke-Verify @('-SkipSmoke', '-SkipStatic', '-SkipExtras')
    Assert-Exit 'unformatted working tree exits non-zero' 1
    Assert-Contains 'the unformatted file is named' 'main.go'

    Write-Host ''
    Write-Host '== 4. committed CRLF content is reported the way CI reports it'
    Reset-Repo
    Set-CrlfEverywhere
    Push-Location $repo
    try {
        git add -A
        git commit -qm 'crlf' | Out-Null
    }
    finally {
        Pop-Location
    }
    Invoke-Verify @('-SkipSmoke', '-SkipStatic', '-SkipExtras')
    Assert-Exit 'committed CRLF exits non-zero' 1
    Assert-Contains 'the failure comes from the committed content' 'committed content'

    Write-Host ''
    Write-Host '== 5. staticcheck-only failure is reported'
    Reset-Repo
    $deadStore = "package main`n`nfunc value() int { return 1 }`n`nfunc main() {`n`tx := value()`n`tx = 2`n`t_ = x`n}`n"
    [System.IO.File]::WriteAllText((Join-Path $repo 'main.go'), $deadStore, $utf8NoBom)
    gofmt -w (Join-Path $repo 'main.go')
    Push-Location $repo
    try {
        git add -A
        git commit -qm 'dead assignment' | Out-Null
    }
    finally {
        Pop-Location
    }
    Invoke-Verify @('-SkipSmoke', '-SkipExtras')
    Assert-Exit 'dead assignment exits non-zero' 1
    Assert-Contains 'the staticcheck reason is reported' 'SA4006'

    Write-Host ''
    Write-Host '----------------------------------------'
    Write-Host ("{0} passed, {1} failed" -f $script:Passed, $script:Failed)
    if ($script:Failed -ne 0) { exit 1 }
    exit 0
}
finally {
    if (Test-Path -LiteralPath $scratch) {
        Remove-Item -Recurse -Force -LiteralPath $scratch -ErrorAction SilentlyContinue
    }
}
