#!/bin/bash
# run_observation.sh — One observation read-benchmark run.
#
# Wraps `go test -run TestObservation` with iostat in the background, then
# bundles the latency / badger / iostat outputs into raOutput_obs_*/.
#
# Usage:
#   ./run_observation.sh                                # defaults
#   OBS_MODE=kvsep OBS_PATTERN=zipf OBS_RATIO=3:7 OBS_NOPS=1000000 ./run_observation.sh
#
# Required: TestExtractWorkload has been run for the chosen OBS_MODE so that
#   analysis/CASTLE/observation/data/accounts_<mode>.bin exists.
#
# Env vars consumed:
#   OBS_MODE          kvsep|nosep                       (default: kvsep)
#   OBS_PATTERN       uniform|zipf|seq|recent           (default: zipf)
#   OBS_RATIO         tx:acct                           (default: 1:1)
#   OBS_NOPS          total ops                         (default: 1000000)
#   OBS_SEED          rng seed                          (default: 42)
#   OBS_ZIPF_S        zipf skew                         (default: 1.1)
#   OBS_RECENT_FRAC   recent-heavy hot fraction         (default: 0.1)
#   OBS_DATADIR       override chaindata datadir        (default: ~/ethereum/execution/data-<mode>)
#   DROP_CACHE        1 → sudo drop_caches before run   (default: 0)
#   IOSTAT_DEVICE     filter iostat to this device      (default: auto from datadir)
#   EBPF_TRACE        1 → run bpftrace to per-inode     (default: 0; needs sudo)
#                     attribute page-cache fills so we
#                     can split SSD read bytes between
#                     LSM (.sst) and vlog (.vlog).

set -e

OBS_DIR="$(cd "$(dirname "$0")" && pwd)"
GETH_REPO="$(cd "$OBS_DIR/../../.." && pwd)"

export PATH=$PATH:/usr/local/go/bin

OBS_MODE="${OBS_MODE:-kvsep}"
OBS_PATTERN="${OBS_PATTERN:-zipf}"
OBS_RATIO="${OBS_RATIO:-1:1}"
OBS_NOPS="${OBS_NOPS:-2000000}"
OBS_SEED="${OBS_SEED:-42}"
OBS_ZIPF_S="${OBS_ZIPF_S:-1.1}"
OBS_RECENT_FRAC="${OBS_RECENT_FRAC:-0.1}"

case "$OBS_MODE" in
  kvsep|nosep) ;;
  *) echo "OBS_MODE must be kvsep|nosep (got $OBS_MODE)"; exit 1 ;;
esac

OBS_DATADIR="${OBS_DATADIR:-$HOME/ethereum/execution/data-${OBS_MODE}}"
if [ ! -d "$OBS_DATADIR/geth/chaindata" ]; then
  echo "ERROR: $OBS_DATADIR/geth/chaindata not found"
  exit 1
fi

# Detect block device backing the datadir (e.g. /dev/nvme0n1p1 → nvme0n1)
if [ -z "$IOSTAT_DEVICE" ]; then
  src_dev=$(df --output=source "$OBS_DATADIR" | tail -1)
  src_dev=$(basename "$src_dev")
  # strip partition suffix: nvme0n1p1 → nvme0n1, sda1 → sda
  case "$src_dev" in
    nvme*) IOSTAT_DEVICE="${src_dev%p[0-9]*}" ;;
    sd*|hd*|vd*) IOSTAT_DEVICE="${src_dev%[0-9]*}" ;;
    *) IOSTAT_DEVICE="$src_dev" ;;
  esac
fi
echo "[run] iostat device: $IOSTAT_DEVICE"

# Ratio tag like 3_7 for filesystem-friendly output dir
RATIO_TAG="${OBS_RATIO//:/_}"
TS="$(date +%Y%m%d_%H%M%S)"
OBS_OUTPUT_DIR="$OBS_DIR/raOutput_obs_${OBS_MODE}_${OBS_PATTERN}_${RATIO_TAG}_${TS}"
mkdir -p "$OBS_OUTPUT_DIR"/{badger_metrics,latency,iostat}

