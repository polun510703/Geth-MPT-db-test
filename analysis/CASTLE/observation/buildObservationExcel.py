#!/usr/bin/env python3
"""
buildObservationExcel.py — Aggregate observation test runs into one comparison .xlsx.

Usage:
  python3 buildObservationExcel.py [run_dir_or_glob ...] [-d DIR] [-o OUT] [--exclude PAT]

  run_dir_or_glob : positional paths or globs of specific raOutput_obs_* dirs.
                    Relative paths resolve under -d. If none given, every
                    raOutput_obs_* under -d is included.
  -d / --dir      : observation root directory (default: script's own dir)
  -o / --output   : output workbook path. Default depends on filter mode:
                      no filter -> <dir>/observation_summary.xlsx
                      subset    -> <dir>/observation_summary_subset.xlsx
  --exclude PAT   : glob (repeatable) removed from the final set.

Each run folder is expected to contain:
  config.json
  latency/summary.json              (with the new read_amp.* block when available)
  latency/badger_timeseries.csv     (optional, 1 Hz cumulative read counters)
  iostat/iostat_summary.csv
  badger_metrics/badger_metrics_obs_*.csv

Sheets emitted:
  Overview    one row per run, curated KPIs, color-scale highlighting
  Latency     overall/tx/acct percentile breakdown
  IO          iostat metrics mean + max
  Badger      raw badger CSV metrics + legacy derived columns
  ReadAmp     per-level read decomposition + RA ratios (new)
  Timeseries  per-run cumulative LSM/vlog/proc read curves (new)
  Charts      bar charts referencing Overview
"""

import argparse
import csv
import glob
import json
import os
import re
import sys

try:
    from openpyxl import Workbook
    from openpyxl.chart import BarChart, Reference, ScatterChart, Series
    from openpyxl.chart.marker import Marker
    from openpyxl.styles import Alignment, Font, PatternFill
    from openpyxl.utils import get_column_letter
except ImportError:
    sys.stderr.write(
        "[error] openpyxl not installed. Install it with:\n"
        "          pip install --user openpyxl\n"
    )
    sys.exit(1)


RUN_DIR_RE = re.compile(
    r"^raOutput_obs_(?P<mode>\w+?)_(?P<pattern>\w+?)_"
    r"(?P<rA>\d+)_(?P<rB>\d+)_(?P<ts>\d{8}_\d{6})$"
)

HEADER_FILL = PatternFill("solid", fgColor="305496")
GROUP_FILL = PatternFill("solid", fgColor="8EA9DB")
HEADER_FONT = Font(bold=True, color="FFFFFF")


# --------------------------------------------------------------------------- #
# Readers
# --------------------------------------------------------------------------- #
def read_config(run_dir):
    path = os.path.join(run_dir, "config.json")
    with open(path) as f:
        return json.load(f)


def _flatten(obj, prefix=""):
    out = {}
    for k, v in obj.items():
        key = "%s_%s" % (prefix, k) if prefix else k
        if isinstance(v, dict):
            out.update(_flatten(v, key))
        else:
            out[key] = v
    return out


def read_summary(run_dir):
    """Recursively flatten latency/summary.json so read_amp.* is reachable."""
    path = os.path.join(run_dir, "latency", "summary.json")
    with open(path) as f:
        raw = json.load(f)
    return _flatten(raw)


def read_iostat(run_dir):
    path = os.path.join(run_dir, "iostat", "iostat_summary.csv")
    out = {}
    with open(path) as f:
        for row in csv.DictReader(f):
            m = row["metric"].strip()
            out["%s_mean" % m] = _num(row.get("mean"))
            out["%s_max" % m] = _num(row.get("max"))
    return out


def read_badger(run_dir):
    matches = glob.glob(
        os.path.join(run_dir, "badger_metrics", "badger_metrics_obs_*.csv")
    )
    if not matches:
        return {}
    out = {}
    with open(sorted(matches)[-1]) as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or line.startswith("metric,"):
                continue
            key, _, val = line.partition(",")
            key, val = key.strip(), val.strip()
            if val.startswith("{"):
                try:
                    d = json.loads(val)
                except json.JSONDecodeError:
                    continue
                if not d:
                    continue
                if len(d) == 1 and list(d.keys())[0].startswith("/"):
                    out[key] = list(d.values())[0]
                else:
                    for sk, sv in d.items():
                        out["%s_%s" % (key, sk)] = sv
            else:
                out[key] = _num(val)
    return out


