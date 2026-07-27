#!/usr/bin/env bash
# Atomically activate gang-server.new and restore the previous executable when
# the replacement cannot stay running. A successful rollback deliberately
# keeps a non-zero exit status so CI reports the broken release.
set -euo pipefail

APP_DIR="${APP_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
CURRENT_BIN="${GANG_BIN:-$APP_DIR/gang-server}"
NEW_BIN="${GANG_NEW_BIN:-$APP_DIR/gang-server.new}"
BACKUP_BIN="${GANG_BACKUP_BIN:-$APP_DIR/gang-server.previous}"

cd "$APP_DIR"

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

if ./restart.sh gang; then
  rm -f "$BACKUP_BIN"
  echo "[gang] release activated"
  exit 0
fi

echo "[gang] release failed; restoring previous binary" >&2
if [ ! -f "$BACKUP_BIN" ]; then
  echo "[gang] rollback unavailable: no previous binary" >&2
  exit 1
fi

# restart.sh has already stopped the old process. stop.sh also clears any stale
# PID left behind if the candidate exited during its startup check.
./stop.sh gang || true
mv -f "$BACKUP_BIN" "$CURRENT_BIN"
chmod +x "$CURRENT_BIN"
if ./start.sh gang; then
  echo "[gang] rollback restored the previous release" >&2
else
  echo "[gang] rollback failed; manual recovery is required" >&2
fi
exit 1
