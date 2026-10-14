param(
  [string]$InstallDir = "C:\Program Files\QIndexer",
  [string]$ConfigPath = "",
  [string]$BinaryPath = "",
  [switch]$SkipDatabase,
  [switch]$InstallSidecars,
  [switch]$InstallService,
  [switch]$StartService,
  [string]$ServiceName = "QIndexer"
)

$ErrorActionPreference = "Stop"

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
    $rule = New-Object Security.AccessControl.FileSystemAccessRule($identity, "FullControl", "ContainerInherit,ObjectInherit", "None", "Allow")
    $acl.AddAccessRule($rule)
  }
  Set-Acl -LiteralPath $Path -AclObject $acl
}

$repoRoot = Resolve-RepoRoot
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

$binDir = Join-Path $InstallDir "bin"
$scriptDir = Join-Path $InstallDir "scripts"
$configDir = Join-Path $InstallDir "configs"
$dataDir = Join-Path $InstallDir "data"
$logDir = Join-Path $dataDir "logs"
New-Item -ItemType Directory -Force -Path $binDir, $scriptDir, $configDir, $dataDir, $logDir | Out-Null

$installedBinary = Join-Path $binDir "qindexer.exe"
Copy-Item -LiteralPath $BinaryPath -Destination $installedBinary -Force
Copy-Item -LiteralPath (Join-Path $repoRoot "scripts\setup-sidecars-windows.ps1") -Destination (Join-Path $scriptDir "setup-sidecars-windows.ps1") -Force
Copy-Item -LiteralPath (Join-Path $repoRoot "scripts\setup-postgres-windows.ps1") -Destination (Join-Path $scriptDir "setup-postgres-windows.ps1") -Force

if ($ConfigPath -and (Test-Path -LiteralPath $ConfigPath)) {
  Copy-Item -LiteralPath $ConfigPath -Destination (Join-Path $configDir "qindexer.yaml") -Force
} elseif (-not (Test-Path -LiteralPath (Join-Path $configDir "qindexer.yaml"))) {
  if ($SkipDatabase) {
    throw "-SkipDatabase requires -ConfigPath when no existing QIndexer configuration is installed."
  }
  Require-Admin
  $database = & (Join-Path $scriptDir "setup-postgres-windows.ps1") -InstallDir $InstallDir -DataDir $dataDir | ConvertFrom-Json
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
    $database = & (Join-Path $scriptDir "setup-postgres-windows.ps1") -InstallDir $InstallDir -DataDir $dataDir | ConvertFrom-Json
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
  & (Join-Path $scriptDir "setup-sidecars-windows.ps1") -InstallDir $InstallDir
}

if ($InstallService) {
  Require-Admin
  $existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if ($existing) {
    if ($existing.Status -ne "Stopped") {
      Stop-Service -Name $ServiceName -Force
      $existing.WaitForStatus("Stopped", "00:00:20")
    }
    sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 1
  }
  $binPath = "`"$installedBinary`" run --config `"$installedConfig`""
  sc.exe create $ServiceName binPath= $binPath start= auto DisplayName= "QIndexer Search Service" | Out-Null
  sc.exe description $ServiceName "Portable filesystem search indexer for QSurfer." | Out-Null
  if ($StartService) {
    Start-Service -Name $ServiceName
  }
}

Write-Host "QIndexer installed"
Write-Host "  Binary: $installedBinary"
Write-Host "  Config: $installedConfig"
Write-Host "  Data:   $dataDir"
Write-Host "  Logs:   $logDir\qindexer.log"