def read_ebpf_summary(run_dir):
    """ebpf/ebpf_bytes_summary.json -> dict (or empty if absent)."""
    path = os.path.join(run_dir, "ebpf", "ebpf_bytes_summary.json")
    if not os.path.exists(path):
        return {}
    with open(path) as f:
        return json.load(f)


def read_timeseries(run_dir):
    """latency/badger_timeseries.csv -> list of dicts (or empty if absent)."""
    path = os.path.join(run_dir, "latency", "badger_timeseries.csv")
    if not os.path.exists(path):
        return []
    rows = []
    with open(path) as f:
        for row in csv.DictReader(f):
            try:
                rows.append({k: int(v) for k, v in row.items()})
            except (ValueError, TypeError):
                continue
    return rows


def _num(s):
    if s is None:
        return None
    try:
        f = float(s)
        return int(f) if f.is_integer() else f
    except (ValueError, AttributeError):
        return s


def _div(a, b):
    try:
        if a is None or b in (None, 0):
            return None
        return a / b
    except TypeError:
        return None


# --------------------------------------------------------------------------- #
# Run model
# --------------------------------------------------------------------------- #
class Run:
    def __init__(self, run_dir):
        m = RUN_DIR_RE.match(os.path.basename(run_dir))
        self.dir = run_dir
        self.mode = m.group("mode")
        self.pattern = m.group("pattern")
        self.ratio = "%s:%s" % (m.group("rA"), m.group("rB"))
        self.ts = m.group("ts")
        self.label = "%s_%s_%s" % (self.mode, self.ratio.replace(":", "-"), self.pattern)
        self.cfg = read_config(run_dir)
        self.sm = read_summary(run_dir)
        self.io = read_iostat(run_dir)
        self.bg = read_badger(run_dir)
        self.ebpf = read_ebpf_summary(run_dir)
        self.ts_rows = read_timeseries(run_dir)
        self.derived = self._derive()

    def _derive(self):
        sm, bg = self.sm, self.bg

        def _ra(key, fallback=None):
            v = sm.get("read_amp_" + key)
            return v if v is not None else fallback

        # Per-level get counts: prefer summary.read_amp, else badger CSV.
        l0 = _ra("badger_lsm_gets_by_level_l0", bg.get("badger_get_num_lsm_l0") or 0)
        l5 = _ra("badger_lsm_gets_by_level_l5", bg.get("badger_get_num_lsm_l5") or 0)
        l6 = _ra("badger_lsm_gets_by_level_l6", bg.get("badger_get_num_lsm_l6") or 0)
        # Per-level bloom hits
        b0 = _ra("badger_bloom_filter_hits_l0", bg.get("badger_hit_num_lsm_bloom_filter_l0"))
        b5 = _ra("badger_bloom_filter_hits_l5", bg.get("badger_hit_num_lsm_bloom_filter_l5"))
        b6 = _ra("badger_bloom_filter_hits_l6", bg.get("badger_hit_num_lsm_bloom_filter_l6"))
        bloom_all = _ra("badger_bloom_filter_hits_DoesNotHave_ALL",
                        bg.get("badger_hit_num_lsm_bloom_filter_DoesNotHave_ALL"))
        bloom_hit = _ra("badger_bloom_filter_hits_DoesNotHave_HIT",
                        bg.get("badger_hit_num_lsm_bloom_filter_DoesNotHave_HIT"))
        # Byte totals
        lsm_read = _ra("badger_lsm_bytes_read", bg.get("badger_read_bytes_lsm"))
        vlog_read = _ra("badger_vlog_bytes_read", bg.get("badger_read_bytes_vlog"))
        logical = _ra("logical_bytes_returned")
        proc_read = _ra("proc_read_bytes")
        proc_rchar = _ra("proc_rchar_bytes")
        # Engine/device sizes (still only in badger CSV)
        vlog_sz = bg.get("badger_size_bytes_vlog")
        lsm_sz = bg.get("badger_size_bytes_lsm")

        return {
            "ra_engine": _ra("ra_engine"),
            "ra_device_over_engine": _ra("ra_device_over_engine"),
            "ra_device_over_logical": _ra("ra_device_over_logical"),
            "logical_GB": _div(logical, 1e9),
            "lsm_read_GB": _div(lsm_read, 1e9),
            "vlog_read_GB": _div(vlog_read, 1e9),
            "lsm_ssd_read_GB": _div(self.ebpf.get("lsm_ssd_bytes"), 1e9),
            "vlog_ssd_read_GB": _div(self.ebpf.get("vlog_ssd_bytes"), 1e9),
            "proc_read_GB": _div(proc_read, 1e9),
            "proc_rchar_GB": _div(proc_rchar, 1e9),
            "vlog_size_GB": _div(vlog_sz, 1e9),
            "lsm_size_GB": _div(lsm_sz, 1e9),
            "vlog_lsm_ratio": _div(vlog_sz, lsm_sz),
            "lsm_get_l0": l0,
            "lsm_get_l5": l5,
            "lsm_get_l6": l6,
            "lsm_get_total": (l0 or 0) + (l5 or 0) + (l6 or 0),
            "bloom_l0": b0,
            "bloom_l5": b5,
            "bloom_l6": b6,
            "bloom_DoesNotHave_ALL": bloom_all,
            "bloom_DoesNotHave_HIT": bloom_hit,
            "bloom_hit_rate": _div(bloom_hit, bloom_all),
            "total_wall_s": _div(sm.get("total_wall_ns"), 1e9),
            # Get-phase read time (engine perspective). summary.json stores ns;
            # surface seconds in Excel, plus the vlog time fraction (the direct
            # "is the bottleneck in vlog?" signal) and avg per-read (us).
            "lsm_read_time_s": _div(_ra("lsm_read_time_ns"), 1e9),
            "vlog_read_time_s": _div(_ra("vlog_read_time_ns"), 1e9),
            "vlog_time_frac": _ra("vlog_time_frac"),
            "lsm_avg_read_us": _div(_ra("lsm_avg_read_ns"), 1e3),
            "vlog_avg_read_us": _div(_ra("vlog_avg_read_ns"), 1e3),
        }

    def ratio_key(self):
        a, b = self.ratio.split(":")
        return (int(a), int(b))


