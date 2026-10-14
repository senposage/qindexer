[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$InstallDir,
  [Parameter(Mandatory = $true)][string]$DataDir,
  [string]$DatabaseName = "qindexer",
  [string]$RoleName = "qindexer",
  [int]$Port = 0,
  [string]$ProgressPath = ""
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "install-progress-windows.ps1")

function Report-Progress {
  param([string]$Status, [string]$Stage, [int]$Percent, [string]$Message)
  Write-QIndexerInstallProgress -ProgressPath $ProgressPath -Status $Status -Stage $Stage -Percent $Percent -Message $Message
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

if ($DatabaseName -notmatch '^[A-Za-z_][A-Za-z0-9_]*$' -or $RoleName -notmatch '^[A-Za-z_][A-Za-z0-9_]*$') {
  throw "Database and role names must contain only letters, digits, and underscores."
}

$postgresRoot = Join-Path $InstallDir "postgresql"
$clusterDir = Join-Path $DataDir "postgresql"
$serviceName = "qindexer-postgresql"
$psql = Join-Path $postgresRoot "bin\psql.exe"
$createdb = Join-Path $postgresRoot "bin\createdb.exe"
$installer = Join-Path $env:TEMP "qindexer-postgresql-18.6-windows-x64.exe"
$superPassword = New-Secret
$appPassword = New-Secret

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
  try {
    Report-Progress "running" "postgres_download" 25 "Downloading PostgreSQL."
    Write-Host "Downloading PostgreSQL 18.6 from the official PostgreSQL Windows distributor..."
    Invoke-WebRequest -Uri "https://get.enterprisedb.com/postgresql/postgresql-18.6-1-windows-x64.exe" -OutFile $installer -UseBasicParsing
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
  } finally {
    if (Test-Path -LiteralPath $installer) { Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue }
  }
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