# config.json snapshot
{
  echo "{"
  echo "  \"obs_mode\": \"$OBS_MODE\","
  echo "  \"obs_pattern\": \"$OBS_PATTERN\","
  echo "  \"obs_ratio\": \"$OBS_RATIO\","
  echo "  \"obs_nops\": $OBS_NOPS,"
  echo "  \"obs_seed\": $OBS_SEED,"
  echo "  \"obs_zipf_s\": $OBS_ZIPF_S,"
  echo "  \"obs_recent_frac\": $OBS_RECENT_FRAC,"
  echo "  \"obs_datadir\": \"$OBS_DATADIR\","
  echo "  \"iostat_device\": \"$IOSTAT_DEVICE\","
  echo "  \"git_commit\": \"$(cd "$GETH_REPO" && git rev-parse HEAD 2>/dev/null || echo unknown)\","
  echo "  \"sysstat_version\": \"$(iostat -V 2>&1 | head -1)\","
  echo "  \"timestamp\": \"$TS\""
  echo "}"
} > "$OBS_OUTPUT_DIR/config.json"

# Optional: drop OS page cache (needs root). When run via sudo the file write
# is what requires privilege; refuse silently rather than fail if not allowed.
if [ "$DROP_CACHE" = "1" ]; then
  echo "[run] dropping OS page cache (echo 3 > /proc/sys/vm/drop_caches)"
  sync
  if [ "$(id -u)" -eq 0 ]; then
    echo 3 > /proc/sys/vm/drop_caches
  else
    sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches' || echo "[warn] drop_caches failed (no sudo?), continuing"
  fi
fi

# Background iostat: extended, decimal MB, timestamps, every 1s.
# LC_ALL=C forces English numerals so the timestamp parser works regardless
# of the system locale.
IOSTAT_LOG="$OBS_OUTPUT_DIR/iostat/iostat.txt"
LC_ALL=C iostat -xmt 1 "$IOSTAT_DEVICE" > "$IOSTAT_LOG" 2>&1 &
IOSTAT_PID=$!
echo "[run] iostat PID=$IOSTAT_PID → $IOSTAT_LOG"
sleep 1  # let iostat capture one sample before benchmark starts

# Optional: bpftrace per-inode page-cache fill counter. Used by
# parse_ebpf_pagecache.py to split proc_read_bytes between .sst (LSM) and
# .vlog (vlog). Requires sudo because tracepoint access is privileged.
EBPF_TRACE="${EBPF_TRACE:-0}"
if [ "$EBPF_TRACE" = "1" ]; then
  # Prefer SUDO_ASKPASS (non-tty automation friendly) then cached sudo then
  # interactive prompt. Pick the form once and reuse.
  if [ -n "$SUDO_ASKPASS" ] && [ -x "$SUDO_ASKPASS" ]; then
    SUDO="sudo -A"
  elif sudo -n true 2>/dev/null; then
    SUDO="sudo -n"
  else
    echo "[run] caching sudo for bpftrace; enter password if prompted"
    sudo -v && SUDO="sudo -n" \
      || { echo "[error] sudo unavailable; rerun with EBPF_TRACE=0 or set SUDO_ASKPASS"; exit 1; }
  fi
  mkdir -p "$OBS_OUTPUT_DIR/ebpf"
  EBPF_LOG="$OBS_OUTPUT_DIR/ebpf/pagecache.txt"
  # No in-BPF dev/inode filter (see ebpf_pagecache_ino.bt comments).
  # Raise BPFTRACE_MAP_KEYS_MAX well above the system-wide unique-inode
  # working set during the benchmark (~tens of thousands typical). With
  # the default 4096 the map fills during process startup with libs/binaries
  # and chaindata reads get silently dropped. Use `env` so the var survives
  # the sudo env_reset.
  $SUDO env BPFTRACE_MAP_KEYS_MAX=1000000 \
    bpftrace "$OBS_DIR/ebpf_pagecache_ino.bt" \
      > "$EBPF_LOG" 2>&1 &
  EBPF_LAUNCHER_PID=$!
  sleep 1  # let bpftrace attach the probe
  echo "[run] bpftrace launched (launcher pid=$EBPF_LAUNCHER_PID) → $EBPF_LOG"
