#!/usr/bin/env -S bash
set -euo pipefail

INSTALL_DIR="/opt/qindexer"
DATA_DIR="/var/lib/qindexer"
DATABASE_NAME="qindexer"
ROLE_NAME="qindexer"
PORT=""
PROGRESS_PATH=""
LOG_PATH=""
RESET_INCOMPLETE=0

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/install-progress-linux.sh"

usage() {
  cat <<'USAGE'
Usage: setup-postgres-linux.sh [options]

Creates or repairs QIndexer's private, loopback-only PostgreSQL cluster. The
cluster is started and stopped by qindexer itself; the host PostgreSQL service
is never enabled, started, or modified.

Options:
  --install-dir PATH       QIndexer installation directory, default /opt/qindexer
  --data-dir PATH          QIndexer data directory, default /var/lib/qindexer
  --database NAME          Database name, default qindexer
  --role NAME              Database role, default qindexer
  --port PORT              Private PostgreSQL port; auto-select when omitted
  --progress-file PATH     Write installation progress JSON to PATH
  --log-file PATH          Append setup output to PATH
  --reset-incomplete       Remove an incomplete private cluster with no state file
  -h, --help               Show this help
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --database) DATABASE_NAME="$2"; shift 2 ;;
    --role) ROLE_NAME="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --progress-file) PROGRESS_PATH="$2"; shift 2 ;;
    --log-file) LOG_PATH="$2"; shift 2 ;;
    --reset-incomplete) RESET_INCOMPLETE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

report_progress() {
  CURRENT_STAGE="$2"
  CURRENT_MESSAGE="$3"
  write_qindexer_install_progress "$PROGRESS_PATH" "$1" "$2" "$3" "$4"
}

log() {
  printf '%s\n' "$*" >&2
  if [[ -n "$LOG_PATH" ]]; then
    printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >> "$LOG_PATH"
  fi
}

run_logged() {
  if [[ -n "$LOG_PATH" ]]; then
    "$@" 2>&1 | tee -a "$LOG_PATH" >&2
  else
    "$@" >&2
  fi
}

SUDO=""
if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then SUDO="sudo"; fi
as_root() { if [[ -n "$SUDO" ]]; then "$SUDO" "$@"; else "$@"; fi; }
run_as_root() { if [[ -n "$SUDO" ]]; then run_logged "$SUDO" "$@"; else run_logged "$@"; fi; }
as_owner() {
  local owner="$1"
  shift
  if [[ "$(id -un)" == "$owner" ]]; then "$@"; else as_root runuser -u "$owner" -- "$@"; fi
}
run_as_owner() {
  local owner="$1"
  shift
  if [[ "$(id -un)" == "$owner" ]]; then run_logged "$@"; else run_as_root runuser -u "$owner" -- "$@"; fi
}

