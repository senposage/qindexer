param(
  [string]$ServiceName = "QSurferSearchService"
)

$ErrorActionPreference = "Stop"

$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if (-not $service) {
  Write-Host "Service does not exist: $ServiceName"
  exit 0
}

if ($service.Status -ne "Stopped") {
  Stop-Service -Name $ServiceName -Force
}

sc.exe delete $ServiceName | Out-Host
Write-Host "Deleted service: $ServiceName"

