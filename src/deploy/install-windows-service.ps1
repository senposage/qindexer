param(
  [string]$BinaryPath = "C:\Program Files\QSurferSearch\qindexer.exe",
  [string]$ConfigPath = "C:\ProgramData\QSurferSearch\config.yaml",
  [string]$ServiceName = "QSurferSearchService"
)

$ErrorActionPreference = "Stop"

$quotedBinary = '"' + $BinaryPath + '" run --config "' + $ConfigPath + '"'

if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
  Write-Host "Service already exists: $ServiceName"
  exit 0
}

New-Service `
  -Name $ServiceName `
  -BinaryPathName $quotedBinary `
  -DisplayName "QIndexer" `
  -Description "Indexes configured local, SMB, and NAS roots for QSurfer search." `
  -StartupType Automatic

Write-Host "Installed service: $ServiceName"
Write-Host "Start it with: Start-Service $ServiceName"