# --------------------------------------------------------------------------- #
# Selection
# --------------------------------------------------------------------------- #
def _is_valid_run(path):
    if not os.path.isdir(path):
        return False
    if not RUN_DIR_RE.match(os.path.basename(path)):
        return False
    required = [
        os.path.join(path, "config.json"),
        os.path.join(path, "latency", "summary.json"),
        os.path.join(path, "iostat", "iostat_summary.csv"),
    ]
    return all(os.path.exists(p) for p in required)


def resolve_selection(obs_dir, positional, excludes):
    """Return sorted list of absolute run directories."""
    selected = set()
    if positional:
        for p in positional:
            if os.path.isabs(p):
                matches = glob.glob(p)
            else:
                matches = glob.glob(os.path.join(obs_dir, p))
            if not matches:
                sys.stderr.write("[warn] no match for %r\n" % p)
                continue
            for m in matches:
                selected.add(os.path.abspath(m))
    else:
        for p in glob.glob(os.path.join(obs_dir, "raOutput_obs_*")):
            selected.add(os.path.abspath(p))

    for pat in excludes or []:
        if os.path.isabs(pat):
            drop = glob.glob(pat)
        else:
            drop = glob.glob(os.path.join(obs_dir, pat))
        for d in drop:
            selected.discard(os.path.abspath(d))

    valid = []
    for p in selected:
        if not _is_valid_run(p):
            sys.stderr.write("[warn] skip %s (not a valid run dir)\n"
                             % os.path.basename(p))
            continue
        valid.append(p)
    return sorted(valid)


def build_runs(run_dirs):
    from collections import Counter
    runs = []
    for path in run_dirs:
        try:
            runs.append(Run(path))
        except Exception as e:  # noqa: BLE001
            sys.stderr.write("[warn] skip %s (%s)\n" % (os.path.basename(path), e))
    runs.sort(key=lambda r: (r.pattern, r.ratio_key(), r.mode, r.ts))
    counts = Counter((r.mode, r.pattern, r.ratio) for r in runs)
    for r in runs:
        if counts[(r.mode, r.pattern, r.ratio)] > 1:
            r.label = "%s_%s" % (r.label, r.ts[-6:])
    return runs


# --------------------------------------------------------------------------- #
# Sheet helpers
# --------------------------------------------------------------------------- #
def _style_header(ws, row, ncols, fill=HEADER_FILL):
    for c in range(1, ncols + 1):
        cell = ws.cell(row=row, column=c)
        cell.fill = fill
        cell.font = HEADER_FONT
        cell.alignment = Alignment(horizontal="center", vertical="center")