fi

# Run the benchmark
STDOUT_LOG="$OBS_OUTPUT_DIR/stdout.log"
echo "[run] benchmark starting (mode=$OBS_MODE pattern=$OBS_PATTERN ratio=$OBS_RATIO nops=$OBS_NOPS)"

set +e
OBS_RUN=1 \
OBS_MODE="$OBS_MODE" OBS_PATTERN="$OBS_PATTERN" OBS_RATIO="$OBS_RATIO" \
OBS_NOPS="$OBS_NOPS" OBS_SEED="$OBS_SEED" OBS_ZIPF_S="$OBS_ZIPF_S" \
OBS_RECENT_FRAC="$OBS_RECENT_FRAC" \
OBS_DATADIR="$OBS_DATADIR" OBS_OUTPUT_DIR="$OBS_OUTPUT_DIR" \
  go test -run TestObservation -v -timeout 0 "$OBS_DIR" 2>&1 | tee "$STDOUT_LOG"
RC=${PIPESTATUS[0]}
set -e

# Stop iostat
sleep 1  # let iostat capture one sample after benchmark ends
kill -INT "$IOSTAT_PID" 2>/dev/null || true
wait "$IOSTAT_PID" 2>/dev/null || true

# Stop bpftrace and dump per-inode page-cache map, then snapshot the
# chaindata inode → filename table the parser needs to join on.
if [ "$EBPF_TRACE" = "1" ]; then
  # SIGINT the actual bpftrace process (the sudo wrapper's PID is not the
  # bpftrace PID; pgrep -f matches by command line). bpftrace auto-prints
  # all @maps before exit.
  $SUDO pkill -INT -f ebpf_pagecache_ino.bt 2>/dev/null || true
  # Give bpftrace time to walk the map and flush stdout.
  sleep 3
  wait "$EBPF_LAUNCHER_PID" 2>/dev/null || true
  echo "[run] bpftrace stopped"

  CHAINDATA="$OBS_DATADIR/geth/chaindata"
  INODE_MAP="$OBS_OUTPUT_DIR/ebpf/inode_map.csv"
  {
    echo "inode,path,type"
    for f in "$CHAINDATA"/*.sst; do
      [ -e "$f" ] || continue
      echo "$(stat -c '%i' "$f"),$f,sst"
    done
    for f in "$CHAINDATA"/*.vlog; do
      [ -e "$f" ] || continue
      echo "$(stat -c '%i' "$f"),$f,vlog"
    done
  } > "$INODE_MAP"
  echo "[run] inode map → $INODE_MAP ($(($(wc -l < "$INODE_MAP") - 1)) entries)"
fi

# Pull the benchmark window timestamps and summarize iostat over that range
START_NS=$(grep -E '^\[OBS\] start=' "$STDOUT_LOG" | head -1 | sed 's/^\[OBS\] start=//')
END_NS=$(grep -E '^\[OBS\] end=' "$STDOUT_LOG" | head -1 | sed 's/^\[OBS\] end=//')
echo "[run] benchmark window: $START_NS..$END_NS"

# iostat_summary.csv: parse the text iostat log, keep rows for IOSTAT_DEVICE
# within the [start,end] timestamp window. We rely on `iostat -t` printing one
# wall-clock header line above each block (e.g. "11/14/26 12:34:56" — locale
# may vary, so just count blocks by header line).
python3 - "$IOSTAT_LOG" "$IOSTAT_DEVICE" "$START_NS" "$END_NS" \
  "$OBS_OUTPUT_DIR/iostat/iostat_summary.csv" <<'PY' || echo "[warn] iostat summary failed"
import sys, re, datetime, csv

log, dev, start_ns, end_ns, out = sys.argv[1:]
start_ns = int(start_ns) if start_ns else 0
end_ns   = int(end_ns)   if end_ns   else 10**19

# iostat -t prints either "MM/DD/YY HH:MM:SS" or "MM/DD/YYYY HH:MM:SS AM/PM"
ts_re = re.compile(r'^\s*\d{2}/\d{2}/\d{2,4}\s+\d{2}:\d{2}:\d{2}(\s*[AP]M)?\s*$')

rows = []   # list of (ts_ns, fields...)
header = None
cur_ts = None
def parse_ts(s):
    s = s.strip()
    for fmt in ("%m/%d/%y %I:%M:%S %p", "%m/%d/%Y %I:%M:%S %p",
                "%m/%d/%y %H:%M:%S",    "%m/%d/%Y %H:%M:%S"):
        try:
            dt = datetime.datetime.strptime(s, fmt)
            return int(dt.timestamp() * 1e9)
        except ValueError:
            continue
    return None

with open(log) as f:
    for line in f:
        line = line.rstrip()
        if ts_re.match(line):
            cur_ts = parse_ts(line)
            continue
        if line.startswith("Device"):
            header = line.split()
            continue
        if header and cur_ts is not None and (line.startswith(dev + " ") or line.startswith(dev + "\t")):
            parts = line.split()
            if parts and parts[0] == dev:
                rows.append((cur_ts, parts))

# Filter to benchmark window
window = [r for r in rows if start_ns <= r[0] <= end_ns]
if not window:
    print(f"[warn] no iostat rows in window {start_ns}..{end_ns}; falling back to all rows ({len(rows)})", file=sys.stderr)
    window = rows

if not window or not header:
    open(out, "w").close()
    sys.exit(0)

# Index helpers
def col(name, default=None):
    try: return header.index(name)
    except ValueError: return default

idx = {k: col(k) for k in ("r/s","w/s","rMB/s","wMB/s","r_await","w_await","await","aqu-sz","%util")}

agg = {k: [] for k in idx if idx[k] is not None}
for _, parts in window:
    for k, i in idx.items():
        if i is None or i >= len(parts): continue
        try: agg[k].append(float(parts[i]))
        except ValueError: pass

def mean(xs): return sum(xs)/len(xs) if xs else 0.0
def maxv(xs): return max(xs)         if xs else 0.0

with open(out, "w", newline="") as f:
    w = csv.writer(f)
    w.writerow(["metric","mean","max","samples"])
    for k in ("r/s","w/s","rMB/s","wMB/s","r_await","w_await","await","aqu-sz","%util"):
        if k in agg:
            w.writerow([k, f"{mean(agg[k]):.4f}", f"{maxv(agg[k]):.4f}", len(agg[k])])
print(f"[run] iostat_summary.csv written ({len(window)} samples in window)")
PY

# Post-process ebpf log → per-engine SSD byte split.
if [ "$EBPF_TRACE" = "1" ]; then
  python3 "$OBS_DIR/parse_ebpf_pagecache.py" \
    --bpftrace-log "$OBS_OUTPUT_DIR/ebpf/pagecache.txt" \
    --inode-map "$OBS_OUTPUT_DIR/ebpf/inode_map.csv" \
    --out "$OBS_OUTPUT_DIR/ebpf/ebpf_bytes_summary.json" \
    || echo "[warn] ebpf parse failed"
fi

echo ""
echo "[done] Output: $OBS_OUTPUT_DIR"
echo "       rc=$RC"
if [ "$EBPF_TRACE" != "1" ]; then
  echo "       (hint: rerun with EBPF_TRACE=1 to split SSD bytes between LSM and vlog; needs sudo)"
fi

# Refresh the cross-run comparison workbook (does not affect $RC).
python3 "$OBS_DIR/buildObservationExcel.py" -d "$OBS_DIR" \
    || echo "[warn] observation_summary.xlsx build failed"

exit $RC
