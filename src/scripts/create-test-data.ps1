param(
  [string]$Destination = "D:\qindexer",
  [switch]$Force
)

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Drawing

function Write-Bytes([string]$Path, [byte[]]$Bytes) {
  [IO.Directory]::CreateDirectory((Split-Path -Parent $Path)) | Out-Null
  [IO.File]::WriteAllBytes($Path, $Bytes)
}

function Write-Pdf([string]$Path, [byte[][]]$Objects) {
  $ascii = [Text.Encoding]::ASCII
  $stream = [IO.MemoryStream]::new()
  $header = $ascii.GetBytes("%PDF-1.4`n%QIDX`n")
  $stream.Write($header, 0, $header.Length)
  $offsets = [Collections.Generic.List[int]]::new()
  for ($i = 0; $i -lt $Objects.Count; $i++) {
    $offsets.Add([int]$stream.Position)
    $prefix = $ascii.GetBytes("$($i + 1) 0 obj`n")
    $stream.Write($prefix, 0, $prefix.Length)
    $stream.Write($Objects[$i], 0, $Objects[$i].Length)
    $suffix = $ascii.GetBytes("`nendobj`n")
    $stream.Write($suffix, 0, $suffix.Length)
  }
  $xref = [int]$stream.Position
  $table = "xref`n0 $($Objects.Count + 1)`n0000000000 65535 f `n"
  foreach ($offset in $offsets) { $table += ("{0:D10} 00000 n `n" -f $offset) }
  $tail = $table + "trailer`n<< /Size $($Objects.Count + 1) /Root 1 0 R >>`nstartxref`n$xref`n%%EOF`n"
  $bytes = $ascii.GetBytes($tail)
  $stream.Write($bytes, 0, $bytes.Length)
  Write-Bytes $Path $stream.ToArray()
  $stream.Dispose()
}

function New-TextPdf([string]$Path, [string]$Text) {
  $escaped = $Text.Replace("\", "\\").Replace("(", "\(").Replace(")", "\)")
  $content = "BT /F1 18 Tf 72 720 Td ($escaped) Tj ET"
  $ascii = [Text.Encoding]::ASCII
  [byte[][]]$objects = @(
    $ascii.GetBytes("<< /Type /Catalog /Pages 2 0 R >>"),
    $ascii.GetBytes("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
    $ascii.GetBytes("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>"),
    $ascii.GetBytes("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
    $ascii.GetBytes("<< /Length $($ascii.GetByteCount($content)) >>`nstream`n$content`nendstream")
  )
  Write-Pdf $Path $objects
}

function New-ScannedPdf([string]$Path, [byte[]]$Jpeg, [int]$Width, [int]$Height) {
  $ascii = [Text.Encoding]::ASCII
  $imageHeader = $ascii.GetBytes("<< /Type /XObject /Subtype /Image /Width $Width /Height $Height /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length $($Jpeg.Length) >>`nstream`n")
  $imageFooter = $ascii.GetBytes("`nendstream")
  $image = [byte[]]::new($imageHeader.Length + $Jpeg.Length + $imageFooter.Length)
  [Array]::Copy($imageHeader, 0, $image, 0, $imageHeader.Length)
  [Array]::Copy($Jpeg, 0, $image, $imageHeader.Length, $Jpeg.Length)
  [Array]::Copy($imageFooter, 0, $image, $imageHeader.Length + $Jpeg.Length, $imageFooter.Length)
  $draw = "q 612 0 0 792 0 0 cm /Im0 Do Q"
  [byte[][]]$objects = @(
    $ascii.GetBytes("<< /Type /Catalog /Pages 2 0 R >>"),
    $ascii.GetBytes("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
    $ascii.GetBytes("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>"),
    $image,
    $ascii.GetBytes("<< /Length $($ascii.GetByteCount($draw)) >>`nstream`n$draw`nendstream")
  )
  Write-Pdf $Path $objects
}

function New-OCRImage([string]$Path, [string]$Text) {
  $bitmap = [Drawing.Bitmap]::new(1800, 500)
  $graphics = [Drawing.Graphics]::FromImage($bitmap)
  $graphics.Clear([Drawing.Color]::White)
  $font = [Drawing.Font]::new("Arial", 42, [Drawing.FontStyle]::Bold)
  $brush = [Drawing.SolidBrush]::new([Drawing.Color]::Black)
  $graphics.DrawString($Text, $font, $brush, 55, 190)
  $bitmap.Save($Path, [Drawing.Imaging.ImageFormat]::Png)
  $jpegStream = [IO.MemoryStream]::new()
  $bitmap.Save($jpegStream, [Drawing.Imaging.ImageFormat]::Jpeg)
  $brush.Dispose(); $font.Dispose(); $graphics.Dispose(); $bitmap.Dispose()
  $bytes = $jpegStream.ToArray(); $jpegStream.Dispose()
  return ,$bytes
}

if (Test-Path -LiteralPath $Destination) {
  if (-not $Force) { throw "$Destination already exists. Re-run with -Force to replace only this synthetic corpus." }
  Remove-Item -LiteralPath $Destination -Recurse -Force
}
@("content", "office", "pdf", "ocr", "nested\project-alpha") | ForEach-Object {
  New-Item -ItemType Directory -Force (Join-Path $Destination $_) | Out-Null
}

[IO.File]::WriteAllText((Join-Path $Destination "content\plain-notes.txt"), "QINDEXER TEXT ORCHID 731`nThis phrase validates content search from a text file.")
[IO.File]::WriteAllText((Join-Path $Destination "nested\project-alpha\path-note.md"), "QINDEXER PATH CEDAR 219")

$stage = Join-Path $env:TEMP ("qindexer-office-" + [guid]::NewGuid())
New-Item -ItemType Directory -Force $stage | Out-Null
try {
  @{
    "word\document.xml" = "<document><body><p>QINDEXER DOCX VIOLET 317</p></body></document>"
    "xl\sharedStrings.xml" = "<sst><si><t>QINDEXER XLSX COBALT 428</t></si></sst>"
    "ppt\slides\slide1.xml" = "<slide><text>QINDEXER PPTX AMBER 539</text></slide>"
  }.GetEnumerator() | ForEach-Object {
    $file = Join-Path $stage $_.Key
    [IO.Directory]::CreateDirectory((Split-Path -Parent $file)) | Out-Null
    [IO.File]::WriteAllText($file, $_.Value)
  }
  Compress-Archive -Path (Join-Path $stage "word") -DestinationPath (Join-Path $Destination "office\sample.docx")
  Compress-Archive -Path (Join-Path $stage "xl") -DestinationPath (Join-Path $Destination "office\sample.xlsx")
  Compress-Archive -Path (Join-Path $stage "ppt") -DestinationPath (Join-Path $Destination "office\sample.pptx")
} finally { Remove-Item -LiteralPath $stage -Recurse -Force }

New-TextPdf (Join-Path $Destination "pdf\embedded-text.pdf") "QINDEXER PDF EMBER 514"
$ocrText = "QINDEXER OCR AURORA 842"
$jpeg = New-OCRImage (Join-Path $Destination "ocr\receipt-ocr.png") $ocrText
New-ScannedPdf (Join-Path $Destination "ocr\scanned-invoice.pdf") $jpeg 1800 500

[IO.File]::WriteAllText((Join-Path $Destination "README.txt"), @"
QIndexer synthetic validation corpus.
Unique search markers: ORCHID 731, VIOLET 317, COBALT 428, AMBER 539, EMBER 514, AURORA 842, CEDAR 219.
"@)

Write-Host "Created QIndexer test corpus at $Destination"
