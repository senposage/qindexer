[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$InstallDir,
  [Parameter(Mandatory = $true)][string]$DataDir,
  [string]$DatabaseName = "qindexer",
  [string]$RoleName = "qindexer",
  [int]$Port = 0,
  [switch]$ResetIncomplete,
  [string]$ProgressPath = ""
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "install-progress-windows.ps1")

function Report-Progress {
  param(
    [string]$Status,
    [string]$Stage,
    [int]$Percent,
    [string]$Message,
    [long]$BytesDownloaded = 0,
    [long]$BytesTotal = 0,
    [long]$BytesPerSecond = 0
  )
  Write-QIndexerInstallProgress -ProgressPath $ProgressPath -Status $Status -Stage $Stage -Percent $Percent -Message $Message -BytesDownloaded $BytesDownloaded -BytesTotal $BytesTotal -BytesPerSecond $BytesPerSecond
}

trap {
  Report-Progress "failed" "postgres" 100 $_.Exception.Message
  throw $_
}

Report-Progress "running" "postgres_prepare" 5 "Preparing private PostgreSQL setup."

function New-Secret {
  param([int]$Length = 32)
  $alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
  $bytes = New-Object byte[] ($Length * 2)
  $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
  try {
    $rng.GetBytes($bytes)
  } finally {
    $rng.Dispose()
  }
  $out = New-Object Text.StringBuilder
  foreach ($byte in $bytes) {
    if ($out.Length -ge $Length) { break }
    [void]$out.Append($alphabet[$byte % $alphabet.Length])
  }
  return $out.ToString()
}

function Test-PortFree {
  param([int]$Candidate)
  $listener = $null
  try {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $Candidate)
    $listener.Start()
    return $true
  } catch {
    return $false
  } finally {
    if ($null -ne $listener) { $listener.Stop() }
  }
}

function Get-PostgresScalar {
  param(
    [Parameter(Mandatory = $true)][string]$Executable,
    [Parameter(Mandatory = $true)][int]$ServerPort,
    [Parameter(Mandatory = $true)][string]$Sql,
    [Parameter(Mandatory = $true)][string]$Operation
  )

  $output = @(& $Executable -h 127.0.0.1 -p $ServerPort -U postgres -d postgres -tAc $Sql 2>&1)
  if ($LASTEXITCODE -ne 0) {
    $detail = (($output | ForEach-Object { [string]$_ }) -join [Environment]::NewLine).Trim()
    if ($detail) { throw "Could not $Operation. $detail" }
    throw "Could not $Operation."
  }
  return (($output | ForEach-Object { [string]$_ }) -join [Environment]::NewLine).Trim()
}

function Format-ByteSize {
  param([long]$Bytes)
  if ($Bytes -ge 1GB) { return ("{0:N1} GB" -f ($Bytes / 1GB)) }
  if ($Bytes -ge 1MB) { return ("{0:N1} MB" -f ($Bytes / 1MB)) }
  if ($Bytes -ge 1KB) { return ("{0:N1} KB" -f ($Bytes / 1KB)) }
  return "$Bytes bytes"
}

function Download-PostgresInstaller {
  param(
    [Parameter(Mandatory = $true)][string]$Uri,
    [Parameter(Mandatory = $true)][string]$Destination
  )

  $request = [Net.HttpWebRequest]::Create($Uri)
  $request.AllowAutoRedirect = $true
  $request.UserAgent = "QIndexer PostgreSQL setup"
  $request.Timeout = 30000
  $request.ReadWriteTimeout = 300000
  $response = $null
  $input = $null
  $output = $null
  try {
    $response = $request.GetResponse()
    $total = [int64]$response.ContentLength
    $input = $response.GetResponseStream()
    $output = [IO.File]::Open($Destination, [IO.FileMode]::Create, [IO.FileAccess]::Write, [IO.FileShare]::None)
    $buffer = New-Object byte[] (1MB)
    $downloaded = [int64]0
    $startedAt = [DateTime]::UtcNow
    $lastReportedAt = $startedAt.AddSeconds(-1)
    while (($read = $input.Read($buffer, 0, $buffer.Length)) -gt 0) {
      $output.Write($buffer, 0, $read)
      $downloaded += $read
      $now = [DateTime]::UtcNow
      if (($now - $lastReportedAt).TotalSeconds -lt 1) { continue }
      $elapsed = [Math]::Max(0.001, ($now - $startedAt).TotalSeconds)
      $rate = [int64]($downloaded / $elapsed)
      if ($total -gt 0) {
        $ratio = [Math]::Min(1.0, $downloaded / [double]$total)
        $percent = 25 + [int][Math]::Floor(29 * $ratio)
        $message = "Downloading PostgreSQL: $(Format-ByteSize $downloaded) of $(Format-ByteSize $total) ($([int][Math]::Floor(100 * $ratio))%) at $(Format-ByteSize $rate)/s."
      } else {
        $percent = 25
        $message = "Downloading PostgreSQL: $(Format-ByteSize $downloaded) at $(Format-ByteSize $rate)/s."
      }
      Report-Progress "running" "postgres_download" $percent $message $downloaded $total $rate
      $lastReportedAt = $now
    }
    $elapsed = [Math]::Max(0.001, ([DateTime]::UtcNow - $startedAt).TotalSeconds)
    $rate = [int64]($downloaded / $elapsed)
    $message = "Downloaded PostgreSQL: $(Format-ByteSize $downloaded) at $(Format-ByteSize $rate)/s."
    Report-Progress "running" "postgres_download" 54 $message $downloaded $total $rate
  } finally {
    if ($output) { $output.Dispose() }
    if ($input) { $input.Dispose() }
    if ($response) { $response.Dispose() }
  }
}

