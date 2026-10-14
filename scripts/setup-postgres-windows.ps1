[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$InstallDir,
  [Parameter(Mandatory = $true)][string]$DataDir,
  [string]$DatabaseName = "qindexer",
  [string]$RoleName = "qindexer",
  [int]$Port = 0
)

$ErrorActionPreference = "Stop"

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
  $listener = New-Object Net.Sockets.TcpListener([Net.IPAddress]::Loopback, $Candidate)
  try {
    $listener.Start()
    return $true
  } catch {
    return $false
  } finally {
    $listener.Stop()
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

if (-not (Test-Path -LiteralPath $psql)) {
  try {
    Write-Host "Downloading PostgreSQL 18.6 from the official PostgreSQL Windows distributor..."
    Invoke-WebRequest -Uri "https://get.enterprisedb.com/postgresql/postgresql-18.6-1-windows-x64.exe" -OutFile $installer -UseBasicParsing
    if ((Get-Item -LiteralPath $installer).Length -lt 50MB) { throw "The PostgreSQL installer download is unexpectedly small." }
    New-Item -ItemType Directory -Force -Path $clusterDir | Out-Null
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
$service = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
if (-not $service) { throw "PostgreSQL installation completed but service $serviceName was not found." }
if ($service.Status -ne "Running") { Start-Service -Name $serviceName }
$service.WaitForStatus("Running", [TimeSpan]::FromSeconds(30))

$env:PGPASSWORD = $superPassword
try {
  $roleExists = (& $psql -h 127.0.0.1 -p $selectedPort -U postgres -d postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname = '$RoleName'" 2>$null).Trim()
  if ($roleExists -ne "1") {
    & $psql -h 127.0.0.1 -p $selectedPort -U postgres -d postgres -v ON_ERROR_STOP=1 -c "CREATE ROLE $RoleName LOGIN PASSWORD '$appPassword'" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not create the QIndexer PostgreSQL role." }
  } else {
    & $psql -h 127.0.0.1 -p $selectedPort -U postgres -d postgres -v ON_ERROR_STOP=1 -c "ALTER ROLE $RoleName PASSWORD '$appPassword'" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not update the QIndexer PostgreSQL role." }
  }
  $databaseExists = (& $psql -h 127.0.0.1 -p $selectedPort -U postgres -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$DatabaseName'" 2>$null).Trim()
  if ($databaseExists -ne "1") {
    & $createdb -h 127.0.0.1 -p $selectedPort -U postgres -O $RoleName $DatabaseName | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not create the QIndexer PostgreSQL database." }
  }
} finally {
  Remove-Item Env:PGPASSWORD -ErrorAction SilentlyContinue
}

[pscustomobject]@{
  driver = "postgres"
  dsn = "postgres://$RoleName`:$appPassword@127.0.0.1`:$selectedPort/$DatabaseName?sslmode=disable"
  host = "127.0.0.1"
  port = $selectedPort
  database = $DatabaseName
  service = $serviceName
} | ConvertTo-Json -Compress
