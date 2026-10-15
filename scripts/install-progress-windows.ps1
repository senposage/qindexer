function Write-QIndexerInstallProgress {
  param(
    [string]$ProgressPath,
    [Parameter(Mandatory = $true)][string]$Status,
    [Parameter(Mandatory = $true)][string]$Stage,
    [Parameter(Mandatory = $true)][int]$Percent,
    [Parameter(Mandatory = $true)][string]$Message,
    [long]$BytesDownloaded = 0,
    [long]$BytesTotal = 0,
    [long]$BytesPerSecond = 0
  )

  if (-not $ProgressPath) { return }
  try {
    $directory = Split-Path -Parent $ProgressPath
    if ($directory) { New-Item -ItemType Directory -Force -Path $directory | Out-Null }
    $state = [ordered]@{
      schema_version = 1
      status = $Status
      stage = $Stage
      percent = [Math]::Max(0, [Math]::Min(100, $Percent))
      message = $Message
      bytes_downloaded = [Math]::Max(0, $BytesDownloaded)
      bytes_total = [Math]::Max(0, $BytesTotal)
      bytes_per_second = [Math]::Max(0, $BytesPerSecond)
      updated_at = [DateTime]::UtcNow.ToString("o")
    } | ConvertTo-Json -Compress
    $temporary = "$ProgressPath.tmp"
    [IO.File]::WriteAllText($temporary, $state + [Environment]::NewLine, (New-Object Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $temporary -Destination $ProgressPath -Force
  } catch {
    Write-Warning "Could not write QIndexer installer progress: $($_.Exception.Message)"
  }
}
