param(
  [string]$InstallDir = "C:\Program Files\QIndexer",
  [string]$ConfigPath = "",
  [string]$BinaryPath = "",
  [switch]$SkipDatabase,
  [switch]$InstallSidecars,
  [switch]$InstallService,
  [switch]$StartService,
  [string]$ServiceName = "QIndexer",
  [string]$ProgressPath = ""
)

$ErrorActionPreference = "Stop"

function Write-InstallProgress {
  param([string]$Status, [string]$Stage, [int]$Percent, [string]$Message)
  Write-QIndexerInstallProgress -ProgressPath $ProgressPath -Status $Status -Stage $Stage -Percent $Percent -Message $Message
}

function Resolve-RepoRoot {
  $scriptDir = Split-Path -Parent $PSCommandPath
  return Split-Path -Parent $scriptDir
}

function Require-Admin {
  $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
  $principal = New-Object Security.Principal.WindowsPrincipal($identity)
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Service installation requires an elevated PowerShell session."
  }
}

function Set-PrivateAcl {
  param([Parameter(Mandatory = $true)][string]$Path)
  $item = Get-Item -LiteralPath $Path
  $acl = Get-Acl -LiteralPath $Path
  $acl.SetAccessRuleProtection($true, $false)
  foreach ($rule in @($acl.Access)) {
    [void]$acl.RemoveAccessRule($rule)
  }
  $identities = @(
    [Security.Principal.WindowsIdentity]::GetCurrent().Name,
    "BUILTIN\Administrators",
    "NT AUTHORITY\SYSTEM"
  ) | Select-Object -Unique
  foreach ($identity in $identities) {
    if ($item.PSIsContainer) {
      $inheritance = [Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [Security.AccessControl.InheritanceFlags]::ObjectInherit
      $rule = [Security.AccessControl.FileSystemAccessRule]::new($identity, [Security.AccessControl.FileSystemRights]::FullControl, $inheritance, [Security.AccessControl.PropagationFlags]::None, [Security.AccessControl.AccessControlType]::Allow)
    } else {
      $rule = [Security.AccessControl.FileSystemAccessRule]::new($identity, [Security.AccessControl.FileSystemRights]::FullControl, [Security.AccessControl.AccessControlType]::Allow)
    }
    $acl.AddAccessRule($rule)
  }
  Set-Acl -LiteralPath $Path -AclObject $acl
}

function Stop-QIndexerService {
  param([Parameter(Mandatory = $true)][string]$Name)
  $service = Get-Service -Name $Name -ErrorAction SilentlyContinue
  if (-not $service) { return $false }
  $wasRunning = $service.Status -ne "Stopped"
  if ($wasRunning) { Stop-Service -Name $Name -Force }
  for ($attempt = 0; $attempt -lt 60; $attempt++) {
    $service = Get-Service -Name $Name -ErrorAction SilentlyContinue
    if (-not $service -or $service.Status -eq "Stopped") { return $wasRunning }
    Start-Sleep -Milliseconds 500
  }
  throw "QIndexer service $Name did not stop within 30 seconds; update aborted before replacing files."
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

$repoRoot = Resolve-RepoRoot
. (Join-Path $repoRoot "scripts\install-progress-windows.ps1")
trap {
  Write-InstallProgress "failed" "install" 100 $_.Exception.Message
  throw $_
}
Write-InstallProgress "running" "install_prepare" 5 "Preparing the QIndexer installation."
if (-not $BinaryPath) {
  $candidates = @(
    (Join-Path $repoRoot "qindexer.exe"),
    (Join-Path $repoRoot "qindexer_windows_amd64.exe"),
    (Join-Path $repoRoot "outputs\bin\qindexer_windows_amd64.exe"),
    (Join-Path $repoRoot "src\dist\qindexer_windows_amd64.exe")
  )
  $BinaryPath = $candidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
}
if (-not (Test-Path -LiteralPath $BinaryPath)) {
  throw "QIndexer binary not found: $BinaryPath"
}

$restartExistingService = Stop-QIndexerService -Name $ServiceName
$restartPrivatePostgres = $false
if ($restartExistingService) {
  $restartPrivatePostgres = Set-QIndexerPrivatePostgresState -Running $false
}

$binDir = Join-Path $InstallDir "bin"
$scriptDir = Join-Path $InstallDir "scripts"
$configDir = Join-Path $InstallDir "configs"
$dataDir = Join-Path $InstallDir "data"
$logDir = Join-Path $dataDir "logs"
New-Item -ItemType Directory -Force -Path $binDir, $scriptDir, $configDir, $dataDir, $logDir | Out-Null

Write-InstallProgress "running" "install_files" 20 "Installing QIndexer files."
$installedBinary = Join-Path $binDir "qindexer.exe"
Copy-Item -LiteralPath $BinaryPath -Destination $installedBinary -Force
Copy-Item -LiteralPath (Join-Path $repoRoot "scripts\setup-sidecars-windows.ps1") -Destination (Join-Path $scriptDir "setup-sidecars-windows.ps1") -Force
Copy-Item -LiteralPath (Join-Path $repoRoot "scripts\setup-postgres-windows.ps1") -Destination (Join-Path $scriptDir "setup-postgres-windows.ps1") -Force
Copy-Item -LiteralPath (Join-Path $repoRoot "scripts\install-progress-windows.ps1") -Destination (Join-Path $scriptDir "install-progress-windows.ps1") -Force

if ($ConfigPath -and (Test-Path -LiteralPath $ConfigPath)) {
  Copy-Item -LiteralPath $ConfigPath -Destination (Join-Path $configDir "qindexer.yaml") -Force
} elseif (-not (Test-Path -LiteralPath (Join-Path $configDir "qindexer.yaml"))) {
  if ($SkipDatabase) {
    throw "-SkipDatabase requires -ConfigPath when no existing QIndexer configuration is installed."
  }
  Require-Admin
  $database = & (Join-Path $scriptDir "setup-postgres-windows.ps1") -InstallDir $InstallDir -DataDir $dataDir -ResetIncomplete -ProgressPath $ProgressPath | ConvertFrom-Json
  if (-not $database.dsn) { throw "PostgreSQL setup did not return a QIndexer DSN." }
  $sampleConfig = @(
    (Join-Path $repoRoot "configs\example.yaml"),
    (Join-Path $repoRoot "src\configs\example.yaml")
  ) | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
  if (-not $sampleConfig) { throw "No example config found in the release bundle or repository." }
  Copy-Item -LiteralPath $sampleConfig -Destination (Join-Path $configDir "qindexer.yaml") -Force
  $installedConfig = Join-Path $configDir "qindexer.yaml"
  $configText = Get-Content -LiteralPath $installedConfig -Raw
  $configText = [regex]::Replace($configText, '(?m)^(\s*dsn:\s*).*$', {
      param($match)
      return $match.Groups[1].Value + '"' + $database.dsn + '"'
    })
  Set-Content -LiteralPath $installedConfig -Value $configText -Encoding utf8
}

$installedConfig = Join-Path $configDir "qindexer.yaml"
if (Test-Path -LiteralPath $installedConfig) {
  $configText = Get-Content -LiteralPath $installedConfig -Raw
  $needsProvisionedDatabase = $configText -match '(?m)^\s*dsn:\s*["'']?postgres://qindexer:change-me@'
  if ($needsProvisionedDatabase) {
    if ($SkipDatabase) { throw "The supplied configuration contains the PostgreSQL placeholder DSN, but -SkipDatabase was requested." }
    Require-Admin
    $database = & (Join-Path $scriptDir "setup-postgres-windows.ps1") -InstallDir $InstallDir -DataDir $dataDir -ResetIncomplete -ProgressPath $ProgressPath | ConvertFrom-Json
    if (-not $database.dsn) { throw "PostgreSQL setup did not return a QIndexer DSN." }
    $configText = [regex]::Replace($configText, '(?m)^(\s*dsn:\s*).*$', {
        param($match)
        return $match.Groups[1].Value + '"' + $database.dsn + '"'
      })
    Set-Content -LiteralPath $installedConfig -Value $configText -Encoding utf8
  }
}

Set-PrivateAcl -Path $configDir
Set-PrivateAcl -Path $dataDir
Set-PrivateAcl -Path $logDir
Set-PrivateAcl -Path $scriptDir
if (Test-Path -LiteralPath $installedConfig) {
  Set-PrivateAcl -Path $installedConfig
}

if ($InstallSidecars) {
  Require-Admin
  Write-InstallProgress "running" "install_sidecars" 88 "Installing optional extraction sidecars."
  & (Join-Path $scriptDir "setup-sidecars-windows.ps1") -InstallDir $InstallDir
}

if ($InstallService) {
  Require-Admin
  Write-InstallProgress "running" "install_service" 92 "Installing the QIndexer service."
  $existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if ($existing) {
    sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 1
  }
  $binPath = "`"$installedBinary`" run --config `"$installedConfig`" --stop-private-postgres-on-exit"
  sc.exe create $ServiceName binPath= $binPath start= auto DisplayName= "QIndexer Search Service" | Out-Null
  sc.exe description $ServiceName "Portable filesystem search indexer for QSurfer." | Out-Null
  if ($StartService -or $restartExistingService) {
    Set-QIndexerPrivatePostgresState -Running $true | Out-Null
    Start-Service -Name $ServiceName
  }
} elseif ($restartExistingService) {
  if ($restartPrivatePostgres) { Set-QIndexerPrivatePostgresState -Running $true | Out-Null }
  Start-Service -Name $ServiceName
}

Write-InstallProgress "completed" "install_ready" 100 "QIndexer installation is complete."
Write-Host "QIndexer installed"
Write-Host "  Binary: $installedBinary"
Write-Host "  Config: $installedConfig"
Write-Host "  Data:   $dataDir"
Write-Host "  Logs:   $logDir\qindexer.log"
