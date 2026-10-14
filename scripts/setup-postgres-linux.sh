#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="/opt/qindexer"
DATA_DIR="/var/lib/qindexer"
DATABASE_NAME="qindexer"
ROLE_NAME="qindexer"
PORT="5432"
PROGRESS_PATH=""

source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/install-progress-linux.sh"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --install-dir) INSTALL_DIR="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --database) DATABASE_NAME="$2"; shift 2 ;;
    --role) ROLE_NAME="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --progress-file) PROGRESS_PATH="$2"; shift 2 ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done

report_progress() {
  write_qindexer_install_progress "$PROGRESS_PATH" "$1" "$2" "$3" "$4"
}

trap 'report_progress failed postgres 100 "PostgreSQL setup failed."' ERR
report_progress running postgres_prepare 5 "Preparing private PostgreSQL setup."

[[ "$DATABASE_NAME" =~ ^[A-Za-z_][A-Za-z0-9_]*$ && "$ROLE_NAME" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || { echo "Database and role names must contain only letters, digits, and underscores." >&2; exit 2; }

SUDO=""
if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then SUDO="sudo"; fi

install_postgres() {
  if command -v dnf >/dev/null 2>&1; then
    $SUDO dnf install -y postgresql-server postgresql
    $SUDO postgresql-setup --initdb --unit postgresql || true
    $SUDO systemctl enable --now postgresql
  elif command -v apt-get >/dev/null 2>&1; then
    $SUDO apt-get update
    $SUDO apt-get install -y postgresql postgresql-client
    $SUDO systemctl enable --now postgresql
  elif command -v zypper >/dev/null 2>&1; then
    $SUDO zypper --non-interactive install postgresql-server postgresql
    $SUDO systemctl enable --now postgresql
  elif command -v pacman >/dev/null 2>&1; then
    $SUDO pacman -Sy --needed --noconfirm postgresql
    if [[ ! -d /var/lib/postgres/data ]]; then
      $SUDO install -d -o postgres -g postgres /var/lib/postgres/data
      $SUDO -u postgres initdb -D /var/lib/postgres/data
    fi
    $SUDO systemctl enable --now postgresql
  else
    echo "No supported PostgreSQL package manager was found." >&2
    exit 1
  fi
}

report_progress running postgres_discover 15 "Checking the local PostgreSQL installation."
if ! command -v psql >/dev/null 2>&1 || ! id postgres >/dev/null 2>&1; then
  report_progress running postgres_install 55 "Installing PostgreSQL with the system package manager."
  install_postgres
fi
command -v psql >/dev/null 2>&1 || { echo "PostgreSQL installation completed but psql is unavailable." >&2; exit 1; }
id postgres >/dev/null 2>&1 || { echo "PostgreSQL installation completed but the postgres service account is unavailable." >&2; exit 1; }

report_progress running postgres_start 70 "Starting PostgreSQL."
if ! $SUDO -u postgres pg_isready -p "$PORT" >/dev/null 2>&1; then
  $SUDO systemctl start postgresql || $SUDO systemctl start postgresql.service
fi
for _ in {1..30}; do
  if $SUDO -u postgres pg_isready -p "$PORT" >/dev/null 2>&1; then break; fi
  sleep 1
done
$SUDO -u postgres pg_isready -p "$PORT" >/dev/null 2>&1 || { echo "PostgreSQL did not become ready on port $PORT." >&2; exit 1; }

secret() { head -c 64 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c1-32; }
APP_PASSWORD="$(secret)"
report_progress running postgres_configure 85 "Creating the QIndexer database and login."
ROLE_EXISTS="$($SUDO -u postgres psql -p "$PORT" -d postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname = '$ROLE_NAME'")"
if [[ "$ROLE_EXISTS" != "1" ]]; then
  $SUDO -u postgres psql -p "$PORT" -d postgres -v ON_ERROR_STOP=1 -c "CREATE ROLE $ROLE_NAME LOGIN PASSWORD '$APP_PASSWORD'" >/dev/null
else
  $SUDO -u postgres psql -p "$PORT" -d postgres -v ON_ERROR_STOP=1 -c "ALTER ROLE $ROLE_NAME PASSWORD '$APP_PASSWORD'" >/dev/null
fi
DATABASE_EXISTS="$($SUDO -u postgres psql -p "$PORT" -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$DATABASE_NAME'")"
if [[ "$DATABASE_EXISTS" != "1" ]]; then
  $SUDO -u postgres createdb -p "$PORT" -O "$ROLE_NAME" "$DATABASE_NAME"
fi

report_progress completed postgres_ready 100 "Private PostgreSQL is ready for QIndexer."
printf '{"driver":"postgres","dsn":"postgres://%s:%s@127.0.0.1:%s/%s?sslmode=disable","host":"127.0.0.1","port":%s,"database":"%s"}\n' "$ROLE_NAME" "$APP_PASSWORD" "$PORT" "$DATABASE_NAME" "$PORT" "$DATABASE_NAME"
