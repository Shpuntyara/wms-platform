param(
    [Parameter(Mandatory=$true)][string]$PgBin,
    [Parameter(Mandatory=$true)][string]$GoExe,
    [Parameter(Mandatory=$true)][string]$PythonExe,
    [string]$Report = 'work/go-backend-verification.json'
)
$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$testRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot ('work/integration-' + [guid]::NewGuid().ToString('N'))))
$allowedRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot 'work')) + [IO.Path]::DirectorySeparatorChar
if (-not $testRoot.StartsWith($allowedRoot, [StringComparison]::OrdinalIgnoreCase)) { throw 'Invalid isolated test path' }
New-Item -ItemType Directory -Path $testRoot -Force | Out-Null
$dataPath = Join-Path $testRoot 'pgdata'
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$listener.Start()
$testPort = $listener.LocalEndpoint.Port
$listener.Stop()
$psql = Join-Path $PgBin 'psql.exe'
$ctl = Join-Path $PgBin 'pg_ctl.exe'
$started = $false
$priorAdmin = $env:WMS_TEST_ADMIN_DSN
$priorRuntime = $env:WMS_TEST_RUNTIME_DSN

function Invoke-Checked([string]$Program, [string[]]$Arguments) {
    & $Program @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Test command failed with exit code $LASTEXITCODE" }
}

Push-Location $repoRoot
try {
    Write-Host 'Creating an isolated local PostgreSQL cluster'
    Invoke-Checked (Join-Path $PgBin 'initdb.exe') @('-D',$dataPath,'-U','wms_test_admin','-A','trust','--encoding=UTF8','--locale=C')
    Invoke-Checked $ctl @('-D',$dataPath,'-l',(Join-Path $testRoot 'postgres.log'),'-o',"-h 127.0.0.1 -p $testPort",'-w','start')
    $started = $true
    $connection = @('-X','-q','-h','127.0.0.1','-p',"$testPort",'-U','wms_test_admin','-v','ON_ERROR_STOP=1')
    $roles = 'CREATE ROLE wms_test_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE; CREATE ROLE wms_test_runtime NOLOGIN; CREATE ROLE wms_test_module NOLOGIN; CREATE ROLE wms_go_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE IN ROLE wms_test_runtime'
    Invoke-Checked $psql ($connection + @('-d','postgres','-c',$roles))
    Invoke-Checked (Join-Path $PgBin 'createdb.exe') @('-h','127.0.0.1','-p',"$testPort",'-U','wms_test_admin','-O','wms_test_owner','wms_kernel_test_backend')
    $migrations = @('-d','wms_kernel_test_backend','-v','runtime_role=wms_test_runtime','-v','module_role=wms_test_module','-c','SET ROLE wms_test_owner')
    foreach ($file in @('db/migrations/0001_inventory_schema.sql','db/migrations/0002_inventory_commands.sql','db/migrations/0003_backend_api.sql','db/bootstrap/grants.psql','db/bootstrap/backend-grants.psql','modules/dispatch-quality/migrations/001_create_checks.sql','modules/dispatch-quality/migrations/002_document_facts.sql','db/bootstrap/dispatch-quality-grants.psql')) {
        $migrations += @('-f',$file)
    }
    Invoke-Checked $psql ($connection + $migrations)
    $env:WMS_TEST_ADMIN_DSN = "postgres://wms_test_admin@127.0.0.1:$testPort/wms_kernel_test_backend?sslmode=disable"
    $env:WMS_TEST_RUNTIME_DSN = "postgres://wms_go_runtime@127.0.0.1:$testPort/wms_kernel_test_backend?sslmode=disable"
    Write-Host 'Running vet and Go unit/integration tests with race detection'
    Invoke-Checked $GoExe @('vet','-tags=integration','./...')
    $events = Join-Path $testRoot 'go-tests.jsonl'
    & $GoExe test -race -tags=integration ./... -count=1 -timeout=120s -json | Set-Content -LiteralPath $events -Encoding utf8
    if ($LASTEXITCODE -ne 0) { throw "Go tests failed; inspect $events" }
    Write-Host 'Running existing SQL-kernel regression scenarios'
    $legacy = Join-Path $testRoot 'postgres-kernel.json'
    & $PythonExe tests/postgres_kernel.py --psql $psql --port "$testPort" --database wms_kernel_test_backend --admin wms_test_admin --report $legacy | Set-Content -LiteralPath (Join-Path $testRoot 'sql-tests.log') -Encoding utf8
    if ($LASTEXITCODE -ne 0) { throw "SQL kernel tests failed; inspect $legacy" }
    $pgVersion = & $psql @connection -d wms_kernel_test_backend -At -c 'SHOW server_version'
    $goVersion = & $GoExe version
    Invoke-Checked $PythonExe @('scripts/summarize-tests.py','--events',$events,'--legacy',$legacy,'--output',$Report,'--go',"$goVersion",'--postgres',"$pgVersion")
} finally {
    if ($started) { & $ctl -D $dataPath -m fast -w stop; if ($LASTEXITCODE -ne 0) { Write-Warning "Could not stop isolated PostgreSQL at $dataPath" } }
    $env:WMS_TEST_ADMIN_DSN = $priorAdmin
    $env:WMS_TEST_RUNTIME_DSN = $priorRuntime
    Pop-Location
}
