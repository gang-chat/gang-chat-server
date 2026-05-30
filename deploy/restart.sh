#!/usr/bin/env bash
# Restart services.
#   ./restart.sh         # restart all
#   ./restart.sh gang    # restart only gang-server (leaves livekit untouched)
#   ./restart.sh livekit # restart only livekit-server
set -euo pipefail
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
target="${1:-all}"
stop_target "$target"
start_target "$target"
status_target "$target"
