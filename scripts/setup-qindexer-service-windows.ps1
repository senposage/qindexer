[CmdletBinding()]
param(
  [string]$InstallDir = "C:\ProgramData\QIndexer",
  [string]$BinaryPath = "",
  [string]$NssmPath = "",
  [string]$ServiceName = "QIndexer",
  [switch]$SkipDatabase,
  [switch]$InstallSidecars,
  [string]$ProgressPath = ""
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "install-progress-windows.ps1")

function Write-InstallProgress {
  param([string]$Status, [string]$Stage, [int]$Percent, [string]$Message)
  Write-QIndexerInstallProgress -ProgressPath $ProgressPath -Status $Status -Stage $Stage -Percent $Percent -Message $Message
}

trap {
  Write-InstallProgress "failed" "service_install" 100 $_.Exception.Message
  throw $_
}

Write-InstallProgress "running" "service_prepare" 5 "Preparing the QIndexer service installation."

function Require-Administrator {
  $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = New-Object Security.Principal.WindowsPrincipal($identity)
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Run this setup script from an elevated PowerShell session."
  }
}

function Find-Nssm {
  param([string]$PreferredPath)
  $candidates = @(
    $PreferredPath,
    (Get-Command nssm.exe -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Source -ErrorAction SilentlyContinue),
    (Join-Path $PSScriptRoot "nssm.exe"),
    (Join-Path ${env:ProgramFiles} "nssm\win64\nssm.exe"),
    (Join-Path ${env:ProgramFiles} "nssm\nssm.exe")
  ) | Where-Object { $_ -and (Test-Path -LiteralPath $_ -PathType Leaf) }
  return $candidates | Select-Object -First 1
}

function Find-Binary {
  param([string]$PreferredPath)
  $bundleRoot = Split-Path -Parent $PSScriptRoot
  $candidates = @(
    $PreferredPath,
    (Join-Path $bundleRoot "qindexer.exe"),
    (Join-Path $bundleRoot "qindexer_windows_amd64.exe"),
    (Join-Path $bundleRoot "src\dist\qindexer_windows_amd64.exe")
  ) | Where-Object { $_ -and (Test-Path -LiteralPath $_ -PathType Leaf) }
  return $candidates | Select-Object -First 1
}

function Stop-ServiceAndWait {
  param(
    [Parameter(Mandatory = $true)][string]$Name,
    [Parameter(Mandatory = $true)][string]$NssmExecutable
  )
  $service = Get-Service -Name $Name -ErrorAction SilentlyContinue
  if (-not $service) { return $false }
  $wasRunning = $service.Status -ne "Stopped"
  if (-not $wasRunning) { return $false }
  & $NssmExecutable stop $Name | Out-Null
  for ($attempt = 0; $attempt -lt 60; $attempt++) {
    $service = Get-Service -Name $Name -ErrorAction SilentlyContinue
    if (-not $service -or $service.Status -eq "Stopped") { return $true }
    Start-Sleep -Milliseconds 500
  }
  throw "QIndexer service $Name did not stop within 30 seconds; setup aborted before replacing files."
}

function Set-QIndexerPrivatePostgresState {
  param([Parameter(Mandatory = $true)][bool]$Running)
  $service = Get-Service -Name "qindexer-postgresql" -ErrorAction SilentlyContinue
  if (-not $service) { return $false }
  $wasRunning = $service.Status -ne "Stopped"
  if ($Running -and -not $wasRunning) {
    Start-Service -Name "qindexer-postgresql"
  } elseif (-not $Running -and $wasRunning) {
    Stop-Service -Name "qindexer-postgresql" -Force
  }
  for ($attempt = 0; $attempt -lt 60; $attempt++) {
    $service = Get-Service -Name "qindexer-postgresql" -ErrorAction SilentlyContinue
    if (-not $service -or (($Running -and $service.Status -eq "Running") -or (-not $Running -and $service.Status -eq "Stopped"))) {
      return $wasRunning
    }
    Start-Sleep -Milliseconds 500
  }
  $desiredState = if ($Running) { "start" } else { "stop" }
  throw "QIndexer's private PostgreSQL service did not $desiredState within 30 seconds."
}