def _autosize(ws, ncols):
    for c in range(1, ncols + 1):
        width = 0
        for cell in ws[get_column_letter(c)]:
            if cell.value is not None:
                width = max(width, len(str(cell.value)))
        ws.column_dimensions[get_column_letter(c)].width = min(max(width + 2, 9), 40)


def _write_columnar(ws, runs, rows, start_row=1, start_col=1):
    """Columnar layout: each run is a column; each metric is a row.

    Row `start_row`     = "metric" + run labels.
    Col `start_col`     = metric labels (one per row).
    Cell (start_row+i, start_col+1+j) = accessor(runs[j]) for rows[i].

    Returns (last_row, last_col).
    """
    n_runs = len(runs)
    ws.cell(row=start_row, column=start_col, value="metric")
    for j, run in enumerate(runs, start=start_col + 1):
        ws.cell(row=start_row, column=j, value=run.label)
    _style_header(ws, start_row, start_col + n_runs)
    for i, (label, acc) in enumerate(rows, start=start_row + 1):
        cell = ws.cell(row=i, column=start_col, value=label)
        cell.fill = GROUP_FILL
        cell.font = HEADER_FONT
        cell.alignment = Alignment(horizontal="left", vertical="center")
        for j, run in enumerate(runs, start=start_col + 1):
            ws.cell(row=i, column=j, value=acc(run))
    last_col = start_col + n_runs
    last_row = start_row + len(rows)
    ws.freeze_panes = ws.cell(row=start_row + 1, column=start_col + 1)
    _autosize(ws, last_col)
    return last_row, last_col


def _ns_to_us(src, key):
    def acc(r):
        v = getattr(r, src).get(key)
        return round(v / 1000.0, 1) if isinstance(v, (int, float)) else None
    return acc


def _rnd(src, key, n=3):
    def acc(r):
        v = getattr(r, src).get(key)
        return round(v, n) if isinstance(v, (int, float)) else v
    return acc


def _get(src, key):
    return lambda r: getattr(r, src).get(key)


# --------------------------------------------------------------------------- #
# Sheet builders (columnar: each run = one column, each metric = one row)
# --------------------------------------------------------------------------- #
# Overview row indices are referenced from the Charts sheet. If you reorder
# these rows, update build_charts() too.
OVERVIEW_METRIC_ROWS = [
    ("mode", lambda r: r.mode, False),
    ("ratio", lambda r: r.ratio, False),
    ("qps", _rnd("sm", "qps", 1), True),
    ("total_wall_s", _rnd("derived", "total_wall_s", 1), True),
    ("overall_p50_us", _ns_to_us("sm", "overall_p50"), True),
    ("overall_p99_us", _ns_to_us("sm", "overall_p99"), True),
    ("tx_p99_us", _ns_to_us("sm", "tx_p99"), True),
    ("acct_p99_us", _ns_to_us("sm", "acct_p99"), True),
    ("rMB/s", _rnd("io", "rMB/s_mean", 1), True),
    ("r_await", _rnd("io", "r_await_mean", 3), True),
    ("%util_max", _rnd("io", "%util_max", 1), True),
    ("aqu_sz", _rnd("io", "aqu-sz_mean", 3), True),
    ("ra_device_over_logical", _rnd("derived", "ra_device_over_logical", 3), True),
    ("lsm_read_time_s", _rnd("derived", "lsm_read_time_s", 2), True),
    ("vlog_read_time_s", _rnd("derived", "vlog_read_time_s", 2), True),
    ("vlog_time_frac", _rnd("derived", "vlog_time_frac", 3), True),
    ("lsm_avg_read_us", _rnd("derived", "lsm_avg_read_us", 2), True),
    ("vlog_avg_read_us", _rnd("derived", "vlog_avg_read_us", 2), True),
    ("vlog_GB", _rnd("derived", "vlog_size_GB", 2), True),
    ("lsm_GB", _rnd("derived", "lsm_size_GB", 2), True),
    ("lsm_ssd_read_GB", _rnd("derived", "lsm_ssd_read_GB", 2), True),
    ("vlog_ssd_read_GB", _rnd("derived", "vlog_ssd_read_GB", 2), True),
    ("lsm_get_total", _get("derived", "lsm_get_total"), True),
    ("bloom_hit_rate", _rnd("derived", "bloom_hit_rate", 4), True),
    ("git_commit", lambda r: (r.cfg.get("git_commit") or "")[:10], False),
]


