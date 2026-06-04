#!/bin/bash
# run_analysis.sh — Run RA analysis + trie node stats plotting + execute stats plotting
#
# Usage:
#   ./run_analysis.sh                              # analyze latest files
#   ./run_analysis.sh 11350800_11351000            # analyze a specific block range
#   ./run_analysis.sh 11350800_11351000 1000       # with bucket_size=1000 for plots
#   ./run_analysis.sh "" "" kvsep                  # with mode suffix for output dir

set -e

EXEC_DIR=~/ethereum/execution
ANALYSIS_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$ANALYSIS_DIR"

BLOCK_RANGE="$1"
MODE_SUFFIX="$3"
# ── Bucket size for execute stats plots ─────────────────────────────────────
# Set DEFAULT_BUCKET_SIZE to a fixed value (e.g., 1000) or leave as "auto".
# Command-line $2 overrides this default.
DEFAULT_BUCKET_SIZE="500"
BUCKET_SIZE="${2:-$DEFAULT_BUCKET_SIZE}"
if [ "$BUCKET_SIZE" = "auto" ]; then
  BUCKET_SIZE=""
fi

# ── Helper: find latest file matching prefix (+ optional block range) ────────
find_file() {
  local prefix="$1"
  if [ -n "$BLOCK_RANGE" ]; then
    ls -t "$EXEC_DIR/${prefix}_${BLOCK_RANGE}"* 2>/dev/null | head -1
  else
    ls -t "$EXEC_DIR/${prefix}_"* 2>/dev/null | head -1
  fi
}

# ── Ensure RA binary is built (optional — skip RA if build fails) ────────────
PARENT_DIR="$(dirname "$ANALYSIS_DIR")"
if [ ! -f "$PARENT_DIR/bin/analysisReadAmplification" ]; then
  echo "RA binary not found. Attempting to build..."
  bash "$PARENT_DIR/build.sh" build || echo "  WARNING: RA build failed, will skip RA analysis."
fi

# ── Resolve files ────────────────────────────────────────────────────────────
TRACE_FILE=$(find_file "blktrace")
TRIE_FILE=$(find_file "trie_node_stats")
EXEC_STATS_FILE=$(find_file "execute_stats")

if [ -z "$TRACE_FILE" ] && [ -z "$TRIE_FILE" ] && [ -z "$EXEC_STATS_FILE" ]; then
  echo "Error: no blktrace, trie_node_stats, or execute_stats file found."
  exit 1
fi

# Derive block range from whichever file we found
if [ -n "$TRACE_FILE" ]; then
  RANGE=$(basename "$TRACE_FILE" | sed 's/^blktrace_\([0-9]*_[0-9]*\).*/\1/')
elif [ -n "$TRIE_FILE" ]; then
  RANGE=$(basename "$TRIE_FILE" | sed 's/^trie_node_stats_\([0-9]*_[0-9]*\).*/\1/')
elif [ -n "$EXEC_STATS_FILE" ]; then
  RANGE=$(basename "$EXEC_STATS_FILE" | sed 's/^execute_stats_\([0-9]*_[0-9]*\).*/\1/')
fi
if [ -n "$MODE_SUFFIX" ]; then
  OUTPUT_DIR="./raOutput_${RANGE}_${MODE_SUFFIX}"
else
  OUTPUT_DIR="./raOutput_${RANGE}"
fi

echo "Output directory: $OUTPUT_DIR"
echo ""

# ── Sub-directories for each analysis ────────────────────────────────────────
RA_DIR="$OUTPUT_DIR/read_amplification"
TRIE_DIR="$OUTPUT_DIR/trie_node_stats"
EXEC_DIR_OUT="$OUTPUT_DIR/execute_stats"

# ── 1. Read Amplification analysis ───────────────────────────────────────────
if [ -n "$TRACE_FILE" ] && [ -f "$PARENT_DIR/bin/analysisReadAmplification" ]; then
  mkdir -p "$RA_DIR"
  echo "══════════════════════════════════════════════════════"
  echo "[1/3] Read Amplification Analysis"
  echo "  Input:  $TRACE_FILE"
  echo "  Output: $RA_DIR"
  echo "══════════════════════════════════════════════════════"
  "$PARENT_DIR/bin/analysisReadAmplification" "$TRACE_FILE" "$RA_DIR"
  echo ""
elif [ -n "$TRACE_FILE" ]; then
  echo "[1/3] Skipping RA analysis: RA binary not found (blktrace file exists)."
else
  echo "[1/3] Skipping RA analysis: no blktrace file found."
fi

# ── 2. Trie node stats: copy CSV + plot ──────────────────────────────────────
if [ -n "$TRIE_FILE" ]; then
  mkdir -p "$TRIE_DIR"
  echo "══════════════════════════════════════════════════════"
  echo "[2/3] Trie Node Stats"
  echo "  Input:  $TRIE_FILE"
  echo "  Output: $TRIE_DIR"
  echo "══════════════════════════════════════════════════════"
  cp "$TRIE_FILE" "$TRIE_DIR/"
  echo "  Copied: $(basename "$TRIE_FILE")"
  python3 "$ANALYSIS_DIR/plotTrieNodeStats.py" "$TRIE_FILE" "$TRIE_DIR"
  echo ""
else
  echo "[2/3] Skipping trie analysis: no trie_node_stats file found."
fi

# ── 3. Execute stats: copy CSV + plot ────────────────────────────────────────
if [ -n "$EXEC_STATS_FILE" ]; then
  mkdir -p "$EXEC_DIR_OUT"
  echo "══════════════════════════════════════════════════════"
  echo "[3/3] Execute Stats (insertChain timing)"
  echo "  Input:  $EXEC_STATS_FILE"
  echo "  Output: $EXEC_DIR_OUT"
  echo "══════════════════════════════════════════════════════"
  cp "$EXEC_STATS_FILE" "$EXEC_DIR_OUT/"
  echo "  Copied: $(basename "$EXEC_STATS_FILE")"
  python3 "$ANALYSIS_DIR/plotExecuteStats.py" "$EXEC_STATS_FILE" "$EXEC_DIR_OUT" $BUCKET_SIZE
  echo ""
else
  echo "[3/3] Skipping execute stats: no execute_stats file found."
fi

# ── 4. BadgerDB metrics: copy CSV ───────────────────────────────────────────
BADGER_FILE=$(find_file "badger_metrics")
if [ -n "$BADGER_FILE" ]; then
  BADGER_DIR="$OUTPUT_DIR/badger_metrics"
  mkdir -p "$BADGER_DIR"
  echo "══════════════════════════════════════════════════════"
  echo "[4/4] BadgerDB Metrics"
  echo "  Input:  $BADGER_FILE"
  echo "  Output: $BADGER_DIR"
  echo "══════════════════════════════════════════════════════"
  cp "$BADGER_FILE" "$BADGER_DIR/"
  echo "  Copied: $(basename "$BADGER_FILE")"
  echo ""
else
  echo "[4/4] Skipping BadgerDB metrics: no badger_metrics file found."
fi

echo "[done] Results in: $OUTPUT_DIR"
