#!/usr/bin/env bash
# Start services in the background.
#   ./start.sh           # start all (livekit + gang-server)
#   ./start.sh gang      # start only gang-server
#   ./start.sh livekit   # start only livekit-server
set -euo pipefail
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
start_target "${1:-all}"
status_target "${1:-all}"
