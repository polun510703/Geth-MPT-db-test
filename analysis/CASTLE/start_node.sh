#!/bin/bash
# start_node.sh — Build geth, launch Geth + Prysm in background, then run analysis
#
# Usage:
#   ./start_node.sh kvsep    # Full KV separation (threshold=1)
#   ./start_node.sh nosep    # No KV separation (threshold=1048576)
#   ./start_node.sh both     # Run kvsep first, then nosep sequentially
#
# Options:
#   SKIP_BUILD=true ./start_node.sh kvsep   # skip build step
#
# View logs:
#   tail -f ~/ethereum/logs/latest/geth.log
#   tail -f ~/ethereum/logs/latest/prysm.log

set -e

if [ -z "$TMUX" ] && [ -z "$STY" ] && [ -n "$SSH_CONNECTION" ]; then
  echo "[warn] SSH session without tmux/screen — disconnect will kill this run."
  echo "[warn] Consider: ./tmux_run.sh $*"
  echo "[warn] Continuing in 5s... (Ctrl+C to abort)"
  sleep 5
fi

export PATH=$PATH:/usr/local/go/bin

GETH_REPO=~/Geth-CASTLE-lab
EXEC_DIR=~/ethereum/execution
CONS_DIR=~/ethereum/consensus

# ── Parse mode ───────────────────────────────────────────────────────────────
MODE="${1:-kvsep}"
if [ "$MODE" != "kvsep" ] && [ "$MODE" != "nosep" ] && [ "$MODE" != "both" ]; then
  echo "Usage: $0 {kvsep|nosep|both}"
  echo "  kvsep  — Full KV separation (ValueThreshold=1)"
  echo "  nosep  — No KV separation (ValueThreshold=1048576)"
  echo "  both   — Run kvsep first, then nosep sequentially"
  exit 1
fi

# ── Run a single mode (kvsep or nosep) ──────────────────────────────────────
run_single() {
  local mode="$1"
  local threshold datadir

  if [ "$mode" = "kvsep" ]; then
    threshold=1
    datadir="./data-kvsep"
  else
    threshold=1048576
    datadir="./data-nosep"
  fi

  echo ""
  echo "══════════════════════════════════════════════════════"
  echo "  Mode: $mode (ValueThreshold=$threshold)"
  echo "  Datadir: $datadir"
  echo "══════════════════════════════════════════════════════"
  echo ""

  LOG_DIR=~/ethereum/logs/$(date +%Y%m%d_%H%M%S)_${mode}

  # ── 0. Clean up stale processes ────────────────────────────────────────────
  if pgrep -x geth > /dev/null; then
    echo "[cleanup] Killing stale geth (PID: $(pgrep -x geth))..."
    pkill -x geth
    sleep 2
  fi
  if pgrep -f "prysm.sh|beacon-chain" > /dev/null; then
    echo "[cleanup] Killing stale prysm (PID: $(pgrep -f 'prysm.sh|beacon-chain'))..."
    pkill -f "prysm.sh|beacon-chain"
    sleep 2
  fi
  # Remove stale lock file if no geth process is running
  if [ -f "$EXEC_DIR/${datadir}/geth/LOCK" ]; then
    echo "[cleanup] Removing stale LOCK file..."
    rm -f "$EXEC_DIR/${datadir}/geth/LOCK"
  fi

  # ── 1. Build ───────────────────────────────────────────────────────────────
  if [ "${SKIP_BUILD}" != "true" ]; then
    echo "[1/4] Building geth..."
    cd "$GETH_REPO"
    make
    NEW_BIN="$GETH_REPO/build/bin/geth"
    OLD_BIN="$EXEC_DIR/geth"
    if ! cmp -s "$NEW_BIN" "$OLD_BIN"; then
      cp "$NEW_BIN" "$OLD_BIN"
      echo "      Binary updated."
    else
      echo "      Binary unchanged, skipping copy."
    fi
    # Only build once for 'both' mode
    SKIP_BUILD=true
  else
    echo "[1/4] Skipping build (SKIP_BUILD=true)"
  fi

  # ── 2. Prepare log directory ───────────────────────────────────────────────
  mkdir -p "$LOG_DIR"
  ln -sfn "$LOG_DIR" ~/ethereum/logs/latest
  echo "[2/4] Logs: $LOG_DIR"

  # ── 3. Start geth ─────────────────────────────────────────────────────────
  echo "[3/4] Starting geth (mode=$mode, threshold=$threshold)..."
  cd "$EXEC_DIR"
  GOMAXPROCS=1 stdbuf -oL ./geth \
    --db.engine=badger \
    --db.badger.valuethreshold=$threshold \
    --cache.noprefetch \
    --mainnet \
    --datadir "$datadir" \
    --syncmode full \
    --http \
    --http.api eth,net,engine,admin \
    --authrpc.jwtsecret ../jwt.hex \
    > "$LOG_DIR/geth.log" 2>&1 &
  GETH_PID=$!
  echo "      geth PID=$GETH_PID"

  # ── 4. Wait for geth authrpc, then start prysm ────────────────────────────
  echo "      Waiting for geth authrpc (port 8551)..."
  for i in $(seq 1 60); do
    if ss -tlnp | grep -q ':8551'; then
      echo "      geth authrpc ready."
      break
    fi
    if ! kill -0 "$GETH_PID" 2>/dev/null; then
      echo "ERROR: geth exited before authrpc was ready. Check log:"
      echo "  tail $LOG_DIR/geth.log"
      exit 1
    fi
    sleep 2
  done

  echo "[4/4] Starting prysm..."
  cd "$CONS_DIR"

  USE_PRYSM_VERSION=v7.1.2 ./prysm.sh beacon-chain \
    --accept-terms-of-use \
    --datadir ./data \
    --execution-endpoint=http://localhost:8551 \
    --mainnet \
    --jwt-secret=../jwt.hex \
    --checkpoint-sync-url=https://beaconstate.info \
    --genesis-beacon-api-url=https://beaconstate.info \
    > "$LOG_DIR/prysm.log" 2>&1 &
  PRYSM_PID=$!
  echo "      prysm PID=$PRYSM_PID"

  echo ""
  echo "══════════════════════════════════════════════════════"
  echo "  [$mode] Nodes running in background."
  echo "  View logs:"
  echo "    tail -f $LOG_DIR/geth.log"
  echo "    tail -f $LOG_DIR/prysm.log"
  echo "  Waiting for geth to exit before running analysis..."
  echo "══════════════════════════════════════════════════════"
  echo ""

  # ── 5. Wait for geth to finish → run analysis → stop prysm ────────────────
  wait $GETH_PID
  echo ""
  echo "[$mode] geth exited. Running analysis..."
  cd "$GETH_REPO/analysis/CASTLE"
  bash run_analysis.sh "" "" "$mode"

  echo "[$mode] Analysis done. Stopping prysm..."
  kill $PRYSM_PID 2>/dev/null
  wait $PRYSM_PID 2>/dev/null || true

  echo "[$mode] Finished. Logs in: $LOG_DIR"
}

# ── Main ─────────────────────────────────────────────────────────────────────
if [ "$MODE" = "both" ]; then
  echo "Running both modes sequentially: kvsep → nosep"
  run_single kvsep
  run_single nosep
  echo ""
  echo "[done] Both modes finished."
else
  run_single "$MODE"
  echo ""
  echo "[done] All finished."
fi
