#!/usr/bin/env bash

write_qindexer_install_progress() {
  local path="$1" status="$2" stage="$3" percent="$4" message="$5"
  local bytes_downloaded="${6:-0}" bytes_total="${7:-0}" bytes_per_second="${8:-0}"
  [[ -n "$path" ]] || return 0
  local directory temporary escaped
  directory="$(dirname "$path")"
  temporary="${path}.tmp"
  escaped="$(printf '%s' "$message" | sed 's/\\/\\\\/g; s/"/\\"/g')"
  mkdir -p "$directory" || return 0
  printf '{"schema_version":1,"status":"%s","stage":"%s","percent":%s,"message":"%s","bytes_downloaded":%s,"bytes_total":%s,"bytes_per_second":%s,"updated_at":"%s"}\n' \
    "$status" "$stage" "$percent" "$escaped" "$bytes_downloaded" "$bytes_total" "$bytes_per_second" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$temporary" || return 0
  mv -f "$temporary" "$path" || return 0
}
