[CmdletBinding()]
param(
  [string]$InstallDir = "C:\Program Files\QIndexer"
)

$ErrorActionPreference = "Stop"

function Require-Command {
  param([Parameter(Mandatory = $true)][string]$Name)
  $command = Get-Command $Name -ErrorAction SilentlyContinue
  if (-not $command) { throw "Required command was not found after installation: $Name" }
  return $command.Source
}

function Install-WingetPackage {
  param([Parameter(Mandatory = $true)][string]$Id)
  & winget install --id $Id --exact --silent --accept-package-agreements --accept-source-agreements
  if ($LASTEXITCODE -ne 0) { throw "winget failed installing $Id" }
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

function Add-MachinePathEntry {
  param([Parameter(Mandatory = $true)][string]$Directory)
  $current = [Environment]::GetEnvironmentVariable("Path", "Machine")
  $entries = @($current -split ';' | Where-Object { $_ })
  if ($entries -contains $Directory) { return }
  [Environment]::SetEnvironmentVariable("Path", (($entries + $Directory) -join ';'), "Machine")
}

if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
  throw "winget is required for automated Windows sidecar setup. Install Tesseract OCR, Ghostscript, Python 3, and OCRmyPDF manually, then add their executable directories to the machine PATH."
}

Install-WingetPackage "UB-Mannheim.TesseractOCR"
Install-WingetPackage "ArtifexSoftware.Ghostscript"
if (-not (Get-Command py -ErrorAction SilentlyContinue) -and -not (Get-Command python -ErrorAction SilentlyContinue)) {
  Install-WingetPackage "Python.Python.3.12"
}

$python = Find-Executable @("py.exe", "python.exe")
if (-not $python) { throw "Python was installed but is not available; restart PowerShell and re-run this script." }
$venv = Join-Path $InstallDir "sidecars\python"
& $python -m venv $venv
if ($LASTEXITCODE -ne 0) { throw "Failed creating the OCRmyPDF virtual environment" }
$venvPython = Join-Path $venv "Scripts\python.exe"
& $venvPython -m pip install --upgrade pip ocrmypdf
if ($LASTEXITCODE -ne 0) { throw "Failed installing OCRmyPDF into $venv" }

$tesseract = Find-Executable @("tesseract.exe")
$ocrmypdf = Join-Path $venv "Scripts\ocrmypdf.exe"
$ghostscript = Find-Executable @("gswin64c.exe", "gswin32c.exe")
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
