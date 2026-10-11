#!/usr/bin/env bash
set -euo pipefail

# Installs optional OCR/extraction sidecars and can record their resolved paths
# in a QIndexer config. It does not install QIndexer itself.

CONFIG_PATH=""
ENABLE_OCR=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --config) CONFIG_PATH="$2"; shift 2 ;;
    --enable-ocr) ENABLE_OCR=1; shift ;;
    -h|--help)
      cat <<'USAGE'
Usage: setup-sidecars-linux.sh [options]

Options:
  --config PATH     Update crawler.ocr command paths in this config.
  --enable-ocr      Also enable global OCR in the supplied config.
USAGE
      exit 0
      ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

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

echo "Ready: tesseract, ocrmypdf, gs, and pdftotext are available on the system PATH."

if [[ -n "$CONFIG_PATH" ]]; then
  if [[ ! -f "$CONFIG_PATH" ]]; then
    echo "Config not found: $CONFIG_PATH" >&2
    exit 1
  fi
  tesseract_path="$(command -v tesseract)"
  ocrmypdf_path="$(command -v ocrmypdf)"
  temp_config="$(mktemp "${CONFIG_PATH}.sidecars.XXXXXX")"
  trap 'rm -f "$temp_config"' EXIT
  awk -v tesseract="$tesseract_path" -v ocrmypdf="$ocrmypdf_path" -v enable="$ENABLE_OCR" '
    /^  ocr:[[:space:]]*$/ { in_ocr=1; print; next }
    in_ocr && /^  [A-Za-z_][A-Za-z0-9_]*:[[:space:]]*$/ { in_ocr=0 }
    in_ocr && /^    tesseract_command:/ { print "    tesseract_command: \"" tesseract "\""; changed=1; next }
    in_ocr && /^    ocrmypdf_command:/ { print "    ocrmypdf_command: \"" ocrmypdf "\""; changed=1; next }
    in_ocr && enable == "1" && /^    enabled:/ { print "    enabled: true"; changed=1; next }
    { print }
    END { if (!changed) exit 20 }
  ' "$CONFIG_PATH" > "$temp_config" || {
    echo "Could not find the global crawler.ocr block in $CONFIG_PATH" >&2
    exit 1
  }
  chmod --reference="$CONFIG_PATH" "$temp_config" 2>/dev/null || true
  mv "$temp_config" "$CONFIG_PATH"
  trap - EXIT
  echo "Configured OCR command paths in $CONFIG_PATH"
  if [[ "$ENABLE_OCR" -ne 1 ]]; then
    echo "OCR remains disabled. Re-run with --enable-ocr when you are ready to process the backlog."
  fi
fi