def build_overview(wb, runs):
    ws = wb.active
    ws.title = "Overview"
    rows = [(label, acc) for label, acc, _ in OVERVIEW_METRIC_ROWS]
    _write_columnar(ws, runs, rows)
    return ws


def build_latency(wb, runs):
    ws = wb.create_sheet("Latency")
    groups = ["overall", "tx", "acct"]
    stats = ["count", "min", "mean", "p50", "p90", "p95", "p99", "p999"]
    n_runs = len(runs)
    # Row 1 header: col 1 = "group", col 2 = "stat", cols 3.. = run labels
    ws.cell(row=1, column=1, value="group")
    ws.cell(row=1, column=2, value="stat")
    for j, run in enumerate(runs, start=3):
        ws.cell(row=1, column=j, value=run.label)
    _style_header(ws, 1, 2 + n_runs)

    row = 2
    for g in groups:
        gcell = ws.cell(row=row, column=1, value=g)
        gcell.fill = GROUP_FILL
        gcell.font = HEADER_FONT
        gcell.alignment = Alignment(horizontal="center", vertical="center")
        if len(stats) > 1:
            ws.merge_cells(start_row=row, start_column=1,
                           end_row=row + len(stats) - 1, end_column=1)
        for s in stats:
            scell = ws.cell(row=row, column=2, value=s)
            scell.fill = HEADER_FILL
            scell.font = HEADER_FONT
            for j, run in enumerate(runs, start=3):
                v = run.sm.get("%s_%s" % (g, s))
                if s != "count" and isinstance(v, (int, float)):
                    v = round(v / 1000.0, 1)
                ws.cell(row=row, column=j, value=v)
            row += 1
    ws.freeze_panes = "C2"
    _autosize(ws, 2 + n_runs)
    return ws


def build_io(wb, runs):
    ws = wb.create_sheet("IO")
    metrics = ["r/s", "w/s", "rMB/s", "wMB/s", "r_await", "w_await",
               "await", "aqu-sz", "%util"]
    rows = []
    for m in metrics:
        rows.append((m + "_mean", _rnd("io", m + "_mean", 3)))
        rows.append((m + "_max", _rnd("io", m + "_max", 3)))
    _write_columnar(ws, runs, rows)
    return ws


def build_badger(wb, runs):
    ws = wb.create_sheet("Badger")
    raw_keys, seen = [], set()
    for r in runs:
        for k in r.bg:
            if k not in seen:
                seen.add(k)
                raw_keys.append(k)
    derived_keys = ["vlog_size_GB", "lsm_size_GB", "vlog_lsm_ratio",
                    "lsm_get_l0", "lsm_get_l5", "lsm_get_l6", "lsm_get_total",
                    "bloom_hit_rate"]
    rows = [("mode", lambda r: r.mode),
            ("ratio", lambda r: r.ratio)]
    for k in raw_keys:
        rows.append((k, (lambda key: lambda r: r.bg.get(key))(k)))
    for k in derived_keys:
        rows.append(("* " + k, (lambda key: lambda r: (
            round(r.derived[key], 4) if isinstance(r.derived.get(key), float)
            else r.derived.get(key)))(k)))
    _write_columnar(ws, runs, rows)
    return ws


