[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)]
  [string]$ConfigPath,
  [string]$InstallDir = "C:\Program Files\QIndexer",
  [switch]$EnableOCR
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

function Set-OCRCommandConfig {
  param(
    [Parameter(Mandatory = $true)][string]$Path,
    [Parameter(Mandatory = $true)][string]$Tesseract,
    [Parameter(Mandatory = $true)][string]$OCRmyPDF,
    [bool]$Enable
  )
  $lines = Get-Content -LiteralPath $Path
  $insideOCR = $false
  $changed = $false
  $out = foreach ($line in $lines) {
    if ($line -match '^  ocr:\s*$') { $insideOCR = $true; $line; continue }
    if ($insideOCR -and $line -match '^  [A-Za-z_][A-Za-z0-9_]*:\s*$') { $insideOCR = $false }
    if ($insideOCR -and $line -match '^    tesseract_command:\s*') {
      $changed = $true
      '    tesseract_command: "' + $Tesseract.Replace('"', '""') + '"'
    } elseif ($insideOCR -and $line -match '^    ocrmypdf_command:\s*') {
      $changed = $true
      '    ocrmypdf_command: "' + $OCRmyPDF.Replace('"', '""') + '"'
    } elseif ($insideOCR -and $Enable -and $line -match '^    enabled:\s*') {
      $changed = $true
      '    enabled: true'
    } else {
      $line
    }
  }
  if (-not $changed) { throw "Could not find the global crawler.ocr block in $Path" }
  Set-Content -LiteralPath $Path -Value $out -Encoding utf8
}

function Add-MachinePathEntry {
  param([Parameter(Mandatory = $true)][string]$Directory)
  $current = [Environment]::GetEnvironmentVariable("Path", "Machine")
  $entries = @($current -split ';' | Where-Object { $_ })
  if ($entries -contains $Directory) { return }
  [Environment]::SetEnvironmentVariable("Path", (($entries + $Directory) -join ';'), "Machine")
}

if (-not (Test-Path -LiteralPath $ConfigPath)) { throw "Config not found: $ConfigPath" }
if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
  throw "winget is required for automated Windows sidecar setup. Install Tesseract OCR, Ghostscript, Python 3, and OCRmyPDF manually, then set their command paths in $ConfigPath."
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
$env:Path = (Split-Path -Parent $tesseract) + ';' + (Split-Path -Parent $ghostscript) + ';' + $env:Path
Set-OCRCommandConfig -Path $ConfigPath -Tesseract $tesseract -OCRmyPDF $ocrmypdf -Enable $EnableOCR
Write-Host "OCR sidecars are installed and configured."
Write-Host "  Tesseract: $tesseract"
Write-Host "  OCRmyPDF:  $ocrmypdf"
Write-Host "  Ghostscript: $ghostscript"
Write-Host "  Service PATH: registered Tesseract and Ghostscript directories at machine scope"
if (-not $EnableOCR) { Write-Host "OCR remains disabled in the config. Re-run with -EnableOCR when you are ready to process the backlog." }
