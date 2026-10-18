#!/usr/bin/env -S bash
set -euo pipefail

INSTALL_DIR="/opt/qindexer"
CONFIG_DIR="/etc/qindexer"
DATA_DIR="/var/lib/qindexer"
SERVICE_NAME="qindexer"
BINARY_PATH=""
SIDECAR_MODE="prompt"
SKIP_DATABASE=0
PROGRESS_FILE=""

usage() {
  cat <<'USAGE'
Usage: setup-qindexer-service-linux.sh [options]

Installs and starts QIndexer as a native systemd service. A newly created
configuration has no roots and has generated local search and admin tokens.

Options:
  --install-dir PATH    Binary installation directory, default /opt/qindexer
  --config-dir PATH     Configuration directory, default /etc/qindexer
  --data-dir PATH       Data directory, default /var/lib/qindexer
  --service-name NAME   systemd unit name, default qindexer
  --binary PATH         Source qindexer Linux binary
  --with-sidecars       Install OCR/extraction sidecars without prompting
  --without-sidecars    Do not install OCR/extraction sidecars
  --skip-database       Require an existing configuration; do not provision PostgreSQL
  --progress-file PATH  Write atomic installation progress JSON to PATH
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
    --skip-database) SKIP_DATABASE=1; shift ;;
    --progress-file) PROGRESS_FILE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BUNDLE_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/install-progress-linux.sh"
report_progress() {
  write_qindexer_install_progress "$PROGRESS_FILE" "$1" "$2" "$3" "$4"
}
POSTGRES_SETUP_FAILED=0
trap '[[ "$POSTGRES_SETUP_FAILED" -eq 1 ]] || report_progress failed service_install 100 "QIndexer service installation failed."' ERR
report_progress running service_prepare 5 "Preparing the QIndexer service installation."
if [[ -z "$BINARY_PATH" ]]; then
  for candidate in "$BUNDLE_ROOT/qindexer" "$BUNDLE_ROOT/qindexer_linux_amd64" "$BUNDLE_ROOT/src/dist/qindexer_linux_amd64"; do
    if [[ -f "$candidate" ]]; then BINARY_PATH="$candidate"; break; fi
  done
fi
[[ -n "$BINARY_PATH" && -f "$BINARY_PATH" ]] || { echo "QIndexer Linux binary not found. Pass --binary PATH." >&2; exit 1; }

SUDO=""
if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  SUDO="sudo"
  report_progress running service_elevation 8 "Requesting permission to install the QIndexer service."
  sudo -v
fi

CONFIG_PATH="$CONFIG_DIR/qindexer.yaml"
LOG_DIR="$DATA_DIR/logs"
SIDECAR_SCRIPT="$INSTALL_DIR/scripts/setup-sidecars-linux.sh"
POSTGRES_SCRIPT="$INSTALL_DIR/scripts/setup-postgres-linux.sh"
UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"

$SUDO install -d -m 0755 "$INSTALL_DIR" "$INSTALL_DIR/scripts"
$SUDO install -d -m 0700 "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR"
$SUDO touch "$LOG_DIR/install.log"
$SUDO chmod 0600 "$LOG_DIR/install.log"
$SUDO install -m 0755 "$BINARY_PATH" "$INSTALL_DIR/qindexer"
report_progress running service_files 20 "Installing QIndexer service files."
if [[ -f "$SCRIPT_DIR/setup-sidecars-linux.sh" ]]; then
  $SUDO install -m 0755 "$SCRIPT_DIR/setup-sidecars-linux.sh" "$SIDECAR_SCRIPT"
fi
if [[ -f "$SCRIPT_DIR/setup-postgres-linux.sh" ]]; then
  $SUDO install -m 0755 "$SCRIPT_DIR/setup-postgres-linux.sh" "$POSTGRES_SCRIPT"
fi
if [[ -f "$SCRIPT_DIR/install-progress-linux.sh" ]]; then
  $SUDO install -m 0755 "$SCRIPT_DIR/install-progress-linux.sh" "$INSTALL_DIR/scripts/install-progress-linux.sh"
fi

