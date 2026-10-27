param(
    [Parameter(Mandatory=$true)][ValidateSet('Setup','Measure','Stop')][string]$Mode,
    [Parameter(Mandatory=$true)][string]$PgBin,
    [string]$GoExe = 'go',
    [string]$Session = 'work/load-session.json',
    [ValidatePattern('^[a-z][a-z0-9_-]*$')][string]$Label = 'current',
    [string]$BaselineRef,
    [switch]$OptimizeSchema,
    [ValidateRange(1000,2000000)][int]$Movements = 1000000,
    [ValidateRange(1000,500000)][int]$Reservations = 250000,
    [ValidateRange(200,200000)][int]$Balances = 100000,
    [ValidateRange(1,20)][int]$Samples = 10,
    [ValidateRange(1,100)][int]$RequestsPerSample = 20,
    [ValidateRange(1,30)][int]$Seconds = 5,
    [ValidateRange(1,10)][int]$Repetitions = 3
)
$ErrorActionPreference = 'Stop'
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$workRoot = [IO.Path]::GetFullPath((Join-Path $repoRoot 'work')) + [IO.Path]::DirectorySeparatorChar
$sessionPath = [IO.Path]::GetFullPath((Join-Path $repoRoot $Session))
if (-not $sessionPath.StartsWith($workRoot,[StringComparison]::OrdinalIgnoreCase)) { throw 'Session must be inside repository work/' }
$psql = Join-Path $PgBin 'psql.exe'
$ctl = Join-Path $PgBin 'pg_ctl.exe'
function Invoke-Checked([string]$Program,[string[]]$Arguments) {
    & $Program @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Load command failed with exit code $LASTEXITCODE" }
}
function Set-LoadEnvironment($State,[string]$Database) {
    $env:WMS_TEST_ADMIN_DSN = "postgres://wms_test_admin@127.0.0.1:$($State.Port)/${Database}?sslmode=disable"
    $env:WMS_TEST_RUNTIME_DSN = "postgres://wms_go_runtime@127.0.0.1:$($State.Port)/${Database}?sslmode=disable"
    $env:WMS_LOAD_DIR = $State.Root
    $env:WMS_LOAD_LABEL = $Label
    $env:WMS_LOAD_SECONDS = "$Seconds"
    $env:WMS_LOAD_REPETITIONS = "$Repetitions"
}
$environmentKeys = @('WMS_TEST_ADMIN_DSN','WMS_TEST_RUNTIME_DSN','WMS_LOAD_DIR','WMS_LOAD_LABEL','WMS_LOAD_SECONDS','WMS_LOAD_REPETITIONS','WMS_LOAD_MOVEMENTS','WMS_LOAD_RESERVATIONS','WMS_LOAD_BALANCES')
$priorEnvironment = @{}
foreach ($key in $environmentKeys) { $priorEnvironment[$key] = [Environment]::GetEnvironmentVariable($key,'Process') }
Push-Location $repoRoot
try {
    if ($Mode -eq 'Setup') {
        if (Test-Path -LiteralPath $sessionPath) { throw 'Session already exists; use another work/ session path' }
        if ($Balances % 200 -ne 0) { throw 'Balances must be divisible by 200' }
        $loadRoot = Join-Path $repoRoot ('work/load-' + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Path $loadRoot -Force | Out-Null
        $dataPath = Join-Path $loadRoot 'pgdata'
        $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0)
        $listener.Start()
        $loadPort = $listener.LocalEndpoint.Port
        $listener.Stop()
        $state = [pscustomobject]@{Root=$loadRoot; DataPath=$dataPath; Port=$loadPort; SeedDatabase='wms_kernel_test_load_seed'}
        $started = $false
        try {
            Invoke-Checked (Join-Path $PgBin 'initdb.exe') @('-D',$dataPath,'-U','wms_test_admin','-A','trust','--encoding=UTF8','--locale=C')
            Invoke-Checked $ctl @('-D',$dataPath,'-l',(Join-Path $loadRoot 'postgres.log'),'-o',"-h 127.0.0.1 -p $loadPort -c shared_preload_libraries=pg_stat_statements -c pg_stat_statements.track=all -c track_io_timing=on -c shared_buffers=256MB -c max_wal_size=2GB",'-w','start')
            $started = $true
            $connection = @('-X','-q','-h','127.0.0.1','-p',"$loadPort",'-U','wms_test_admin','-v','ON_ERROR_STOP=1')
            Invoke-Checked $psql ($connection + @('-d','postgres','-c','CREATE ROLE wms_test_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE; CREATE ROLE wms_test_runtime NOLOGIN; CREATE ROLE wms_test_module NOLOGIN; CREATE ROLE wms_go_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE IN ROLE wms_test_runtime'))
            Invoke-Checked (Join-Path $PgBin 'createdb.exe') @('-h','127.0.0.1','-p',"$loadPort",'-U','wms_test_admin','-O','wms_test_owner',$state.SeedDatabase)
            Invoke-Checked $psql ($connection + @('-d',$state.SeedDatabase,'-c','CREATE EXTENSION pg_stat_statements'))
            $migrationArgs = @('-d',$state.SeedDatabase,'-v','runtime_role=wms_test_runtime','-v','module_role=wms_test_module','-c','SET ROLE wms_test_owner')
            foreach ($file in @('db/migrations/0001_inventory_schema.sql','db/migrations/0002_inventory_commands.sql','db/migrations/0003_backend_api.sql','db/migrations/0004_module_integrity.sql','db/bootstrap/grants.psql','db/bootstrap/backend-grants.psql')) { $migrationArgs += @('-f',$file) }
            Invoke-Checked $psql ($connection + $migrationArgs)
            Set-LoadEnvironment $state $state.SeedDatabase
            $env:WMS_LOAD_MOVEMENTS = "$Movements"
            $env:WMS_LOAD_RESERVATIONS = "$Reservations"
            $env:WMS_LOAD_BALANCES = "$Balances"
            Write-Host 'Seeding an isolated, durable PostgreSQL history fixture'
            & $GoExe test '-tags=integration,load' ./internal/postgres '-run=^TestLoadSeed$' -count=1 -timeout=30m -v 2>&1 | Tee-Object -FilePath (Join-Path $loadRoot 'seed.log')
            if ($LASTEXITCODE -ne 0) { throw 'Load fixture setup failed' }
            $settings = & $psql @connection -d $state.SeedDatabase -At -c "SELECT jsonb_object_agg(name,jsonb_build_object('value',setting,'unit',unit)) FROM pg_settings WHERE name IN ('server_version','shared_buffers','work_mem','max_connections','fsync','synchronous_commit','full_page_writes','max_wal_size','plan_cache_mode','track_io_timing')"
            $hostInfo = [pscustomobject]@{CPU=(Get-CimInstance Win32_Processor | Select-Object -First 1 -ExpandProperty Name); RAMBytes=(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory; PGSettings=($settings | ConvertFrom-Json)}
            $hostInfo | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $loadRoot 'host.json') -Encoding utf8
            New-Item -ItemType Directory -Path (Split-Path -Parent $sessionPath) -Force | Out-Null
            $state | ConvertTo-Json | Set-Content -LiteralPath $sessionPath -Encoding utf8
            Write-Host "Load session ready: $sessionPath"
        } catch {
            if ($started) { & $ctl -D $dataPath -m fast -w stop }
            throw
        }
        exit
    }
    $state = Get-Content -LiteralPath $sessionPath -Raw | ConvertFrom-Json
    $resolvedData = [IO.Path]::GetFullPath($state.DataPath)
    $resolvedRoot = [IO.Path]::GetFullPath($state.Root)
    if (-not $resolvedData.StartsWith($workRoot,[StringComparison]::OrdinalIgnoreCase) -or -not $resolvedRoot.StartsWith($workRoot,[StringComparison]::OrdinalIgnoreCase)) { throw 'Invalid isolated load-session paths' }
    if ($state.SeedDatabase -ne 'wms_kernel_test_load_seed' -or $state.Port -lt 1 -or $state.Port -gt 65535) { throw 'Invalid isolated load database/port' }
    if ($Mode -eq 'Stop') { Invoke-Checked $ctl @('-D',$resolvedData,'-m','fast','-w','stop'); exit }
    $database = 'wms_kernel_test_load_' + $Label.Replace('-','_')
    $connection = @('-X','-q','-h','127.0.0.1','-p',"$($state.Port)",'-U','wms_test_admin','-v','ON_ERROR_STOP=1')
    $seedBytes = & $psql @connection -d $state.SeedDatabase -At -c 'SELECT pg_database_size(current_database())'
    if ($LASTEXITCODE -ne 0) { throw 'Cannot size the seed database' }
    $availableBytes = [IO.DriveInfo]::new([IO.Path]::GetPathRoot($repoRoot)).AvailableFreeSpace
    if ($availableBytes -lt ([long]$seedBytes + 2GB)) { throw 'Insufficient free disk space for another isolated phase clone' }
    # Each phase starts from the identical immutable seed. No DROP or cleanup
    # of histories is needed to compare mutations fairly.
    Invoke-Checked $psql ($connection + @('-d','postgres','-c',"CREATE DATABASE $database WITH TEMPLATE $($state.SeedDatabase) OWNER wms_test_owner"))
    $phaseRoot = Join-Path $resolvedRoot $Label
    New-Item -ItemType Directory -Path $phaseRoot -Force | Out-Null
    if ($OptimizeSchema) {
        Invoke-Checked $psql ($connection + @('-d',$database,'-c','SET ROLE wms_test_owner','-f','db/migrations/0005_inventory_pagination.sql'))
    }
    Set-LoadEnvironment $state $database
    $overlayArgs = @()
    if ($BaselineRef) {
        $resolvedRef = & git rev-parse --verify "${BaselineRef}^{commit}"
        if ($LASTEXITCODE -ne 0) { throw 'Invalid baseline commit' }
        # The overlay source must not become a stray Go package in ./... .
        $baselinePath = Join-Path $phaseRoot 'baseline-store.txt'
        & git show "${resolvedRef}:internal/postgres/store.go" | Set-Content -LiteralPath $baselinePath -Encoding utf8
        if ($LASTEXITCODE -ne 0) { throw 'Cannot read baseline adapter' }
        $replace = @{}
        $replace[(Join-Path $repoRoot 'internal/postgres/store.go')] = $baselinePath
        $overlayPath = Join-Path $phaseRoot 'overlay.json'
        @{Replace=$replace} | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $overlayPath -Encoding utf8
        $overlayArgs = @('-overlay',$overlayPath)
    }
    Write-Host "Measuring history HTTP pages: $Label"
    & $GoExe test @overlayArgs '-tags=integration,load' ./internal/postgres '-run=^$' '-bench=^BenchmarkHistoryHTTP$' "-benchtime=${RequestsPerSample}x" "-count=$Samples" -timeout=30m -benchmem 2>&1 | Tee-Object -FilePath (Join-Path $phaseRoot 'bench.txt')
    if ($LASTEXITCODE -ne 0) { throw 'History benchmark failed' }
    if ($OptimizeSchema) {
        & $GoExe test @overlayArgs '-tags=integration,load' ./internal/postgres '-run=^Test(LoadPageWorkStaysBounded|PaginationPreservesEveryOwnerScopeShape)$' -count=1 -timeout=5m -v 2>&1 | Tee-Object -FilePath (Join-Path $phaseRoot 'bounded-pages.log')
        if ($LASTEXITCODE -ne 0) { throw 'Scoped pagination work/authorization regression failed' }
    }
    Write-Host "Measuring concurrent API mutations: $Label"
    & $GoExe test @overlayArgs '-tags=integration,load' ./internal/postgres '-run=^TestLoadConcurrent$' -count=1 -timeout=30m -v 2>&1 | Tee-Object -FilePath (Join-Path $phaseRoot 'concurrency.log')
    if ($LASTEXITCODE -ne 0) { throw 'Concurrent load checks failed; inspect concurrency.json' }
    $size = & $psql @connection -d $database -At -c "SELECT jsonb_object_agg(schemaname||'.'||relname,jsonb_build_object('table_bytes',pg_table_size(relid),'index_bytes',pg_indexes_size(relid),'total_bytes',pg_total_relation_size(relid))) FROM pg_stat_user_tables WHERE schemaname IN ('inventory','platform','warehouse')"
    $size | Set-Content -LiteralPath (Join-Path $phaseRoot 'relations.json') -Encoding utf8
    $indexSizes = & $psql @connection -d $database -At -c "SELECT jsonb_object_agg(s.schemaname||'.'||s.indexrelname,jsonb_build_object('bytes',pg_relation_size(s.indexrelid),'valid',i.indisvalid)) FROM pg_stat_user_indexes s JOIN pg_index i ON i.indexrelid=s.indexrelid"
    if ($LASTEXITCODE -ne 0) { throw 'Index size collection failed' }
    $indexSizes | Set-Content -LiteralPath (Join-Path $phaseRoot 'indexes.json') -Encoding utf8
    Write-Host "Load results saved: $phaseRoot"
} finally {
    foreach ($key in $environmentKeys) { [Environment]::SetEnvironmentVariable($key,$priorEnvironment[$key],'Process') }
    Pop-Location
}
