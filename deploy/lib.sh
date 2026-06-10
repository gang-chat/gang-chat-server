#!/usr/bin/env bash
# Shared config + helpers for the gang-chat deploy scripts.
# Sourced by start.sh / stop.sh / restart.sh — not meant to be run directly.
set -euo pipefail

# APP_DIR = directory these scripts live in on the server (the deploy dir).
APP_DIR="${APP_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"

# Optional per-server overrides (paths, binary names, ...). Not committed.
if [ -f "$APP_DIR/deploy.env" ]; then
  # shellcheck disable=SC1091
  set -a; . "$APP_DIR/deploy.env"; set +a
fi

GANG_BIN="${GANG_BIN:-$APP_DIR/gang-server}"
LIVEKIT_BIN="${LIVEKIT_BIN:-$APP_DIR/livekit-server}"
LIVEKIT_CONFIG="${LIVEKIT_CONFIG:-$APP_DIR/livekit.yaml}"

LOG_DIR="${LOG_DIR:-$APP_DIR/logs}"
RUN_DIR="${RUN_DIR:-$APP_DIR/run}"

mkdir -p "$LOG_DIR" "$RUN_DIR"

pidfile() { echo "$RUN_DIR/$1.pid"; }
logfile() { echo "$LOG_DIR/$1.log"; }

# is_running <name> -> 0 if the tracked process is alive
is_running() {
  local pf pid
  pf="$(pidfile "$1")"
  [ -f "$pf" ] || return 1
  pid="$(cat "$pf" 2>/dev/null || true)"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null
}

# start_service <name> <binary> [args...]
start_service() {
  local name="$1" bin="$2"; shift 2
  if is_running "$name"; then
    echo "[$name] already running (pid $(cat "$(pidfile "$name")"))"
    return 0
  fi
  if [ ! -x "$bin" ]; then
    echo "[$name] binary not found or not executable: $bin" >&2
    return 1
  fi
  echo "[$name] starting..."
  # Run from APP_DIR so the app's relative paths (.env, sqlite db) resolve there.
  (
    cd "$APP_DIR"
    nohup "$bin" "$@" </dev/null >>"$(logfile "$name")" 2>&1 &
    echo $! >"$(pidfile "$name")"
  )
  sleep 1
  if is_running "$name"; then
    echo "[$name] started (pid $(cat "$(pidfile "$name")")) -> $(logfile "$name")"
  else
    echo "[$name] FAILED to start — last log lines:" >&2
    tail -n 20 "$(logfile "$name")" >&2 || true
    return 1
  fi
}

# stop_service <name>
stop_service() {
  local name="$1" pf pid
  pf="$(pidfile "$name")"
  if ! is_running "$name"; then
    echo "[$name] not running"
    rm -f "$pf"
    return 0
  fi
  pid="$(cat "$pf")"
  echo "[$name] stopping (pid $pid)..."
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 10); do
    is_running "$name" || break
    sleep 1
  done
  if is_running "$name"; then
    echo "[$name] did not exit, sending SIGKILL"
    kill -9 "$pid" 2>/dev/null || true
    sleep 1
  fi
  rm -f "$pf"
  echo "[$name] stopped"
}

status_service() {
  local name="$1"
  if is_running "$name"; then
    echo "[$name] RUNNING (pid $(cat "$(pidfile "$name")"))"
  else
    echo "[$name] stopped"
  fi
}

# --- service definitions -------------------------------------------------

# ensure_ffmpeg makes sure ffmpeg + ffprobe are present, since the music box
# downloads tracks and transcodes them to Opus on the server. Idempotent:
# does nothing if both are already on PATH. Best-effort install via apt.
ensure_ffmpeg() {
  if command -v ffmpeg >/dev/null 2>&1 && command -v ffprobe >/dev/null 2>&1; then
    return 0
  fi
  echo "[ffmpeg] not found, installing (needed by the music box)..."
  local SUDO=""; command -v sudo >/dev/null 2>&1 && SUDO="sudo"
  if ! command -v apt-get >/dev/null 2>&1; then
    echo "[ffmpeg] apt-get not available; install ffmpeg manually" >&2
    return 1
  fi
  $SUDO apt-get update
  $SUDO apt-get install -y --no-install-recommends ffmpeg
  if command -v ffmpeg >/dev/null 2>&1 && command -v ffprobe >/dev/null 2>&1; then
    echo "[ffmpeg] installed: $(ffmpeg -version 2>/dev/null | sed -n '1p')"
  else
    echo "[ffmpeg] install failed; the music box will be degraded" >&2
    return 1
  fi
}

svc_gang_start()    { ensure_ffmpeg || true; start_service gang    "$GANG_BIN"; }
svc_livekit_start() { start_service livekit "$LIVEKIT_BIN" --config "$LIVEKIT_CONFIG"; }

# Dispatch by target: all (default) | gang | livekit
start_target() {
  case "${1:-all}" in
    all)     svc_livekit_start; svc_gang_start;;   # livekit first, then app
    gang)    svc_gang_start;;
    livekit) svc_livekit_start;;
    *) echo "unknown target: ${1:-} (use: all | gang | livekit)" >&2; return 2;;
  esac
}

stop_target() {
  case "${1:-all}" in
    all)     stop_service gang; stop_service livekit;;  # reverse order
    gang)    stop_service gang;;
    livekit) stop_service livekit;;
    *) echo "unknown target: ${1:-} (use: all | gang | livekit)" >&2; return 2;;
  esac
}

status_target() {
  case "${1:-all}" in
    all)     status_service gang; status_service livekit;;
    gang)    status_service gang;;
    livekit) status_service livekit;;
    *) echo "unknown target: ${1:-} (use: all | gang | livekit)" >&2; return 2;;
  esac
}
