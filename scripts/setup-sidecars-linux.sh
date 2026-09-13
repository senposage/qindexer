#!/usr/bin/env bash
set -euo pipefail

# Installs the optional OCR/extraction sidecars used by QIndexer. It does not
# install QIndexer, modify its configuration, or assume a particular distro.

SUDO=""
if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  SUDO="sudo"
fi

install_with() {
  case "$1" in
    dnf)
      $SUDO dnf install -y tesseract tesseract-langpack-eng ocrmypdf ghostscript poppler-utils
      ;;
    apt-get)
      $SUDO apt-get update
      $SUDO apt-get install -y tesseract-ocr tesseract-ocr-eng ocrmypdf ghostscript poppler-utils
      ;;
    zypper)
      $SUDO zypper --non-interactive install tesseract-ocr tesseract-ocr-traineddata-english ocrmypdf ghostscript poppler-tools
      ;;
    pacman)
      $SUDO pacman -Sy --needed --noconfirm tesseract tesseract-data-eng ocrmypdf ghostscript poppler
      ;;
    brew)
      brew install tesseract ocrmypdf ghostscript poppler
      ;;
  esac
}

manager=""
for candidate in dnf apt-get zypper pacman brew; do
  if command -v "$candidate" >/dev/null 2>&1; then
    manager="$candidate"
    break
  fi
done

if [[ -z "$manager" ]]; then
  echo "No supported package manager found." >&2
  echo "Install: tesseract, ocrmypdf, ghostscript, and poppler (pdftotext)." >&2
  exit 1
fi

echo "Installing QIndexer OCR/extraction sidecars with $manager..."
install_with "$manager"

missing=()
command -v tesseract >/dev/null 2>&1 || missing+=("tesseract")
command -v ocrmypdf >/dev/null 2>&1 || missing+=("ocrmypdf")
command -v gs >/dev/null 2>&1 || missing+=("ghostscript (gs)")
command -v pdftotext >/dev/null 2>&1 || missing+=("poppler (pdftotext)")

if [[ ${#missing[@]} -gt 0 ]]; then
  printf 'Installation completed, but these commands are still unavailable: %s\n' "${missing[*]}" >&2
  exit 1
fi

echo "Ready: tesseract, ocrmypdf, gs, and pdftotext are available on PATH."
