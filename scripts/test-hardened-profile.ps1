param(
    [Parameter(Mandatory=$true)][string]$PgBin,
    [Parameter(Mandatory=$true)][string]$GoExe,
    [string]$PythonExe = 'python',
    [switch]$IncludeMutations
)
$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$testRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot ('work/hardened-profile-' + [guid]::NewGuid().ToString('N'))))
$allowed = [IO.Path]::GetFullPath((Join-Path $repoRoot 'work')) + [IO.Path]::DirectorySeparatorChar
if (-not $testRoot.StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase)) { throw 'Invalid isolated test path' }
New-Item -ItemType Directory -Path $testRoot -Force | Out-Null
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0)
$listener.Start()
$testPort = $listener.LocalEndpoint.Port
$listener.Stop()
$psql = Join-Path $PgBin 'psql.exe'
$ctl = Join-Path $PgBin 'pg_ctl.exe'
$data = Join-Path $testRoot 'pgdata'
$started = $false
Push-Location $repoRoot
try {
    & (Join-Path $PgBin 'initdb.exe') -D $data -U arena_admin -A trust --data-checksums --encoding=UTF8 --locale=C
    if ($LASTEXITCODE -ne 0) { throw 'initdb failed' }
    & $ctl -D $data -l (Join-Path $testRoot 'postgres.log') -o "-h 127.0.0.1 -p $testPort" -w start
    if ($LASTEXITCODE -ne 0) { throw 'PostgreSQL start failed' }
    $started = $true
    $reportRoot = 'docs/verification/onprem'
    New-Item -ItemType Directory -Path $reportRoot -Force | Out-Null
    # IncludeMutations is retained for CLI compatibility; all runs prove every kill.
    & $PythonExe scripts/hardened_gate.py --psql $psql --port $testPort --go $GoExe --output "$reportRoot/portable-current"
    if ($LASTEXITCODE -ne 0) { throw 'Hardened profile gate failed' }
    Write-Host 'On-premise profile: baseline behavioral RED, SQL/HTTP/race GREEN, requested mutants killed.'
} finally {
    if ($started) {
        & $ctl -D $data -m fast -w stop
        if ($LASTEXITCODE -eq 0) {
            $resolvedData = [IO.Path]::GetFullPath($data)
            if (-not $resolvedData.StartsWith($allowed,[StringComparison]::OrdinalIgnoreCase) -or [IO.Path]::GetFileName($resolvedData) -ne 'pgdata') { throw 'Refuse cleanup outside disposable work cluster' }
            # Test-only keys existed in process memory and this ephemeral DB.
            # Remove the stopped data directory rather than retain key material.
            Remove-Item -LiteralPath $resolvedData -Recurse -Force
        }
    }
    Pop-Location
}
