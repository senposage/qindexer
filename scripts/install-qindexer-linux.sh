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
POSTGRES_SETUP_FAILED=0
trap '[[ "$POSTGRES_SETUP_FAILED" -eq 1 ]] || report_progress failed install 100 "QIndexer installation failed."' ERR
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
  report_progress running install_elevation 8 "Requesting permission to install QIndexer."
  sudo -v
fi

$SUDO install -d -m 0755 "$INSTALL_DIR/bin"
$SUDO install -d -m 0755 "$INSTALL_DIR/scripts"
$SUDO install -d -m 0700 "$INSTALL_DIR/configs" "$INSTALL_DIR/data" "$INSTALL_DIR/data/logs"
$SUDO touch "$INSTALL_DIR/data/logs/install.log"
$SUDO chmod 0600 "$INSTALL_DIR/data/logs/install.log"
$SUDO install -m 0755 "$BINARY_PATH" "$INSTALL_DIR/bin/qindexer"
report_progress running install_files 20 "Installing QIndexer files."
$SUDO install -m 0755 "$REPO_ROOT/scripts/setup-sidecars-linux.sh" "$INSTALL_DIR/scripts/setup-sidecars-linux.sh"
$SUDO install -m 0755 "$REPO_ROOT/scripts/setup-postgres-linux.sh" "$INSTALL_DIR/scripts/setup-postgres-linux.sh"
$SUDO install -m 0755 "$REPO_ROOT/scripts/install-progress-linux.sh" "$INSTALL_DIR/scripts/install-progress-linux.sh"

INSTALL_LOG="$INSTALL_DIR/data/logs/install.log"
DATABASE_DSN=""
DATABASE_PRIVATE_DIR=""
DATABASE_COMMAND=""
DATABASE_USER=""

provision_database() {
  if ! DATABASE_JSON="$($SUDO "$INSTALL_DIR/scripts/setup-postgres-linux.sh" --install-dir "$INSTALL_DIR" --data-dir "$INSTALL_DIR/data" --progress-file "$PROGRESS_FILE" --log-file "$INSTALL_LOG")"; then
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
  $SUDO install -d -m 0700 "$INSTALL_DIR/configs/secrets"
  printf '%s\n' "$search_token" | $SUDO tee "$INSTALL_DIR/configs/secrets/search-api-token" >/dev/null
  printf '%s\n' "$admin_token" | $SUDO tee "$INSTALL_DIR/configs/secrets/admin-api-token" >/dev/null
  $SUDO chmod 0600 "$INSTALL_DIR/configs/secrets/search-api-token" "$INSTALL_DIR/configs/secrets/admin-api-token"
  $SUDO tee "$INSTALL_DIR/configs/qindexer.yaml" >/dev/null <<CONFIG
server:
  bind: "127.0.0.1:41973"
  bind_addresses:
    - "127.0.0.1:41973"
  public_base_url: "http://127.0.0.1:41973"
  auth:
    mode: "static-token"
    token_file: "secrets/search-api-token"

management:
  enabled: true
  bind: "127.0.0.1:41974"
  bind_addresses:
    - "127.0.0.1:41974"
  auth:
    mode: "admin-token"
    token_file: "secrets/admin-api-token"

index:
  data_dir: "$INSTALL_DIR/data"
  database:
    driver: "postgres"
    dsn: "$DATABASE_DSN"
    max_open_conns: 32
    max_idle_conns: 8
    private_postgres:
      data_dir: "$DATABASE_PRIVATE_DIR"
      command: "$DATABASE_COMMAND"
CONFIG
  if [[ -n "$DATABASE_USER" ]]; then
    printf '      user: "%s"\n' "$DATABASE_USER" | $SUDO tee -a "$INSTALL_DIR/configs/qindexer.yaml" >/dev/null
  fi
  $SUDO tee -a "$INSTALL_DIR/configs/qindexer.yaml" >/dev/null <<CONFIG

roots: []
CONFIG
  $SUDO chmod 0600 "$INSTALL_DIR/configs/qindexer.yaml"
}

upgrade_legacy_sqlite_config() {
  local backup temporary
  backup="$INSTALL_DIR/configs/qindexer.yaml.pre-postgres-$(date -u +%Y%m%dT%H%M%SZ).bak"
  temporary="$(mktemp)"
  $SUDO cp "$INSTALL_DIR/configs/qindexer.yaml" "$backup"
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
  ' "$INSTALL_DIR/configs/qindexer.yaml" > "$temporary" || { rm -f "$temporary"; echo "Cannot upgrade the legacy configuration because it has no top-level index section." >&2; exit 1; }
  $SUDO install -m 0600 "$temporary" "$INSTALL_DIR/configs/qindexer.yaml"
  rm -f "$temporary"
  echo "Upgraded the legacy SQLite-era configuration to PostgreSQL. Backup: $backup" >&2
}

add_private_postgres_config() {
  local temporary
  $SUDO grep -qE '^[[:space:]]+private_postgres:[[:space:]]*$' "$INSTALLED_CONFIG" && return
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
  ' "$INSTALLED_CONFIG" > "$temporary" || { rm -f "$temporary"; echo "Cannot add private PostgreSQL settings to the supplied configuration." >&2; exit 1; }
  $SUDO install -m 0600 "$temporary" "$INSTALLED_CONFIG"
  rm -f "$temporary"
}

if [[ -n "$CONFIG_PATH" && -f "$CONFIG_PATH" ]]; then
  $SUDO install -m 0600 "$CONFIG_PATH" "$INSTALL_DIR/configs/qindexer.yaml"
elif [[ ! -f "$INSTALL_DIR/configs/qindexer.yaml" ]]; then
  [[ "$SKIP_DATABASE" -eq 0 ]] || { echo "--skip-database requires --config when no QIndexer configuration exists." >&2; exit 2; }
  provision_database
  write_first_run_config
fi

INSTALLED_CONFIG="$INSTALL_DIR/configs/qindexer.yaml"
if $SUDO grep -Fq 'postgres://qindexer:change-me@' "$INSTALLED_CONFIG"; then
  [[ "$SKIP_DATABASE" -eq 0 ]] || { echo "The supplied configuration contains the PostgreSQL placeholder DSN, but --skip-database was requested." >&2; exit 2; }
  provision_database
  $SUDO sed -i "s|^    dsn:.*|    dsn: \"$DATABASE_DSN\"|" "$INSTALLED_CONFIG"
  add_private_postgres_config
elif ! $SUDO grep -qE '^[[:space:]]+database:[[:space:]]*$' "$INSTALLED_CONFIG"; then
  [[ "$SKIP_DATABASE" -eq 0 ]] || { echo "The existing configuration predates PostgreSQL support; remove --skip-database to upgrade it." >&2; exit 2; }
  report_progress running install_upgrade 70 "Upgrading the legacy SQLite-era configuration."
  provision_database
  upgrade_legacy_sqlite_config
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
