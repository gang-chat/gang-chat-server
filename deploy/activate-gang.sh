#!/usr/bin/env bash
# Atomically activate gang-server.new and restore the previous executable when
# the replacement cannot stay running. A successful rollback deliberately
# keeps a non-zero exit status so CI reports the broken release.
set -euo pipefail

APP_DIR="${APP_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
CURRENT_BIN="${GANG_BIN:-$APP_DIR/gang-server}"
NEW_BIN="${GANG_NEW_BIN:-$APP_DIR/gang-server.new}"
BACKUP_BIN="${GANG_BACKUP_BIN:-$APP_DIR/gang-server.previous}"
GANG_HEALTH_URL="${GANG_HEALTH_URL-http://127.0.0.1:21116/health}"
GANG_HEALTH_ATTEMPTS="${GANG_HEALTH_ATTEMPTS:-20}"
GANG_HEALTH_DELAY_SECONDS="${GANG_HEALTH_DELAY_SECONDS:-1}"

cd "$APP_DIR"
. "$APP_DIR/lib.sh"

probe_health() {
  if [ -z "$GANG_HEALTH_URL" ]; then
    return 0
  fi
  if command -v curl >/dev/null 2>&1; then
    curl --fail --silent --show-error --max-time 2 "$GANG_HEALTH_URL" >/dev/null
    return
  fi
  if command -v wget >/dev/null 2>&1; then
    wget --quiet --timeout=2 --tries=1 -O /dev/null "$GANG_HEALTH_URL"
    return
  fi
  echo "[gang] curl or wget is required for the activation health check" >&2
  return 1
}

wait_for_health() {
  local attempt
  for attempt in $(seq 1 "$GANG_HEALTH_ATTEMPTS"); do
    if ! is_running gang; then
      return 1
    fi
    if probe_health; then
      echo "[gang] health check passed on attempt $attempt"
      return 0
    fi
    sleep "$GANG_HEALTH_DELAY_SECONDS"
  done
  return 1
}

if [ ! -f "$NEW_BIN" ]; then
  echo "[gang] release candidate not found: $NEW_BIN" >&2
  exit 1
fi

rm -f "$BACKUP_BIN"
if [ -f "$CURRENT_BIN" ]; then
  cp -p "$CURRENT_BIN" "$BACKUP_BIN"
fi

chmod +x "$NEW_BIN"
mv -f "$NEW_BIN" "$CURRENT_BIN"

if ./restart.sh gang && wait_for_health; then
  rm -f "$BACKUP_BIN"
  echo "[gang] release activated"
  exit 0
fi

echo "[gang] release failed; restoring previous binary" >&2
tail -n 80 "$(logfile gang)" >&2 || true
if [ ! -f "$BACKUP_BIN" ]; then
  echo "[gang] rollback unavailable: no previous binary" >&2
  exit 1
fi

# restart.sh has already stopped the old process. stop.sh also clears any stale
# PID left behind if the candidate exited during its startup check.
./stop.sh gang || true
mv -f "$BACKUP_BIN" "$CURRENT_BIN"
chmod +x "$CURRENT_BIN"
if ./start.sh gang && wait_for_health; then
  echo "[gang] rollback restored the previous release" >&2
else
  echo "[gang] rollback failed; manual recovery is required" >&2
  tail -n 80 "$(logfile gang)" >&2 || true
fi
exit 1
