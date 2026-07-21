param(
    [Parameter(Mandatory = $true)]
    [string]$Binary
)

$ErrorActionPreference = "Stop"

& $Binary version
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

$rootHelp = (& $Binary --help | Out-String)
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
if (-not $rootHelp.Contains("--resume string")) {
    Write-Error "root help is missing --resume <session-id>"
}

$chatHelp = (& $Binary chat --help | Out-String)
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
if (-not $chatHelp.Contains("--resume string")) {
    Write-Error "chat help is missing --resume <session-id>"
}

foreach ($arguments in @(
    @("sessions", "--help"),
    @("mcp", "--help")
)) {
    & $Binary @arguments | Out-Null
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

foreach ($arguments in @(
    @( "--resume" ),
    @( "--continue" ),
    @( "--session", "smoke", "--resume", "smoke" )
)) {
    $previousErrorActionPreference = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    & $Binary @arguments 2>$null | Out-Null
    $exitCode = $LASTEXITCODE
    $ErrorActionPreference = $previousErrorActionPreference
    if ($exitCode -eq 0) {
        Write-Error "invalid resume invocation unexpectedly succeeded: $arguments"
    }
}

exit 0