function Write-EmptyConfig {
  param(
    [Parameter(Mandatory = $true)][string]$Path,
    [Parameter(Mandatory = $true)][string]$DataDir,
    [Parameter(Mandatory = $true)][string]$DatabaseDSN
  )
  @"
server:
  bind: "127.0.0.1:41973"
  bind_addresses:
    - "127.0.0.1:41973"
  public_base_url: "http://127.0.0.1:41973"
  auth:
    mode: "static-token"

management:
  enabled: true
  bind: "127.0.0.1:41974"
  bind_addresses:
    - "127.0.0.1:41974"
  auth:
    mode: "admin-token"

index:
  data_dir: "$DataDir"
  database:
    driver: "postgres"
    dsn: "$DatabaseDSN"
    max_open_conns: 32
    max_idle_conns: 8

roots: []
"@ | Set-Content -LiteralPath $Path -Encoding utf8
}

Require-Administrator
$nssm = Find-Nssm $NssmPath
if (-not $nssm) {
  throw "NSSM was not found. Install NSSM, add nssm.exe to PATH, or pass -NssmPath C:\path\to\nssm.exe."
}
$sourceBinary = Find-Binary $BinaryPath
if (-not $sourceBinary) {
  throw "QIndexer Windows binary was not found. Pass -BinaryPath C:\path\to\qindexer.exe."
}

$restartExistingService = Stop-ServiceAndWait -Name $ServiceName -NssmExecutable $nssm
$restartPrivatePostgres = $false
if ($restartExistingService) {
  $restartPrivatePostgres = Set-QIndexerPrivatePostgresState -Running $false
}

$binDir = Join-Path $InstallDir "bin"
$configDir = Join-Path $InstallDir "config"
$dataDir = Join-Path $InstallDir "data"
$logsDir = Join-Path $dataDir "logs"
$scriptsDir = Join-Path $InstallDir "scripts"
New-Item -ItemType Directory -Force -Path $binDir, $configDir, $dataDir, $logsDir, $scriptsDir | Out-Null

Write-InstallProgress "running" "service_files" 20 "Installing QIndexer service files."
$installedBinary = Join-Path $binDir "qindexer.exe"
$configPath = Join-Path $configDir "qindexer.yaml"
Copy-Item -LiteralPath $sourceBinary -Destination $installedBinary -Force
$sidecarSource = Join-Path $PSScriptRoot "setup-sidecars-windows.ps1"
if (Test-Path -LiteralPath $sidecarSource) {
  Copy-Item -LiteralPath $sidecarSource -Destination (Join-Path $scriptsDir "setup-sidecars-windows.ps1") -Force
}
$postgresSource = Join-Path $PSScriptRoot "setup-postgres-windows.ps1"
if (Test-Path -LiteralPath $postgresSource) {
  Copy-Item -LiteralPath $postgresSource -Destination (Join-Path $scriptsDir "setup-postgres-windows.ps1") -Force
}
$progressSource = Join-Path $PSScriptRoot "install-progress-windows.ps1"
if (Test-Path -LiteralPath $progressSource) {
  Copy-Item -LiteralPath $progressSource -Destination (Join-Path $scriptsDir "install-progress-windows.ps1") -Force
}
if (-not (Test-Path -LiteralPath $configPath)) {
  if ($SkipDatabase) { throw "-SkipDatabase requires an existing configuration with an explicit database backend." }
  $postgresScript = Join-Path $scriptsDir "setup-postgres-windows.ps1"
  if (-not (Test-Path -LiteralPath $postgresScript)) { throw "The packaged PostgreSQL setup script is missing: $postgresScript" }
  $database = & $postgresScript -InstallDir $InstallDir -DataDir $dataDir -ResetIncomplete -ProgressPath $ProgressPath | ConvertFrom-Json
  if (-not $database.dsn) { throw "PostgreSQL setup did not return a QIndexer DSN." }
  Write-EmptyConfig -Path $configPath -DataDir $dataDir -DatabaseDSN $database.dsn
  Write-Host "Created an empty configuration. Add roots and set the first admin token in the web UI."
}
if ((Get-Content -LiteralPath $configPath -Raw) -match '(?m)^\s*dsn:\s*["'']?postgres://qindexer:change-me@') {
  if ($SkipDatabase) { throw "The existing configuration contains the PostgreSQL placeholder DSN, but -SkipDatabase was requested." }
  $postgresScript = Join-Path $scriptsDir "setup-postgres-windows.ps1"
  if (-not (Test-Path -LiteralPath $postgresScript)) { throw "The packaged PostgreSQL setup script is missing: $postgresScript" }
  $database = & $postgresScript -InstallDir $InstallDir -DataDir $dataDir -ResetIncomplete -ProgressPath $ProgressPath | ConvertFrom-Json
  if (-not $database.dsn) { throw "PostgreSQL setup did not return a QIndexer DSN." }
  $configText = Get-Content -LiteralPath $configPath -Raw
  $configText = [regex]::Replace($configText, '(?m)^(\s*dsn:\s*).*$', {
      param($match)
      return $match.Groups[1].Value + '"' + $database.dsn + '"'
    })
  Set-Content -LiteralPath $configPath -Value $configText -Encoding utf8
}