def build_readamp(wb, runs):
    ws = wb.create_sheet("ReadAmp")
    # Group bands live in col 1 (merged vertically across sub-metric rows).
    groups = [
        ("Bytes(GB)",
         [("logical_GB", "logical_GB"),
          ("lsm_read_GB", "lsm_read_GB"),
          ("vlog_read_GB", "vlog_read_GB"),
          ("lsm_ssd_read_GB", "lsm_ssd_read_GB"),
          ("vlog_ssd_read_GB", "vlog_ssd_read_GB"),
          ("proc_read_GB", "proc_read_GB")]),
        ("LSM gets/level",
         [("l0", "lsm_get_l0"),
          ("l5", "lsm_get_l5"),
          ("l6", "lsm_get_l6"),
          ("total", "lsm_get_total")]),
        ("Bloom hits/level",
         [("l0", "bloom_l0"),
          ("l5", "bloom_l5"),
          ("l6", "bloom_l6"),
          ("DoesNotHave_ALL", "bloom_DoesNotHave_ALL"),
          ("DoesNotHave_HIT", "bloom_DoesNotHave_HIT"),
          ("hit_rate", "bloom_hit_rate")]),
        ("RA ratios",
         [("ra_engine", "ra_engine"),
          ("ra_device_over_engine", "ra_device_over_engine"),
          ("ra_device_over_logical", "ra_device_over_logical")]),
    ]
    n_runs = len(runs)
    ws.cell(row=1, column=1, value="group")
    ws.cell(row=1, column=2, value="metric")
    for j, run in enumerate(runs, start=3):
        ws.cell(row=1, column=j, value=run.label)
    _style_header(ws, 1, 2 + n_runs)

    row = 2
    for group_name, sub in groups:
        gcell = ws.cell(row=row, column=1, value=group_name)
        gcell.fill = GROUP_FILL
        gcell.font = HEADER_FONT
        gcell.alignment = Alignment(horizontal="center", vertical="center")
        if len(sub) > 1:
            ws.merge_cells(start_row=row, start_column=1,
                           end_row=row + len(sub) - 1, end_column=1)
        for sub_name, dkey in sub:
            scell = ws.cell(row=row, column=2, value=sub_name)
            scell.fill = HEADER_FILL
            scell.font = HEADER_FONT
            for j, run in enumerate(runs, start=3):
                v = run.derived.get(dkey)
                if isinstance(v, float):
                    v = round(v, 4)
                ws.cell(row=row, column=j, value=v)
            row += 1
    ws.freeze_panes = "C2"
    _autosize(ws, 2 + n_runs)
    return ws


def build_timeseries(wb, runs):
    ws = wb.create_sheet("Timeseries")
    block_width = 4
    gap = 1
    # Per-run block: cols [label-merged-row1, headers-row2, data-row3+]
    block_starts = []  # list of (run, col_start, first_data_row, last_data_row)
    col = 1
    for run in runs:
        if not run.ts_rows:
            continue
        ws.cell(row=1, column=col, value=run.label).fill = GROUP_FILL
        ws.cell(row=1, column=col).font = HEADER_FONT
        ws.cell(row=1, column=col).alignment = Alignment(horizontal="center")
        ws.merge_cells(start_row=1, start_column=col,
                       end_row=1, end_column=col + block_width - 1)
        headers = ["elapsed_s", "lsm_GB", "vlog_GB", "proc_read_GB"]
        for j, h in enumerate(headers):
            cell = ws.cell(row=2, column=col + j, value=h)
            cell.fill = HEADER_FILL
            cell.font = HEADER_FONT
            cell.alignment = Alignment(horizontal="center")
        ts0 = run.ts_rows[0]["ts_ns"]
        for i, row in enumerate(run.ts_rows, start=3):
            ws.cell(row=i, column=col, value=round((row["ts_ns"] - ts0) / 1e9, 2))
            ws.cell(row=i, column=col + 1, value=round(row.get("badger_read_bytes_lsm", 0) / 1e9, 4))
            ws.cell(row=i, column=col + 2, value=round(row.get("badger_read_bytes_vlog", 0) / 1e9, 4))
            ws.cell(row=i, column=col + 3, value=round(row.get("proc_read_bytes", 0) / 1e9, 4))
        last_data_row = 2 + len(run.ts_rows)
        block_starts.append((run, col, 3, last_data_row))
        col += block_width + gap

    if not block_starts:
        ws.cell(row=1, column=1, value="(no badger_timeseries.csv found in any run)")
        return ws

    # Auto-size only the populated columns.
    _autosize(ws, col - 1)

    def make_chart(title, y_offset, anchor):
        ch = ScatterChart()
        ch.title = title
        ch.scatterStyle = "line"
        ch.x_axis.title = "elapsed_s"
        ch.y_axis.title = "GB (cumulative)"
        ch.height = 10
        ch.width = 20
        for run, c_start, r0, r1 in block_starts:
            xs = Reference(ws, min_col=c_start, min_row=r0, max_row=r1)
            ys = Reference(ws, min_col=c_start + y_offset, min_row=r0, max_row=r1)
            s = Series(ys, xs, title=run.label)
            # Hide markers; "line" scatterStyle keeps the connecting line.
            s.marker = Marker(symbol="none")
            ch.series.append(s)
        ws.add_chart(ch, anchor)

    # Place charts well below the data so they never overlap (data could be
    # hundreds of rows; pick a fixed safe anchor column to the right).
    chart_col = get_column_letter(col + 1)
    make_chart("Cumulative LSM bytes read (GB)", y_offset=1, anchor="%s2" % chart_col)
    make_chart("Cumulative proc_read bytes (GB)", y_offset=3, anchor="%s22" % chart_col)
    return ws