function Select-Port {
  param([int]$Preferred)
  if ($Preferred -gt 0) {
    if (-not (Test-PortFree $Preferred)) { throw "PostgreSQL port $Preferred is already in use." }
    return $Preferred
  }
  foreach ($candidate in 5432..5442) {
    if (Test-PortFree $candidate) { return $candidate }
  }
  throw "No free PostgreSQL port was found between 5432 and 5442."
}

function Stop-PrivatePostgres {
  param(
    [Parameter(Mandatory = $true)][string]$ServiceName,
    [Parameter(Mandatory = $true)][string]$PostgresRoot
  )

  $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if ($service -and $service.Status -ne "Stopped") {
    Stop-Service -Name $ServiceName -Force
  }
  for ($attempt = 0; $attempt -lt 60; $attempt++) {
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $service -or $service.Status -eq "Stopped") { break }
    Start-Sleep -Milliseconds 500
  }
  if ($service -and $service.Status -ne "Stopped") {
    throw "QIndexer's private PostgreSQL service did not stop within 30 seconds."
  }

  $rootPrefix = ([IO.Path]::GetFullPath($PostgresRoot)).TrimEnd('\') + '\'
  for ($attempt = 0; $attempt -lt 20; $attempt++) {
    $processes = @(Get-CimInstance Win32_Process -Filter "Name = 'postgres.exe'" -ErrorAction Stop | Where-Object {
      $_.ExecutablePath -and ([IO.Path]::GetFullPath($_.ExecutablePath)).StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)
    })
    if ($processes.Count -eq 0) { break }
    foreach ($process in $processes) {
      Stop-Process -Id $process.ProcessId -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Milliseconds 500
  }
  $remaining = @(Get-CimInstance Win32_Process -Filter "Name = 'postgres.exe'" -ErrorAction Stop | Where-Object {
    $_.ExecutablePath -and ([IO.Path]::GetFullPath($_.ExecutablePath)).StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)
  })
  if ($remaining.Count -gt 0) {
    throw "QIndexer's private PostgreSQL processes did not exit. Refusing to touch unrelated PostgreSQL services."
  }
  if ($service) {
    sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 1
  }
}

function Remove-DirectoryWithRetry {
  param([Parameter(Mandatory = $true)][string]$Path)
  for ($attempt = 0; $attempt -lt 10; $attempt++) {
    if (-not (Test-Path -LiteralPath $Path)) { return }
    try {
      Remove-Item -LiteralPath $Path -Recurse -Force -ErrorAction Stop
      return
    } catch {
      if ($attempt -eq 9) { throw }
      Start-Sleep -Seconds 1
    }
  }
}

if ($DatabaseName -notmatch '^[A-Za-z_][A-Za-z0-9_]*$' -or $RoleName -notmatch '^[A-Za-z_][A-Za-z0-9_]*$') {
  throw "Database and role names must contain only letters, digits, and underscores."
}

$postgresRoot = Join-Path $InstallDir "postgresql"
$clusterDir = Join-Path $DataDir "postgresql"
$serviceName = "qindexer-postgresql"
$psql = Join-Path $postgresRoot "bin\psql.exe"
$createdb = Join-Path $postgresRoot "bin\createdb.exe"
$installerVersion = "18.6-1"
$installerCacheDir = Join-Path $InstallDir "cache"
$installer = Join-Path $installerCacheDir "postgresql-$installerVersion-windows-x64.exe"
$superPassword = New-Secret
$appPassword = New-Secret

if ($ResetIncomplete -and (Test-Path -LiteralPath $psql)) {
  Report-Progress "running" "postgres_recover" 15 "Resetting an incomplete private PostgreSQL setup."
  Write-Host "Resetting incomplete private PostgreSQL setup for QIndexer..."
  Stop-PrivatePostgres -ServiceName $serviceName -PostgresRoot $postgresRoot
  Remove-DirectoryWithRetry -Path $postgresRoot
  Remove-DirectoryWithRetry -Path $clusterDir
}

