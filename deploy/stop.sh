#!/usr/bin/env bash
# Stop services.
#   ./stop.sh            # stop all
#   ./stop.sh gang       # stop only gang-server
#   ./stop.sh livekit    # stop only livekit-server
set -euo pipefail
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"
stop_target "${1:-all}"
