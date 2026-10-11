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
  for candidate in "$REPO_ROOT/qindexer_linux_amd64" "$REPO_ROOT/outputs/bin/qindexer_linux_amd64" "$REPO_ROOT/src/dist/qindexer_linux_amd64"; do
    if [[ -f "$candidate" ]]; then BINARY_PATH="$candidate"; break; fi
  done
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
$SUDO install -d -m 0755 "$INSTALL_DIR/scripts"
$SUDO install -d -m 0700 "$INSTALL_DIR/configs" "$INSTALL_DIR/data" "$INSTALL_DIR/data/logs"
$SUDO install -m 0755 "$BINARY_PATH" "$INSTALL_DIR/bin/qindexer"
$SUDO install -m 0755 "$REPO_ROOT/scripts/setup-sidecars-linux.sh" "$INSTALL_DIR/scripts/setup-sidecars-linux.sh"

if [[ -n "$CONFIG_PATH" && -f "$CONFIG_PATH" ]]; then
  $SUDO install -m 0600 "$CONFIG_PATH" "$INSTALL_DIR/configs/qindexer.yaml"
elif [[ ! -f "$INSTALL_DIR/configs/qindexer.yaml" ]]; then
  for candidate in "$REPO_ROOT/configs/linux-nas.example.yaml" "$REPO_ROOT/src/configs/linux-nas.example.yaml"; do
    if [[ -f "$candidate" ]]; then
      $SUDO install -m 0600 "$candidate" "$INSTALL_DIR/configs/qindexer.yaml"
      break
    fi
  done
  [[ -f "$INSTALL_DIR/configs/qindexer.yaml" ]] || { echo "No Linux example config found in the release bundle or repository." >&2; exit 1; }
fi

if [[ "$INSTALL_SIDECARS" -eq 1 ]]; then
  $SUDO "$INSTALL_DIR/scripts/setup-sidecars-linux.sh"
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