if (Test-Path -LiteralPath $psql) {
  $selectedPort = $Port
  if ($selectedPort -le 0) {
    $configFile = Join-Path $clusterDir "postgresql.conf"
    $configuredPort = ""
    if (Test-Path -LiteralPath $configFile) {
      $configuredPort = (Select-String -LiteralPath $configFile -Pattern '^\s*port\s*=\s*(\d+)' | Select-Object -First 1).Matches.Groups[1].Value
    }
    $selectedPort = if ($configuredPort) { [int]$configuredPort } else { 5432 }
  }
} else {
  $selectedPort = Select-Port $Port
}

Report-Progress "running" "postgres_discover" 15 "Checking the private PostgreSQL installation."

if (-not (Test-Path -LiteralPath $psql)) {
  New-Item -ItemType Directory -Force -Path $installerCacheDir | Out-Null
  $useCachedInstaller = (Test-Path -LiteralPath $installer) -and ((Get-Item -LiteralPath $installer).Length -ge 50MB)
  if (-not $useCachedInstaller) {
    if (Test-Path -LiteralPath $installer) { Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue }
    Report-Progress "running" "postgres_download" 25 "Connecting to the PostgreSQL download."
    Write-Host "Downloading PostgreSQL 18.6 from the official PostgreSQL Windows distributor..."
    Download-PostgresInstaller -Uri "https://get.enterprisedb.com/postgresql/postgresql-18.6-1-windows-x64.exe" -Destination $installer
  } else {
    $cachedBytes = (Get-Item -LiteralPath $installer).Length
    Report-Progress "running" "postgres_download" 54 "Using cached PostgreSQL $installerVersion installer ($(Format-ByteSize $cachedBytes))." $cachedBytes $cachedBytes 0
    Write-Host "Using cached PostgreSQL $installerVersion installer."
  }
  if ((Get-Item -LiteralPath $installer).Length -lt 50MB) { throw "The PostgreSQL installer download is unexpectedly small." }
  New-Item -ItemType Directory -Force -Path $clusterDir | Out-Null
  Report-Progress "running" "postgres_install" 55 "Installing private PostgreSQL."
  Write-Host "Installing private PostgreSQL server for QIndexer..."
  $arguments = @(
    "--mode", "unattended",
    "--unattendedmodeui", "none",
    "--prefix", $postgresRoot,
    "--datadir", $clusterDir,
    "--serverport", $selectedPort,
    "--servicename", $serviceName,
    "--superpassword", $superPassword,
    "--enable-components", "server,commandlinetools",
    "--disable-components", "pgAdmin,stackbuilder"
  )
  $process = Start-Process -FilePath $installer -ArgumentList $arguments -Wait -PassThru
  if ($process.ExitCode -ne 0) { throw "PostgreSQL installer failed with exit code $($process.ExitCode)." }
}

if (-not (Test-Path -LiteralPath $psql) -or -not (Test-Path -LiteralPath $createdb)) {
  throw "PostgreSQL installation completed but its command-line tools were not found."
}
Report-Progress "running" "postgres_start" 70 "Starting private PostgreSQL."
$service = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
if (-not $service) { throw "PostgreSQL installation completed but service $serviceName was not found." }
if ($service.Status -ne "Running") { Start-Service -Name $serviceName }
$service.WaitForStatus("Running", [TimeSpan]::FromSeconds(30))

$env:PGPASSWORD = $superPassword
try {
  Report-Progress "running" "postgres_configure" 85 "Creating the QIndexer database and login."
  $roleExists = Get-PostgresScalar -Executable $psql -ServerPort $selectedPort -Sql "SELECT 1 FROM pg_roles WHERE rolname = '$RoleName'" -Operation "check the QIndexer PostgreSQL role"
  if ($roleExists -ne "1") {
    & $psql -h 127.0.0.1 -p $selectedPort -U postgres -d postgres -v ON_ERROR_STOP=1 -c "CREATE ROLE $RoleName LOGIN PASSWORD '$appPassword'" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not create the QIndexer PostgreSQL role." }
  } else {
    & $psql -h 127.0.0.1 -p $selectedPort -U postgres -d postgres -v ON_ERROR_STOP=1 -c "ALTER ROLE $RoleName PASSWORD '$appPassword'" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not update the QIndexer PostgreSQL role." }
  }
  $databaseExists = Get-PostgresScalar -Executable $psql -ServerPort $selectedPort -Sql "SELECT 1 FROM pg_database WHERE datname = '$DatabaseName'" -Operation "check the QIndexer PostgreSQL database"
  if ($databaseExists -ne "1") {
    & $createdb -h 127.0.0.1 -p $selectedPort -U postgres -O $RoleName $DatabaseName | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not create the QIndexer PostgreSQL database." }
  }
} finally {
  Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
}

Report-Progress "completed" "postgres_ready" 100 "Private PostgreSQL is ready for QIndexer."

[pscustomobject]@{
  driver = "postgres"
  dsn = "postgres://$RoleName`:$appPassword@127.0.0.1`:$selectedPort/$DatabaseName?sslmode=disable"
  host = "127.0.0.1"
  port = $selectedPort
  database = $DatabaseName
  service = $serviceName
} | ConvertTo-Json -Compress