def build_charts(wb, runs, overview):
    ws = wb.create_sheet("Charts")
    n = len(runs)
    if n == 0:
        return
    # In columnar Overview: row 1 = run labels (cols 2..n+1), each metric is a row.
    cats = Reference(overview, min_col=2, max_col=1 + n, min_row=1, max_row=1)

    # Resolve metric rows by label so charts stay correct if the order shifts.
    row_of = {label: i for i, (label, _, _) in enumerate(
        OVERVIEW_METRIC_ROWS, start=2)}

    def bar(title, metric_labels, anchor):
        ch = BarChart()
        ch.title = title
        ch.type = "col"
        ch.height = 7.5
        ch.width = 15
        for label in metric_labels:
            ri = row_of[label]
            # min_col=1 includes the metric name as the series title.
            data = Reference(overview, min_col=1, max_col=1 + n,
                             min_row=ri, max_row=ri)
            ch.add_data(data, titles_from_data=True, from_rows=True)
        ch.set_categories(cats)
        ws.add_chart(ch, anchor)

    bar("QPS", ["qps"], "B2")
    bar("Overall p99 latency (us)", ["overall_p99_us"], "B18")
    bar("End-to-end read amp (ra_device_over_logical)",
        ["ra_device_over_logical"], "L2")
    bar("vlog vs lsm size (GB)", ["vlog_GB", "lsm_GB"], "L18")
    ws["A1"] = "Charts read from the Overview sheet."
    ws["A1"].font = Font(bold=True)


# --------------------------------------------------------------------------- #
# Main
# --------------------------------------------------------------------------- #
def main():
    script_dir = os.path.dirname(os.path.abspath(__file__))
    ap = argparse.ArgumentParser(
        description="Aggregate observation runs into one .xlsx",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "Examples:\n"
            "  buildObservationExcel.py                          # all runs\n"
            "  buildObservationExcel.py raOutput_obs_nosep_uniform_1_1_20260521_170200\n"
            "  buildObservationExcel.py 'raOutput_obs_*_uniform_1_1_*' -o compare_11.xlsx\n"
            "  buildObservationExcel.py --exclude 'raOutput_obs_*_20260511_*'\n"
        ),
    )
    ap.add_argument("runs", nargs="*",
                    help="run dirs or globs (under -d); omit for all")
    ap.add_argument("-d", "--dir", default=script_dir,
                    help="observation root dir (default: script dir)")
    ap.add_argument("-o", "--output",
                    help="output .xlsx path (defaults differ for full/subset)")
    ap.add_argument("--exclude", action="append", default=[],
                    help="glob to exclude (repeatable)")
    args = ap.parse_args()

    obs_dir = os.path.abspath(args.dir)
    subset_mode = bool(args.runs or args.exclude)
    out = args.output or os.path.join(
        obs_dir,
        "observation_summary_subset.xlsx" if subset_mode else "observation_summary.xlsx",
    )

    run_dirs = resolve_selection(obs_dir, args.runs, args.exclude)
    runs = build_runs(run_dirs)
    if not runs:
        sys.stderr.write("[error] no valid raOutput_obs_* runs in %s\n" % obs_dir)
        sys.exit(1)

    filter_desc = (
        "subset: %d positional, %d exclude" % (len(args.runs), len(args.exclude))
        if subset_mode else "all"
    )
    print("[scan] using %d runs from %s (filter: %s)"
          % (len(runs), obs_dir, filter_desc))

    wb = Workbook()
    overview = build_overview(wb, runs)
    build_latency(wb, runs)
    build_io(wb, runs)
    build_badger(wb, runs)
    build_readamp(wb, runs)
    build_timeseries(wb, runs)
    build_charts(wb, runs, overview)
    wb.save(out)

    print("[done] %d runs -> %s" % (len(runs), out))
    for r in runs:
        ra = r.derived.get("ra_device_over_logical")
        ra_str = "n/a" if ra is None else "%.2f" % ra
        print("        %-26s qps=%-8s ra_dev_over_logical=%s" % (
            r.label, round(r.sm.get("qps", 0), 1), ra_str))


if __name__ == "__main__":
    main()