$installSidecars = $InstallSidecars
if (-not $PSBoundParameters.ContainsKey("InstallSidecars") -and [Environment]::UserInteractive) {
  $installSidecars = (Read-Host "Install optional OCR/extraction sidecars now? [y/N]") -match '^(?i:y|yes)$'
}
if ($installSidecars) {
  Write-InstallProgress "running" "service_sidecars" 85 "Installing optional extraction sidecars."
  $sidecarScript = Join-Path $scriptsDir "setup-sidecars-windows.ps1"
  if (-not (Test-Path -LiteralPath $sidecarScript)) {
    throw "The packaged Windows sidecar installer is missing: $sidecarScript"
  }
  & $sidecarScript -InstallDir $InstallDir -LogPath (Join-Path $logsDir "sidecar-install.log")
}

Write-InstallProgress "running" "service_register" 92 "Registering the QIndexer service."
$existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existing) {
  & $nssm remove $ServiceName confirm | Out-Null
  Start-Sleep -Seconds 1
}

& $nssm install $ServiceName $installedBinary | Out-Null
& $nssm set $ServiceName AppParameters "run --config `"$configPath`" --stop-private-postgres-on-exit" | Out-Null
& $nssm set $ServiceName AppDirectory $InstallDir | Out-Null
& $nssm set $ServiceName DisplayName "QIndexer" | Out-Null
& $nssm set $ServiceName Description "QIndexer filesystem indexing and search service." | Out-Null
& $nssm set $ServiceName Start SERVICE_AUTO_START | Out-Null
& $nssm set $ServiceName AppStdout (Join-Path $logsDir "service-stdout.log") | Out-Null
& $nssm set $ServiceName AppStderr (Join-Path $logsDir "service-stderr.log") | Out-Null
& $nssm set $ServiceName AppRotateFiles 1 | Out-Null
& $nssm set $ServiceName AppRotateOnline 1 | Out-Null
& $nssm set $ServiceName AppRotateBytes 10485760 | Out-Null
if ($restartPrivatePostgres) { Set-QIndexerPrivatePostgresState -Running $true | Out-Null }
& $nssm start $ServiceName | Out-Null

$service = Get-Service -Name $ServiceName
if ($service.Status -ne "Running") {
  throw "The service was installed but did not start. Check $logsDir\service-stderr.log and $logsDir\qindexer.log."
}

Write-InstallProgress "completed" "service_ready" 100 "QIndexer service is running."
Write-Host "QIndexer service is running."
Write-Host "  Service: $ServiceName"
Write-Host "  Admin UI: http://127.0.0.1:41974/"
Write-Host "  Config: $configPath"
Write-Host "  Logs: $logsDir"
