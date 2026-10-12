#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="/opt/qindexer"
CONFIG_DIR="/etc/qindexer"
DATA_DIR="/var/lib/qindexer"
SERVICE_NAME="qindexer"
BINARY_PATH=""
SIDECAR_MODE="prompt"

usage() {
  cat <<'USAGE'
Usage: setup-qindexer-service-linux.sh [options]

Installs and starts QIndexer as a native systemd service. A newly created
configuration has no roots and no token; complete first-run setup in the admin UI.

Options:
  --install-dir PATH    Binary installation directory, default /opt/qindexer
  --config-dir PATH     Configuration directory, default /etc/qindexer
  --data-dir PATH       Data directory, default /var/lib/qindexer
  --service-name NAME   systemd unit name, default qindexer
  --binary PATH         Source qindexer Linux binary
  --with-sidecars       Install OCR/extraction sidecars without prompting
  --without-sidecars    Do not install OCR/extraction sidecars
  -h, --help            Show this help
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --config-dir) CONFIG_DIR="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --service-name) SERVICE_NAME="$2"; shift 2 ;;
    --binary) BINARY_PATH="$2"; shift 2 ;;
    --with-sidecars) SIDECAR_MODE="yes"; shift ;;
    --without-sidecars) SIDECAR_MODE="no"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
if [[ -z "$BINARY_PATH" ]]; then
  for candidate in "$BUNDLE_ROOT/qindexer" "$BUNDLE_ROOT/qindexer_linux_amd64" "$BUNDLE_ROOT/src/dist/qindexer_linux_amd64"; do
    if [[ -f "$candidate" ]]; then BINARY_PATH="$candidate"; break; fi
  done
fi
[[ -n "$BINARY_PATH" && -f "$BINARY_PATH" ]] || { echo "QIndexer Linux binary not found. Pass --binary PATH." >&2; exit 1; }

SUDO=""
if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then SUDO="sudo"; fi

CONFIG_PATH="$CONFIG_DIR/qindexer.yaml"
LOG_DIR="$DATA_DIR/logs"
SIDECAR_SCRIPT="$INSTALL_DIR/scripts/setup-sidecars-linux.sh"
UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"

$SUDO install -d -m 0755 "$INSTALL_DIR" "$INSTALL_DIR/scripts"
$SUDO install -d -m 0700 "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"
$SUDO install -m 0755 "$BINARY_PATH" "$INSTALL_DIR/qindexer"
if [[ -f "$SCRIPT_DIR/setup-sidecars-linux.sh" ]]; then
  $SUDO install -m 0755 "$SCRIPT_DIR/setup-sidecars-linux.sh" "$SIDECAR_SCRIPT"
fi

if [[ ! -f "$CONFIG_PATH" ]]; then
  $SUDO tee "$CONFIG_PATH" >/dev/null <<CONFIG
server:
  bind: "127.0.0.1:41973"
  bind_addresses:
    - "127.0.0.1:41973"
  public_base_url: "http://127.0.0.1:41973"
  auth:
    mode: "static-token"

management:
  enabled: true
  bind: "127.0.0.1:41974"
  bind_addresses:
    - "127.0.0.1:41974"
  auth:
    mode: "admin-token"

index:
  data_dir: "$DATA_DIR"

roots: []
CONFIG
  $SUDO chmod 0600 "$CONFIG_PATH"
  echo "Created an empty configuration. Add roots and set the first admin token in the web UI."
fi

if [[ "$SIDECAR_MODE" == "prompt" && -t 0 ]]; then
  read -r -p "Install optional OCR/extraction sidecars now? [y/N] " response
  [[ "$response" =~ ^([yY]|[yY][eE][sS])$ ]] && SIDECAR_MODE="yes" || SIDECAR_MODE="no"
fi
if [[ "$SIDECAR_MODE" == "yes" ]]; then
  [[ -x "$SIDECAR_SCRIPT" ]] || { echo "Packaged Linux sidecar installer is missing: $SIDECAR_SCRIPT" >&2; exit 1; }
  $SUDO "$SIDECAR_SCRIPT"
fi

$SUDO tee "$UNIT_PATH" >/dev/null <<UNIT
[Unit]
Description=QIndexer filesystem indexing and search service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/qindexer run --config $CONFIG_PATH
Restart=on-failure
RestartSec=5s
LimitNOFILE=1048576
UMask=0077

[Install]
WantedBy=multi-user.target
UNIT

$SUDO systemctl daemon-reload
$SUDO systemctl enable "$SERVICE_NAME"
$SUDO systemctl restart "$SERVICE_NAME"
$SUDO systemctl --no-pager --full status "$SERVICE_NAME"

cat <<DONE
QIndexer service is running.
  Service: $SERVICE_NAME
  Admin UI: http://127.0.0.1:41974/
  Config: $CONFIG_PATH
  Logs: $LOG_DIR
DONE
