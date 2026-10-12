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
  # Do not inspect PATH here: it can include disconnected network locations
  # and make an installer appear hung. Check only standard all-user locations.
  $directories = @(
    (Join-Path $env:ProgramFiles "Tesseract-OCR"),
    (Join-Path $env:ProgramFiles "Python312"),
    (Join-Path $env:ProgramFiles "Python312\Scripts")
  )
  $ghostscriptRoot = Join-Path $env:ProgramFiles "gs"
  if (Test-Path -LiteralPath $ghostscriptRoot) {
    $directories += Get-ChildItem -LiteralPath $ghostscriptRoot -Directory -ErrorAction SilentlyContinue | ForEach-Object { Join-Path $_.FullName "bin" }
  }
  foreach ($directory in $directories) {
    foreach ($candidate in $Candidates) {
      $path = Join-Path $directory $candidate
      if (Test-Path -LiteralPath $path -PathType Leaf) { return $path }
    }
  }
  return $null
}

function Confirm-SignedExecutable {
  param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][string]$Name)
  $signature = Get-AuthenticodeSignature -FilePath $Path
  if ($signature.Status -ne "Valid") {
    throw "$Name download does not have a valid Authenticode signature (status $($signature.Status))."
  }
}

function Get-BundledExtractor {
  param([Parameter(Mandatory = $true)][string]$InstallDir)
  $toolsDir = Join-Path $InstallDir "sidecars\tools\7zip"
  $extractor = Join-Path $toolsDir "7z.exe"
  if (Test-Path -LiteralPath $extractor) { return $extractor }

  $installer = Join-Path $env:TEMP "qindexer-7zip-x64.exe"
  try {
    # 7-Zip supports NSIS archives, unlike its intentionally reduced 7zr tool.
    # It is installed privately beneath QIndexer and is never added to PATH.
    Write-Host "Downloading QIndexer's private extraction runtime..."
    Invoke-WebRequest -Uri "https://www.7-zip.org/a/7z2603-x64.exe" -OutFile $installer -UseBasicParsing
    if ((Get-Item -LiteralPath $installer).Length -lt 1MB) { throw "Downloaded 7-Zip installer is unexpectedly small." }
    $arguments = "/S /D=`"$toolsDir`""
    $process = Start-Process -FilePath $installer -ArgumentList $arguments -Wait -PassThru
    if ($process.ExitCode -ne 0) { throw "Private 7-Zip setup failed with exit code $($process.ExitCode)." }
  } finally {
    if (Test-Path -LiteralPath $installer) { Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue }
  }
  if (-not (Test-Path -LiteralPath $extractor)) { throw "Private 7-Zip setup completed but 7z.exe was not found." }
  return $extractor
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
    throw "Direct Tesseract setup failed: $($_.Exception.Message)"
  } finally {
    if ($installer) { Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue }
  }
  if (-not (Find-Executable @("tesseract.exe"))) {
    throw "Tesseract installation completed but tesseract.exe was not found."
  }
}

function Install-Ghostscript {
  param([Parameter(Mandatory = $true)][string]$InstallDir)
  if ($existing = Find-Executable @("gswin64c.exe", "gswin32c.exe")) { return $existing }
  $privateRuntime = Get-ChildItem -LiteralPath (Join-Path $InstallDir "sidecars\ghostscript") -Filter "gswin64c.exe" -File -Recurse -ErrorAction SilentlyContinue | Select-Object -First 1
  if (-not $privateRuntime) { $privateRuntime = Get-ChildItem -LiteralPath (Join-Path $InstallDir "sidecars\ghostscript") -Filter "gswin32c.exe" -File -Recurse -ErrorAction SilentlyContinue | Select-Object -First 1 }
  if ($privateRuntime) { return $privateRuntime.FullName }

  # Ghostscript publishes the current Windows installer and its SHA-512 digest
  # together in its official release feed. This works under LocalSystem, where
  # the user-scoped winget client is typically unavailable.
  $installer = ""
  try {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/ArtifexSoftware/ghostpdl-downloads/releases/latest" -Headers @{ "User-Agent" = "QIndexer-sidecar-setup" }
    $asset = @($release.assets | Where-Object { $_.name -match '^gs\d+w64\.exe$' } | Select-Object -First 1)[0]
    $checksums = @($release.assets | Where-Object { $_.name -eq "SHA512SUMS" } | Select-Object -First 1)[0]
    if (-not $asset -or -not $asset.browser_download_url -or -not $checksums -or -not $checksums.browser_download_url) {
      throw "The latest upstream Ghostscript release has no 64-bit Windows installer or SHA-512 manifest."
    }

    $installer = Join-Path $env:TEMP ("qindexer-" + $asset.name)
    Write-Host "Downloading Ghostscript $($release.tag_name) from the upstream release..."
    Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $installer -UseBasicParsing
    $checksumResponse = Invoke-WebRequest -Uri $checksums.browser_download_url -UseBasicParsing
    $checksumText = if ($checksumResponse.Content -is [byte[]]) { [Text.Encoding]::UTF8.GetString($checksumResponse.Content) } else { [string]$checksumResponse.Content }
    $expected = [regex]::Match($checksumText, "(?im)^(?<hash>[a-f0-9]{128})\s+\*?" + [regex]::Escape($asset.name) + "\s*$")
    if (-not $expected.Success) { throw "The Ghostscript SHA-512 manifest does not contain $($asset.name)." }
    $actual = (Get-FileHash -LiteralPath $installer -Algorithm SHA512).Hash.ToLowerInvariant()
    if ($actual -ne $expected.Groups['hash'].Value.ToLowerInvariant()) { throw "The downloaded Ghostscript installer did not match the official SHA-512 checksum." }

    $sevenZip = Get-BundledExtractor -InstallDir $InstallDir
    $destination = Join-Path $InstallDir "sidecars\ghostscript"
    New-Item -ItemType Directory -Force -Path $destination | Out-Null
    Write-Host "Extracting the Ghostscript runtime into $destination..."
    # Do not let 7-Zip's progress text become this function's return value.
    & $sevenZip x -y ("-o" + $destination) $installer | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "7-Zip failed extracting the Ghostscript runtime (exit code $LASTEXITCODE)." }
    $ghostscript = Get-ChildItem -LiteralPath $destination -Filter "gswin64c.exe" -File -Recurse -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $ghostscript) { $ghostscript = Get-ChildItem -LiteralPath $destination -Filter "gswin32c.exe" -File -Recurse -ErrorAction SilentlyContinue | Select-Object -First 1 }
    if (-not $ghostscript) { throw "Ghostscript extraction completed but gswin64c.exe was not found." }
    return $ghostscript.FullName
  } catch {
    throw "Direct Ghostscript setup failed: $($_.Exception.Message)"
  } finally {
    if ($installer) { Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue }
  }

  if (-not (Find-Executable @("gswin64c.exe", "gswin32c.exe"))) {
    throw "Ghostscript installation completed but gswin64c.exe was not found."
  }
}

function Install-Python {
  # OCRmyPDF currently supports Python 3.12. Pin this installer so a fresh
  # machine gets a tested service-wide runtime, not whichever user Python or
  # app-execution alias happens to be on PATH.
  $pythonHome = Join-Path $env:ProgramFiles "Python312"
  $python = Join-Path $pythonHome "python.exe"
  if (Test-Path -LiteralPath $python) { return $python }

  $version = "3.12.10"
  $installer = Join-Path $env:TEMP "qindexer-python-$version-amd64.exe"
  try {
    Write-Host "Downloading Python $version from python.org..."
    Invoke-WebRequest -Uri "https://www.python.org/ftp/python/$version/python-$version-amd64.exe" -OutFile $installer -UseBasicParsing
    if ((Get-Item -LiteralPath $installer).Length -lt 20MB) { throw "Downloaded Python installer is unexpectedly small." }
    Confirm-SignedExecutable -Path $installer -Name "Python installer"
    Write-Host "Installing Python for all users..."
    $arguments = "/quiet InstallAllUsers=1 PrependPath=1 Include_pip=1 Include_launcher=1 Include_test=0 Include_doc=0 Shortcuts=0"
    $process = Start-Process -FilePath $installer -ArgumentList $arguments -Wait -PassThru
    if ($process.ExitCode -ne 0) { throw "Python installer failed with exit code $($process.ExitCode)." }
  } finally {
    if (Test-Path -LiteralPath $installer) { Remove-Item -LiteralPath $installer -Force -ErrorAction SilentlyContinue }
  }
  if (-not (Test-Path -LiteralPath $python)) { throw "Python installation completed but $python was not found." }
  return $python
}

function Add-MachinePathEntry {
  param([Parameter(Mandatory = $true)][string]$Directory)
  $environmentKey = "HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment"
  $current = (Get-ItemProperty -LiteralPath $environmentKey -Name "Path").Path
  $entries = @($current -split ';' | Where-Object { $_ })
  if ($entries -contains $Directory) { return }
  # Environment.SetEnvironmentVariable broadcasts to every interactive window
  # and can hang behind an unresponsive process. A direct registry update takes
  # effect for new processes without blocking the service-side installer.
  Set-ItemProperty -LiteralPath $environmentKey -Name "Path" -Value (($entries + $Directory) -join ';')
}

function Notify-EnvironmentChanged {
  if (-not ("QIndexer.NativeMethods" -as [type])) {
    Add-Type @"
using System;
using System.Runtime.InteropServices;
namespace QIndexer {
  public static class NativeMethods {
    [DllImport("user32.dll", CharSet = CharSet.Auto, SetLastError = true)]
    public static extern IntPtr SendMessageTimeout(
      IntPtr hWnd, uint msg, UIntPtr wParam, string lParam,
      uint flags, uint timeout, out UIntPtr result);
  }
}
"@
  }
  $result = [UIntPtr]::Zero
  # Timed broadcast refreshes Explorer/new terminal environments without
  # allowing an unresponsive window to hang the LocalSystem installer.
  [void][QIndexer.NativeMethods]::SendMessageTimeout([IntPtr]0xffff, 0x001a, [UIntPtr]::Zero, "Environment", 0x0002, 1000, [ref]$result)
}

try {
  Write-Host "Checking Tesseract..."
  Install-Tesseract
  $tesseract = Find-Executable @("tesseract.exe")
  if (-not $tesseract) { throw "Tesseract installation completed but tesseract.exe was not found." }
  # Register each successful dependency immediately. A later optional setup
  # failure must not leave an installed Tesseract unusable from CMD or services.
  Add-MachinePathEntry (Split-Path -Parent $tesseract)
  $env:Path = (Split-Path -Parent $tesseract) + ';' + $env:Path

  Write-Host "Checking Ghostscript..."
  $ghostscript = Find-Executable @("gswin64c.exe", "gswin32c.exe")
  if (-not $ghostscript) {
    $ghostscript = Install-Ghostscript -InstallDir $InstallDir
  }

  Write-Host "Checking OCRmyPDF..."
  $ocrmypdf = Find-Executable @("ocrmypdf.exe")
  if (-not $ocrmypdf) {
    $privateOCRmyPDF = Join-Path $InstallDir "sidecars\python\Scripts\ocrmypdf.exe"
    if (Test-Path -LiteralPath $privateOCRmyPDF) { $ocrmypdf = $privateOCRmyPDF }
  }
  if (-not $ocrmypdf) {
    $python = Install-Python
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

  Write-Host "Registering OCR sidecars for services..."
  # OCRmyPDF launches Ghostscript itself. Register the machine-level directories
  # so it works under LocalSystem and other service accounts, not just this shell.
  Add-MachinePathEntry (Split-Path -Parent $tesseract)
  Add-MachinePathEntry (Split-Path -Parent $ghostscript)
  Add-MachinePathEntry (Split-Path -Parent $ocrmypdf)
  Notify-EnvironmentChanged
  $env:Path = (Split-Path -Parent $tesseract) + ';' + (Split-Path -Parent $ghostscript) + ';' + (Split-Path -Parent $ocrmypdf) + ';' + $env:Path
  Write-Host "OCR sidecars are installed and available on PATH."
  Write-Host "  Tesseract: $tesseract"
  Write-Host "  OCRmyPDF:  $ocrmypdf"
  Write-Host "  Ghostscript: $ghostscript"
  Write-Host "  Service PATH: registered Tesseract, Ghostscript, and OCRmyPDF directories at machine scope"
} finally {
  if ($transcriptStarted) { Stop-Transcript | Out-Null }
}