CURRENT_STAGE="postgres_prepare"
CURRENT_MESSAGE="Preparing QIndexer private PostgreSQL."
on_exit() {
  local status=$? details="installer output"
  [[ -n "$LOG_PATH" ]] && details="$LOG_PATH"
  if [[ "$status" -ne 0 ]]; then
    report_progress failed "$CURRENT_STAGE" 100 "PostgreSQL setup failed during $CURRENT_STAGE. See $details for details."
  fi
}
trap on_exit EXIT
report_progress running postgres_prepare 5 "Preparing QIndexer private PostgreSQL."
[[ "$DATABASE_NAME" =~ ^[A-Za-z_][A-Za-z0-9_]*$ && "$ROLE_NAME" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || {
  log "Database and role names must contain only letters, digits, and underscores."
  exit 2
}
[[ -z "$PORT" || "$PORT" =~ ^[0-9]+$ ]] || { log "PostgreSQL port must be numeric."; exit 2; }

if [[ -n "$SUDO" ]]; then
  report_progress running postgres_elevation 8 "Requesting permission to install PostgreSQL packages."
  sudo -v
fi
if [[ -n "$LOG_PATH" ]]; then
  as_root install -d -m 0700 "$(dirname "$LOG_PATH")"
  as_root touch "$LOG_PATH"
  as_root chmod 0600 "$LOG_PATH"
fi

install_postgres_packages() {
  if command -v dnf >/dev/null 2>&1; then
    run_as_root dnf install -y postgresql-server postgresql
  elif command -v apt-get >/dev/null 2>&1; then
    run_as_root apt-get update
    run_as_root apt-get install -y postgresql postgresql-client
  elif command -v zypper >/dev/null 2>&1; then
    run_as_root zypper --non-interactive install postgresql-server postgresql
  elif command -v pacman >/dev/null 2>&1; then
    run_as_root pacman -Sy --needed --noconfirm postgresql
  else
    log "No supported PostgreSQL package manager was found."
    exit 1
  fi
}

resolve_pg_binary() {
  local name="$1" candidate
  if command -v "$name" >/dev/null 2>&1; then command -v "$name"; return 0; fi
  for candidate in "/usr/bin/$name" /usr/lib/postgresql/*/bin/"$name" /usr/pgsql-*/bin/"$name"; do
    [[ -x "$candidate" ]] && { printf '%s\n' "$candidate"; return 0; }
  done
  return 1
}

ensure_postgres_binaries() {
  INITDB="$(resolve_pg_binary initdb || true)"
  POSTGRES="$(resolve_pg_binary postgres || true)"
  PSQL="$(resolve_pg_binary psql || true)"
  CREATEDB="$(resolve_pg_binary createdb || true)"
  PG_ISREADY="$(resolve_pg_binary pg_isready || true)"
  PG_CTL="$(resolve_pg_binary pg_ctl || true)"
  [[ -n "$INITDB" && -n "$POSTGRES" && -n "$PSQL" && -n "$CREATEDB" && -n "$PG_ISREADY" && -n "$PG_CTL" ]]
}

report_progress running postgres_discover 15 "Checking PostgreSQL binaries."
if ! ensure_postgres_binaries; then
  report_progress running postgres_install 55 "Installing PostgreSQL with the system package manager."
  install_postgres_packages
  ensure_postgres_binaries || { log "PostgreSQL packages installed, but required server commands are unavailable."; exit 1; }
fi

PG_ROOT="$DATA_DIR/postgresql"
PG_DATA_DIR="$PG_ROOT/data"
STATE_FILE="$PG_ROOT/qindexer-postgres.env"
RUNNER_USER="$(stat -c '%U' "$INSTALL_DIR" 2>/dev/null || true)"
if [[ -z "$RUNNER_USER" || "$RUNNER_USER" == "root" ]]; then
  if [[ "${PKEXEC_UID:-}" =~ ^[0-9]+$ ]]; then
    RUNNER_USER="$(id -un "$PKEXEC_UID")"
  elif [[ -n "${SUDO_USER:-}" && "$SUDO_USER" != "root" ]]; then
    RUNNER_USER="$SUDO_USER"
  else
    RUNNER_USER="root"
  fi
fi
PG_OWNER="$RUNNER_USER"
if [[ "$PG_OWNER" == "root" ]]; then PG_OWNER="postgres"; fi
id "$PG_OWNER" >/dev/null 2>&1 || { log "Cannot identify the account that will own the private PostgreSQL cluster: $PG_OWNER"; exit 1; }
PG_GROUP="$(id -gn "$PG_OWNER")"
PRIVATE_RUN_AS=""
if [[ "$RUNNER_USER" == "root" ]]; then
  PRIVATE_RUN_AS="$PG_OWNER"
fi

read_state_value() { sed -n "s/^$1=//p" "$STATE_FILE" | head -n 1; }
port_is_available() {
  local candidate="$1"
  if command -v ss >/dev/null 2>&1; then ! ss -ltnH "sport = :$candidate" 2>/dev/null | grep -q .; return; fi
  ! "$PG_ISREADY" -h 127.0.0.1 -p "$candidate" >/dev/null 2>&1
}
choose_port() {
  local candidate
  if [[ -n "$PORT" ]]; then port_is_available "$PORT" || { log "PostgreSQL port $PORT is already in use."; exit 1; }; printf '%s\n' "$PORT"; return; fi
  for candidate in $(seq 5432 5442); do port_is_available "$candidate" && { printf '%s\n' "$candidate"; return; }; done
  log "No free PostgreSQL port was found between 5432 and 5442."
  exit 1
}

if [[ -f "$STATE_FILE" ]]; then
  PORT="$(read_state_value QINDEXER_POSTGRES_PORT)"
  APP_PASSWORD="$(read_state_value QINDEXER_POSTGRES_PASSWORD)"
  [[ "$PORT" =~ ^[0-9]+$ && "$APP_PASSWORD" =~ ^[A-Za-z0-9]+$ && -f "$PG_DATA_DIR/PG_VERSION" ]] || {
    log "The private PostgreSQL state is incomplete. Re-run with --reset-incomplete only after confirming this private cluster can be discarded."
    exit 1
  }
elif [[ -d "$PG_ROOT" ]]; then
  if [[ "$RESET_INCOMPLETE" -ne 1 ]]; then
    log "An incomplete private PostgreSQL directory exists at $PG_ROOT. It was left untouched. Re-run with --reset-incomplete after confirming it contains no needed database."
    exit 1
  fi
  report_progress running postgres_reset 35 "Removing the incomplete private PostgreSQL cluster."
  as_root rm -rf "$PG_ROOT"
fi

if [[ ! -f "$STATE_FILE" ]]; then
  PORT="$(choose_port)"
  APP_PASSWORD="$(head -c 64 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c1-32)"
  report_progress running postgres_initialize 65 "Initializing QIndexer private PostgreSQL data."
  as_root install -d -o "$PG_OWNER" -g "$PG_GROUP" -m 0700 "$PG_ROOT"
  run_as_owner "$PG_OWNER" "$INITDB" -D "$PG_DATA_DIR" --auth-local=peer --auth-host=scram-sha-256
  as_root tee "$PG_DATA_DIR/qindexer.conf" >/dev/null <<CONFIG
listen_addresses = '127.0.0.1'
port = $PORT
unix_socket_directories = '$PG_ROOT'
password_encryption = 'scram-sha-256'
CONFIG
  as_root sh -c "printf '\ninclude_if_exists = '\''$PG_DATA_DIR/qindexer.conf'\''\n' >> '$PG_DATA_DIR/postgresql.conf'"
  as_root sed -i -E 's/^(host[[:space:]]+all[[:space:]]+all[[:space:]]+127\.0\.0\.1\/32[[:space:]]+).*/\1scram-sha-256/' "$PG_DATA_DIR/pg_hba.conf"
  as_root tee "$STATE_FILE" >/dev/null <<STATE
QINDEXER_POSTGRES_PORT=$PORT
QINDEXER_POSTGRES_PASSWORD=$APP_PASSWORD
STATE
  as_root chown -R "$PG_OWNER:$PG_GROUP" "$PG_ROOT"
  as_root chmod 0600 "$STATE_FILE"
fi

report_progress running postgres_start 75 "Starting QIndexer private PostgreSQL."
STARTED_TEMPORARY=0
if ! "$PG_ISREADY" -h 127.0.0.1 -p "$PORT" >/dev/null 2>&1; then
  as_owner "$PG_OWNER" "$POSTGRES" -D "$PG_DATA_DIR" -c listen_addresses=127.0.0.1 -c port="$PORT" > /dev/null 2>&1 &
  POSTGRES_PID=$!
  STARTED_TEMPORARY=1
  for _ in {1..30}; do "$PG_ISREADY" -h 127.0.0.1 -p "$PORT" >/dev/null 2>&1 && break; sleep 1; done
  "$PG_ISREADY" -h 127.0.0.1 -p "$PORT" >/dev/null 2>&1 || { log "Private PostgreSQL did not become ready on port $PORT."; wait "$POSTGRES_PID" || true; exit 1; }
fi

report_progress running postgres_configure 88 "Creating the QIndexer database and login."
ROLE_EXISTS="$(as_owner "$PG_OWNER" "$PSQL" -h "$PG_ROOT" -p "$PORT" -d postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname = '$ROLE_NAME'")"
if [[ "$ROLE_EXISTS" != "1" ]]; then
  as_owner "$PG_OWNER" "$PSQL" -h "$PG_ROOT" -p "$PORT" -d postgres -v ON_ERROR_STOP=1 -c "CREATE ROLE $ROLE_NAME LOGIN PASSWORD '$APP_PASSWORD'" >/dev/null
else
  as_owner "$PG_OWNER" "$PSQL" -h "$PG_ROOT" -p "$PORT" -d postgres -v ON_ERROR_STOP=1 -c "ALTER ROLE $ROLE_NAME PASSWORD '$APP_PASSWORD'" >/dev/null
fi
DATABASE_EXISTS="$(as_owner "$PG_OWNER" "$PSQL" -h "$PG_ROOT" -p "$PORT" -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$DATABASE_NAME'")"
if [[ "$DATABASE_EXISTS" != "1" ]]; then as_owner "$PG_OWNER" "$CREATEDB" -h "$PG_ROOT" -p "$PORT" -O "$ROLE_NAME" "$DATABASE_NAME"; fi

if [[ "$STARTED_TEMPORARY" -eq 1 ]]; then
  report_progress running postgres_handoff 96 "Handing the private PostgreSQL cluster to QIndexer."
  as_owner "$PG_OWNER" "$PG_CTL" -D "$PG_DATA_DIR" -m fast -w stop >/dev/null
fi

report_progress completed postgres_ready 100 "QIndexer private PostgreSQL is ready."
printf '{"driver":"postgres","dsn":"postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable","host":"127.0.0.1","port":%s,"database":"%s","private_data_dir":"%s","postgres_command":"%s","private_user":"%s"}\n' "$ROLE_NAME" "$APP_PASSWORD" "$PORT" "$DATABASE_NAME" "$PORT" "$DATABASE_NAME" "$PG_DATA_DIR" "$POSTGRES" "$PRIVATE_RUN_AS"