INSTALL_LOG="$LOG_DIR/install.log"
DATABASE_DSN=""
DATABASE_PRIVATE_DIR=""
DATABASE_COMMAND=""
DATABASE_USER=""

provision_database() {
  if ! DATABASE_JSON="$($SUDO "$POSTGRES_SCRIPT" --install-dir "$INSTALL_DIR" --data-dir "$DATA_DIR" --progress-file "$PROGRESS_FILE" --log-file "$INSTALL_LOG")"; then
    POSTGRES_SETUP_FAILED=1
    return 1
  fi
  DATABASE_DSN="$(printf '%s' "$DATABASE_JSON" | sed -n 's/.*"dsn":"\([^"]*\)".*/\1/p')"
  DATABASE_PRIVATE_DIR="$(printf '%s' "$DATABASE_JSON" | sed -n 's/.*"private_data_dir":"\([^"]*\)".*/\1/p')"
  DATABASE_COMMAND="$(printf '%s' "$DATABASE_JSON" | sed -n 's/.*"postgres_command":"\([^"]*\)".*/\1/p')"
  DATABASE_USER="$(printf '%s' "$DATABASE_JSON" | sed -n 's/.*"private_user":"\([^"]*\)".*/\1/p')"
  [[ -n "$DATABASE_DSN" && -n "$DATABASE_PRIVATE_DIR" && -n "$DATABASE_COMMAND" ]] || { echo "PostgreSQL setup did not return a complete QIndexer database configuration." >&2; exit 1; }
}

write_first_run_config() {
  local search_token admin_token
  search_token="$(head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9_-' | cut -c1-40)"
  admin_token="$(head -c 48 /dev/urandom | base64 | tr -dc 'A-Za-z0-9_-' | cut -c1-40)"
  $SUDO tee "$CONFIG_PATH" >/dev/null <<CONFIG
server:
  bind: "127.0.0.1:41973"
  bind_addresses:
    - "127.0.0.1:41973"
  public_base_url: "http://127.0.0.1:41973"
  auth:
    mode: "static-token"
    token: "$search_token"

management:
  enabled: true
  bind: "127.0.0.1:41974"
  bind_addresses:
    - "127.0.0.1:41974"
  auth:
    mode: "admin-token"
    token: "$admin_token"

index:
  data_dir: "$DATA_DIR"
  database:
    driver: "postgres"
    dsn: "$DATABASE_DSN"
    max_open_conns: 32
    max_idle_conns: 8
    private_postgres:
      data_dir: "$DATABASE_PRIVATE_DIR"
      command: "$DATABASE_COMMAND"
CONFIG
  if [[ -n "$DATABASE_USER" ]]; then printf '      user: "%s"\n' "$DATABASE_USER" | $SUDO tee -a "$CONFIG_PATH" >/dev/null; fi
  $SUDO tee -a "$CONFIG_PATH" >/dev/null <<CONFIG

roots: []
CONFIG
  $SUDO chmod 0600 "$CONFIG_PATH"
}

upgrade_legacy_sqlite_config() {
  local backup temporary
  backup="$CONFIG_PATH.pre-postgres-$(date -u +%Y%m%dT%H%M%SZ).bak"
  temporary="$(mktemp)"
  $SUDO cp "$CONFIG_PATH" "$backup"
  $SUDO awk -v dsn="$DATABASE_DSN" -v data_dir="$DATABASE_PRIVATE_DIR" -v command="$DATABASE_COMMAND" -v user="$DATABASE_USER" '
    /^index:[[:space:]]*$/ && !inserted {
      print
      print "  database:"
      print "    driver: \"postgres\""
      print "    dsn: \"" dsn "\""
      print "    max_open_conns: 32"
      print "    max_idle_conns: 8"
      print "    private_postgres:"
      print "      data_dir: \"" data_dir "\""
      print "      command: \"" command "\""
      if (user != "") print "      user: \"" user "\""
      inserted = 1
      next
    }
    { print }
    END { if (!inserted) exit 42 }
  ' "$CONFIG_PATH" > "$temporary" || { rm -f "$temporary"; echo "Cannot upgrade the legacy configuration because it has no top-level index section." >&2; exit 1; }
  $SUDO install -m 0600 "$temporary" "$CONFIG_PATH"
  rm -f "$temporary"
  echo "Upgraded the legacy SQLite-era configuration to PostgreSQL. Backup: $backup" >&2
}

add_private_postgres_config() {
  local temporary
  $SUDO grep -qE '^[[:space:]]+private_postgres:[[:space:]]*$' "$CONFIG_PATH" && return
  temporary="$(mktemp)"
  $SUDO awk -v data_dir="$DATABASE_PRIVATE_DIR" -v command="$DATABASE_COMMAND" -v user="$DATABASE_USER" '
    /^[[:space:]]+dsn:[[:space:]]*/ && !inserted {
      print
      print "    private_postgres:"
      print "      data_dir: \"" data_dir "\""
      print "      command: \"" command "\""
      if (user != "") print "      user: \"" user "\""
      inserted = 1
      next
    }
    { print }
    END { if (!inserted) exit 42 }
  ' "$CONFIG_PATH" > "$temporary" || { rm -f "$temporary"; echo "Cannot add private PostgreSQL settings to the supplied configuration." >&2; exit 1; }
  $SUDO install -m 0600 "$temporary" "$CONFIG_PATH"
  rm -f "$temporary"
}

if [[ ! -f "$CONFIG_PATH" ]]; then
  [[ "$SKIP_DATABASE" -eq 0 ]] || { echo "--skip-database requires an existing configuration with an explicit database backend." >&2; exit 2; }
  [[ -x "$POSTGRES_SCRIPT" ]] || { echo "Packaged PostgreSQL setup script is missing: $POSTGRES_SCRIPT" >&2; exit 1; }
  provision_database
  write_first_run_config
  echo "Created an empty configuration with generated local tokens."
fi
if $SUDO grep -Fq 'postgres://qindexer:change-me@' "$CONFIG_PATH"; then
  [[ "$SKIP_DATABASE" -eq 0 ]] || { echo "The existing configuration contains the PostgreSQL placeholder DSN, but --skip-database was requested." >&2; exit 2; }
  [[ -x "$POSTGRES_SCRIPT" ]] || { echo "Packaged PostgreSQL setup script is missing: $POSTGRES_SCRIPT" >&2; exit 1; }
  provision_database
  $SUDO sed -i "s|^    dsn:.*|    dsn: \"$DATABASE_DSN\"|" "$CONFIG_PATH"
  add_private_postgres_config
elif ! $SUDO grep -qE '^[[:space:]]+database:[[:space:]]*$' "$CONFIG_PATH"; then
  [[ "$SKIP_DATABASE" -eq 0 ]] || { echo "The existing configuration predates PostgreSQL support; remove --skip-database to upgrade it." >&2; exit 2; }
  report_progress running service_upgrade 70 "Upgrading the legacy SQLite-era configuration."
  provision_database
  upgrade_legacy_sqlite_config
fi

if [[ "$SIDECAR_MODE" == "prompt" && -t 0 ]]; then
  read -r -p "Install optional OCR/extraction sidecars now? [y/N] " response
  [[ "$response" =~ ^([yY]|[yY][eE][sS])$ ]] && SIDECAR_MODE="yes" || SIDECAR_MODE="no"
fi
if [[ "$SIDECAR_MODE" == "yes" ]]; then
  report_progress running service_sidecars 85 "Installing optional extraction sidecars."
  [[ -x "$SIDECAR_SCRIPT" ]] || { echo "Packaged Linux sidecar installer is missing: $SIDECAR_SCRIPT" >&2; exit 1; }
  $SUDO "$SIDECAR_SCRIPT"
fi

report_progress running service_register 92 "Registering the QIndexer service."
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

report_progress completed service_ready 100 "QIndexer service is running."
cat <<DONE
QIndexer service is running.
  Service: $SERVICE_NAME
  Admin UI: http://127.0.0.1:41974/
  Config: $CONFIG_PATH
  Logs: $LOG_DIR
DONE
