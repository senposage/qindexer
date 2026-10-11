[CmdletBinding()]
param(
  [string]$InstallDir = "C:\Program Files\QIndexer",
  [string]$LogPath = ""
)

$ErrorActionPreference = "Stop"
$transcriptStarted = $false
if ($LogPath) {
  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $LogPath) | Out-Null
  Start-Transcript -Path $LogPath -Append | Out-Null
  $transcriptStarted = $true
}

function Find-Executable {
  param([Parameter(Mandatory = $true)][string[]]$Candidates)
  foreach ($candidate in $Candidates) {
    $command = Get-Command $candidate -ErrorAction SilentlyContinue
    if ($command) { return $command.Source }
  }
  $roots = @($env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:LOCALAPPDATA) | Where-Object { $_ -and (Test-Path $_) }
  foreach ($root in $roots) {
    foreach ($candidate in $Candidates) {
      $found = Get-ChildItem -LiteralPath $root -Filter $candidate -File -Recurse -ErrorAction SilentlyContinue | Select-Object -First 1
      if ($found) { return $found.FullName }
    }
  }
  return $null
}

function Install-WingetPackage {
  param([Parameter(Mandatory = $true)][string]$Id)
  if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
    throw "winget is unavailable, so QIndexer cannot automatically install $Id on this machine."
  }
  Write-Host "Installing $Id with winget..."
  & winget install --id $Id --exact --silent --accept-package-agreements --accept-source-agreements
  if ($LASTEXITCODE -ne 0) { throw "winget failed installing $Id (exit code $LASTEXITCODE)" }
}

function Install-Tesseract {
  if (Find-Executable @("tesseract.exe")) { return }

  # Resolve the current upstream Windows installer so setup does not depend on
  # a per-user winget registration or a release version baked into QIndexer.
  $installer = ""
  try {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/tesseract-ocr/tesseract/releases/latest" -Headers @{ "User-Agent" = "QIndexer-sidecar-setup" }
    $asset = @($release.assets | Where-Object { $_.name -match '^tesseract-ocr-w64-setup-.*\.exe$' } | Select-Object -First 1)[0]
    if (-not $asset -or -not $asset.browser_download_url) { throw "The latest upstream Tesseract release has no 64-bit Windows installer asset." }
    $installer = Join-Path $env:TEMP ("qindexer-" + $asset.name)
    Write-Host "Downloading Tesseract $($release.tag_name) from the upstream GitHub release..."
    Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $installer -UseBasicParsing
    if ((Get-Item -LiteralPath $installer).Length -lt 10MB) {
      throw "Downloaded Tesseract installer is unexpectedly small."
    }
    Write-Host "Installing Tesseract..."
    $process = Start-Process -FilePath $installer -ArgumentList "/S" -Wait -PassThru
    if ($process.ExitCode -ne 0) { throw "Tesseract installer failed with exit code $($process.ExitCode)." }
  } catch {
    Write-Warning "Direct Tesseract setup failed: $($_.Exception.Message)"
    Install-WingetPackage "UB-Mannheim.TesseractOCR"
  } finally {
    if ($installer) { Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue }
  }
  if (-not (Find-Executable @("tesseract.exe"))) {
    throw "Tesseract installation completed but tesseract.exe was not found."
  }
}

function Add-MachinePathEntry {
  param([Parameter(Mandatory = $true)][string]$Directory)
  $current = [Environment]::GetEnvironmentVariable("Path", "Machine")
  $entries = @($current -split ';' | Where-Object { $_ })
  if ($entries -contains $Directory) { return }
  [Environment]::SetEnvironmentVariable("Path", (($entries + $Directory) -join ';'), "Machine")
}

try {
  Install-Tesseract
  $tesseract = Find-Executable @("tesseract.exe")

  $ghostscript = Find-Executable @("gswin64c.exe", "gswin32c.exe")
  if (-not $ghostscript) {
    Install-WingetPackage "ArtifexSoftware.Ghostscript"
    $ghostscript = Find-Executable @("gswin64c.exe", "gswin32c.exe")
  }

  $ocrmypdf = Find-Executable @("ocrmypdf.exe")
  if (-not $ocrmypdf) {
    $python = Find-Executable @("py.exe", "python.exe")
    if (-not $python) {
      Install-WingetPackage "Python.Python.3.12"
      $python = Find-Executable @("py.exe", "python.exe")
    }
    if (-not $python) { throw "Python was installed but is not available; restart PowerShell and re-run this script." }
    $venv = Join-Path $InstallDir "sidecars\python"
    & $python -m venv $venv
    if ($LASTEXITCODE -ne 0) { throw "Failed creating the OCRmyPDF virtual environment" }
    $venvPython = Join-Path $venv "Scripts\python.exe"
    & $venvPython -m pip install --upgrade pip ocrmypdf
    if ($LASTEXITCODE -ne 0) { throw "Failed installing OCRmyPDF into $venv" }
    $ocrmypdf = Join-Path $venv "Scripts\ocrmypdf.exe"
  }

  if (-not $tesseract -or -not (Test-Path -LiteralPath $ocrmypdf) -or -not $ghostscript) {
    throw "OCR sidecar verification failed. Tesseract=$tesseract OCRmyPDF=$ocrmypdf Ghostscript=$ghostscript"
  }

  # OCRmyPDF launches Ghostscript itself. Register the machine-level directories
  # so it works under LocalSystem and other service accounts, not just this shell.
  Add-MachinePathEntry (Split-Path -Parent $tesseract)
  Add-MachinePathEntry (Split-Path -Parent $ghostscript)
  Add-MachinePathEntry (Split-Path -Parent $ocrmypdf)
  $env:Path = (Split-Path -Parent $tesseract) + ';' + (Split-Path -Parent $ghostscript) + ';' + (Split-Path -Parent $ocrmypdf) + ';' + $env:Path
  Write-Host "OCR sidecars are installed and available on PATH."
  Write-Host "  Tesseract: $tesseract"
  Write-Host "  OCRmyPDF:  $ocrmypdf"
  Write-Host "  Ghostscript: $ghostscript"
  Write-Host "  Service PATH: registered Tesseract, Ghostscript, and OCRmyPDF directories at machine scope"
} finally {
  if ($transcriptStarted) { Stop-Transcript | Out-Null }
}
