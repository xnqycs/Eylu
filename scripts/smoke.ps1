param(
    [Parameter(Mandatory = $true)]
    [string]$Binary
)

$ErrorActionPreference = "Stop"

& $Binary version
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

foreach ($arguments in @(
    @("--help"),
    @("chat", "--help"),
    @("sessions", "--help"),
    @("mcp", "--help")
)) {
    & $Binary @arguments | Out-Null
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}
