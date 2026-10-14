#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="/opt/qindexer"
CONFIG_PATH=""
BINARY_PATH=""
SKIP_DATABASE=0
INSTALL_SIDECARS=0
INSTALL_SERVICE=0
START_SERVICE=0
SERVICE_NAME="qindexer"
PROGRESS_FILE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --config) CONFIG_PATH="$2"; shift 2 ;;
    --binary) BINARY_PATH="$2"; shift 2 ;;
    --skip-database) SKIP_DATABASE=1; shift ;;
    --install-sidecars) INSTALL_SIDECARS=1; shift ;;
    --install-service) INSTALL_SERVICE=1; shift ;;
    --start-service) START_SERVICE=1; shift ;;
    --service-name) SERVICE_NAME="$2"; shift 2 ;;
    --progress-file) PROGRESS_FILE="$2"; shift 2 ;;
    -h|--help)
      cat <<'USAGE'
Usage: install-qindexer-linux.sh [options]

Options:
  --install-dir PATH       Install directory, default /opt/qindexer
  --config PATH            Config file to install as configs/qindexer.yaml
  --binary PATH            qindexer Linux binary, default outputs/bin/qindexer_linux_amd64
  --skip-database          Require --config; do not provision local PostgreSQL
  --install-sidecars       Install tesseract, ocrmypdf, ghostscript, poppler-utils when possible
  --install-service        Install a systemd service
  --start-service          Start the systemd service after install
  --service-name NAME      systemd service name, default qindexer
  --progress-file PATH     Write atomic installation progress JSON to PATH
USAGE
      exit 0
      ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/install-progress-linux.sh"
report_progress() {
  write_qindexer_install_progress "$PROGRESS_FILE" "$1" "$2" "$3" "$4"
}
trap 'report_progress failed install 100 "QIndexer installation failed."' ERR
report_progress running install_prepare 5 "Preparing the QIndexer installation."
if [[ -z "$BINARY_PATH" ]]; then
  for candidate in "$REPO_ROOT/qindexer" "$REPO_ROOT/qindexer_linux_amd64" "$REPO_ROOT/outputs/bin/qindexer_linux_amd64" "$REPO_ROOT/src/dist/qindexer_linux_amd64"; do
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
report_progress running install_files 20 "Installing QIndexer files."
$SUDO install -m 0755 "$REPO_ROOT/scripts/setup-sidecars-linux.sh" "$INSTALL_DIR/scripts/setup-sidecars-linux.sh"
$SUDO install -m 0755 "$REPO_ROOT/scripts/setup-postgres-linux.sh" "$INSTALL_DIR/scripts/setup-postgres-linux.sh"
$SUDO install -m 0755 "$REPO_ROOT/scripts/install-progress-linux.sh" "$INSTALL_DIR/scripts/install-progress-linux.sh"

if [[ -n "$CONFIG_PATH" && -f "$CONFIG_PATH" ]]; then
  $SUDO install -m 0600 "$CONFIG_PATH" "$INSTALL_DIR/configs/qindexer.yaml"
elif [[ ! -f "$INSTALL_DIR/configs/qindexer.yaml" ]]; then
  [[ "$SKIP_DATABASE" -eq 0 ]] || { echo "--skip-database requires --config when no QIndexer configuration exists." >&2; exit 2; }
  DATABASE_JSON="$($SUDO "$INSTALL_DIR/scripts/setup-postgres-linux.sh" --install-dir "$INSTALL_DIR" --data-dir "$INSTALL_DIR/data" --progress-file "$PROGRESS_FILE")"
  DATABASE_DSN="$(printf '%s' "$DATABASE_JSON" | sed -n 's/.*"dsn":"\([^"]*\)".*/\1/p')"
  [[ -n "$DATABASE_DSN" ]] || { echo "PostgreSQL setup did not return a QIndexer DSN." >&2; exit 1; }
  for candidate in "$REPO_ROOT/configs/linux-nas.example.yaml" "$REPO_ROOT/src/configs/linux-nas.example.yaml"; do
    if [[ -f "$candidate" ]]; then
      $SUDO install -m 0600 "$candidate" "$INSTALL_DIR/configs/qindexer.yaml"
      break
    fi
  done
  [[ -f "$INSTALL_DIR/configs/qindexer.yaml" ]] || { echo "No Linux example config found in the release bundle or repository." >&2; exit 1; }
  $SUDO sed -i "s|^    dsn:.*|    dsn: \"$DATABASE_DSN\"|" "$INSTALL_DIR/configs/qindexer.yaml"
fi

INSTALLED_CONFIG="$INSTALL_DIR/configs/qindexer.yaml"
if $SUDO grep -Fq 'postgres://qindexer:change-me@' "$INSTALLED_CONFIG"; then
  [[ "$SKIP_DATABASE" -eq 0 ]] || { echo "The supplied configuration contains the PostgreSQL placeholder DSN, but --skip-database was requested." >&2; exit 2; }
  DATABASE_JSON="$($SUDO "$INSTALL_DIR/scripts/setup-postgres-linux.sh" --install-dir "$INSTALL_DIR" --data-dir "$INSTALL_DIR/data" --progress-file "$PROGRESS_FILE")"
  DATABASE_DSN="$(printf '%s' "$DATABASE_JSON" | sed -n 's/.*"dsn":"\([^"]*\)".*/\1/p')"
  [[ -n "$DATABASE_DSN" ]] || { echo "PostgreSQL setup did not return a QIndexer DSN." >&2; exit 1; }
  $SUDO sed -i "s|^    dsn:.*|    dsn: \"$DATABASE_DSN\"|" "$INSTALLED_CONFIG"
fi

if [[ "$INSTALL_SIDECARS" -eq 1 ]]; then
  report_progress running install_sidecars 88 "Installing optional extraction sidecars."
  $SUDO "$INSTALL_DIR/scripts/setup-sidecars-linux.sh"
fi

if [[ "$INSTALL_SERVICE" -eq 1 ]]; then
  report_progress running install_service 92 "Installing the QIndexer service."
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

report_progress completed install_ready 100 "QIndexer installation is complete."
cat <<DONE
QIndexer installed
  Binary: $INSTALL_DIR/bin/qindexer
  Config: $INSTALL_DIR/configs/qindexer.yaml
  Data:   $INSTALL_DIR/data
  Logs:   $INSTALL_DIR/data/logs/qindexer.log
DONE
