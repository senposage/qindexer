#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="/opt/qindexer"
CONFIG_PATH=""
BINARY_PATH=""
INSTALL_SIDECARS=0
INSTALL_SERVICE=0
START_SERVICE=0
SERVICE_NAME="qindexer"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --config) CONFIG_PATH="$2"; shift 2 ;;
    --binary) BINARY_PATH="$2"; shift 2 ;;
    --install-sidecars) INSTALL_SIDECARS=1; shift ;;
    --install-service) INSTALL_SERVICE=1; shift ;;
    --start-service) START_SERVICE=1; shift ;;
    --service-name) SERVICE_NAME="$2"; shift 2 ;;
    -h|--help)
      cat <<'USAGE'
Usage: install-qindexer-linux.sh [options]

Options:
  --install-dir PATH       Install directory, default /opt/qindexer
  --config PATH            Config file to install as configs/qindexer.yaml
  --binary PATH            qindexer Linux binary, default outputs/bin/qindexer_linux_amd64
  --install-sidecars       Install tesseract, ocrmypdf, ghostscript, poppler-utils when possible
  --install-service        Install a systemd service
  --start-service          Start the systemd service after install
  --service-name NAME      systemd service name, default qindexer
USAGE
      exit 0
      ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
if [[ -z "$BINARY_PATH" ]]; then
  BINARY_PATH="$REPO_ROOT/outputs/bin/qindexer_linux_amd64"
fi
if [[ ! -f "$BINARY_PATH" ]]; then
  echo "QIndexer binary not found: $BINARY_PATH" >&2
  exit 1
fi

SUDO=""
if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  SUDO="sudo"
fi

$SUDO install -d -m 0755 "$INSTALL_DIR/bin"
$SUDO install -d -m 0700 "$INSTALL_DIR/configs" "$INSTALL_DIR/data" "$INSTALL_DIR/data/logs"
$SUDO install -m 0755 "$BINARY_PATH" "$INSTALL_DIR/bin/qindexer"

if [[ -n "$CONFIG_PATH" && -f "$CONFIG_PATH" ]]; then
  $SUDO install -m 0600 "$CONFIG_PATH" "$INSTALL_DIR/configs/qindexer.yaml"
elif [[ ! -f "$INSTALL_DIR/configs/qindexer.yaml" ]]; then
  $SUDO install -m 0600 "$REPO_ROOT/src/configs/linux-nas.example.yaml" "$INSTALL_DIR/configs/qindexer.yaml"
fi

if [[ "$INSTALL_SIDECARS" -eq 1 ]]; then
  SIDECAR_INSTALLED=0
  if command -v dnf >/dev/null 2>&1; then
    $SUDO dnf install -y tesseract tesseract-langpack-eng ocrmypdf ghostscript poppler-utils python3-pip || true
    SIDECAR_INSTALLED=1
  elif command -v apt-get >/dev/null 2>&1; then
    $SUDO apt-get update
    $SUDO apt-get install -y tesseract-ocr tesseract-ocr-eng ocrmypdf ghostscript poppler-utils python3-pip || true
    SIDECAR_INSTALLED=1
  elif command -v zypper >/dev/null 2>&1; then
    $SUDO zypper --non-interactive install tesseract-ocr tesseract-ocr-traineddata-english ocrmypdf ghostscript poppler-tools python3-pip || true
    SIDECAR_INSTALLED=1
  elif command -v pacman >/dev/null 2>&1; then
    $SUDO pacman -Sy --needed --noconfirm tesseract tesseract-data-eng ocrmypdf ghostscript poppler python-pip || true
    SIDECAR_INSTALLED=1
  elif command -v brew >/dev/null 2>&1; then
    brew install tesseract ocrmypdf ghostscript poppler || true
    SIDECAR_INSTALLED=1
  else
    echo "No supported package manager found; install tesseract, ocrmypdf, ghostscript, poppler-utils/poppler-tools, and python3-pip manually." >&2
  fi
  if ! command -v ocrmypdf >/dev/null 2>&1 && command -v python3 >/dev/null 2>&1; then
    python3 -m pip install --user --upgrade ocrmypdf || true
  fi
  MISSING=()
  command -v tesseract >/dev/null 2>&1 || MISSING+=("tesseract")
  command -v ocrmypdf >/dev/null 2>&1 || MISSING+=("ocrmypdf")
  command -v gs >/dev/null 2>&1 || MISSING+=("ghostscript/gs")
  command -v pdftotext >/dev/null 2>&1 || MISSING+=("poppler/pdftotext")
  if [[ "${#MISSING[@]}" -gt 0 ]]; then
    echo "Sidecar install completed with missing commands: ${MISSING[*]}" >&2
    echo "Install those with your distro package manager or set explicit commands in qindexer.yaml." >&2
  elif [[ "$SIDECAR_INSTALLED" -eq 1 ]]; then
    echo "OCR/content sidecars verified: tesseract, ocrmypdf, gs, pdftotext"
  fi
fi

if [[ "$INSTALL_SERVICE" -eq 1 ]]; then
  SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
  $SUDO tee "$SERVICE_FILE" >/dev/null <<SERVICE
[Unit]
Description=QIndexer Search Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/bin/qindexer run --config $INSTALL_DIR/configs/qindexer.yaml
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
SERVICE
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable "$SERVICE_NAME"
  if [[ "$START_SERVICE" -eq 1 ]]; then
    $SUDO systemctl restart "$SERVICE_NAME"
  fi
fi

cat <<DONE
QIndexer installed
  Binary: $INSTALL_DIR/bin/qindexer
  Config: $INSTALL_DIR/configs/qindexer.yaml
  Data:   $INSTALL_DIR/data
  Logs:   $INSTALL_DIR/data/logs/qindexer.log
DONE
