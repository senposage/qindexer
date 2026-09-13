param(
  [string]$Version = "1.0.1"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$dist = Join-Path $root "dist"
New-Item -ItemType Directory -Force $dist | Out-Null

$commit = "local"
if (Get-Command git -ErrorAction SilentlyContinue) {
  try { $commit = git -C $root rev-parse --short HEAD } catch { $commit = "local" }
}

$ldflags = "-s -w -X qindexer/internal/version.Version=$Version -X qindexer/internal/version.Commit=$commit"

$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags $ldflags -o (Join-Path $dist "qindexer_windows_amd64.exe") ./cmd/qindexer

$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -trimpath -ldflags $ldflags -o (Join-Path $dist "qindexer_linux_amd64") ./cmd/qindexer

Get-ChildItem $dist
