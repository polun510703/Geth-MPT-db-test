#!/bin/bash
# tmux_run.sh — Run start_node.sh inside a detached tmux session,
# so SSH disconnects do not kill the long-running full-sync experiment.
#
# Usage (drop-in replacement for start_node.sh):
#   ./tmux_run.sh kvsep
#   ./tmux_run.sh nosep
#   ./tmux_run.sh both
#   SKIP_BUILD=true ./tmux_run.sh kvsep
#
# After launch:
#   tmux attach -t geth-sync          # watch live output
#   Ctrl+b then d                     # detach (run keeps going)
#   tmux ls                           # check if session still alive
#   tmux kill-session -t geth-sync    # force stop

set -e

SESSION="geth-sync"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

if ! command -v tmux >/dev/null 2>&1; then
  echo "ERROR: tmux not installed. Install with: sudo apt install tmux"
  exit 1
fi

if tmux has-session -t "$SESSION" 2>/dev/null; then
  echo "ERROR: tmux session '$SESSION' already exists."
  echo "  Attach:     tmux attach -t $SESSION"
  echo "  Force stop: tmux kill-session -t $SESSION"
  exit 1
fi

if [ $# -lt 1 ]; then
  echo "Usage: $0 {kvsep|nosep|both}"
  exit 1
fi

# Forward SKIP_BUILD into the tmux shell. Quote args so spaces survive.
ARGS=""
for a in "$@"; do
  ARGS+=" $(printf '%q' "$a")"
done

tmux new-session -d -s "$SESSION" \
  "cd $(printf '%q' "$SCRIPT_DIR") && \
   SKIP_BUILD=${SKIP_BUILD:-false} ./start_node.sh${ARGS}; \
   echo; echo '--- start_node.sh exited (rc=\$?). Press any key to close tmux pane. ---'; \
   read -n 1"

tmux set-option -t "$SESSION" remain-on-exit on >/dev/null

echo "tmux session '$SESSION' started in background."
echo "  Attach:     tmux attach -t $SESSION"
echo "  Detach:     Ctrl+b then d"
echo "  Logs:       tail -f ~/ethereum/logs/latest/geth.log"
echo "  Force stop: tmux kill-session -t $SESSION"
