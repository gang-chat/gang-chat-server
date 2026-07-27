#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"

cleanup() {
  if [ -f "$TEST_ROOT/run/gang.pid" ]; then
    kill "$(cat "$TEST_ROOT/run/gang.pid")" 2>/dev/null || true
  fi
  rm -rf "$TEST_ROOT"
}
trap cleanup EXIT

cp "$REPO_DIR/deploy/lib.sh" "$REPO_DIR/deploy/start.sh" \
  "$REPO_DIR/deploy/stop.sh" "$REPO_DIR/deploy/restart.sh" \
  "$REPO_DIR/deploy/activate-gang.sh" "$TEST_ROOT/"
chmod +x "$TEST_ROOT"/*.sh

write_service() {
  local path="$1" label="$2" mode="$3"
  cat >"$path" <<EOF
#!/usr/bin/env bash
echo "$label" >>"\${APP_DIR}/started"
if [ "$mode" = "fail" ]; then
  exit 23
fi
while true; do sleep 1; done
EOF
  chmod +x "$path"
}

write_service "$TEST_ROOT/gang-server" old run
APP_DIR="$TEST_ROOT" "$TEST_ROOT/start.sh" gang

write_service "$TEST_ROOT/gang-server.new" new run
APP_DIR="$TEST_ROOT" "$TEST_ROOT/activate-gang.sh"
grep -qx 'new' <(tail -n 1 "$TEST_ROOT/started")
test ! -e "$TEST_ROOT/gang-server.previous"
APP_DIR="$TEST_ROOT" "$TEST_ROOT/stop.sh" gang

write_service "$TEST_ROOT/gang-server.new" broken fail
if APP_DIR="$TEST_ROOT" "$TEST_ROOT/activate-gang.sh"; then
  echo "expected the failed candidate activation to return non-zero" >&2
  exit 1
fi
grep -qx 'new' <(tail -n 1 "$TEST_ROOT/started")
kill -0 "$(cat "$TEST_ROOT/run/gang.pid")"
test ! -e "$TEST_ROOT/gang-server.previous"

echo "activate-gang rollback tests passed"
