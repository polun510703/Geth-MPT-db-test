#!/usr/bin/env python3
"""parse_ebpf_pagecache.py

Aggregate the @pages map produced by ebpf_pagecache_ino.bt and split SSD
read bytes by file type (.sst → LSM, .vlog → vlog) using a snapshot of
inode → filename for the chaindata directory.

Inputs:
  --bpftrace-log   bpftrace stdout. Contains lines like:
                     @pages[<s_dev>, <i_ino>]: <pages>
                   where <pages> is folio-corrected page count.
  --inode-map      CSV "inode,path,type" produced by run_observation.sh.

Output:
  --out  JSON {lsm_ssd_bytes, vlog_ssd_bytes, traced_total_bytes,
              chaindata_other_bytes, page_size, per_sst_bytes_top,
              per_vlog_bytes_top, unmatched_top, notes}

Notes:
- One page = 4096 bytes (PAGE_SIZE on x86_64).
- bpftrace captures system-wide page-cache fills; "unmatched" entries
  belong to other inodes (other files / devices) and are summed under
  unmatched_top for debugging. They are NOT added to LSM/vlog/total.
- The relevant sanity check is `lsm_ssd_bytes + vlog_ssd_bytes` vs
  summary.json.read_amp.proc_read_bytes — they should agree within ~10%
  since proc_read_bytes is the per-process counter and chaindata is the
  dominant disk read source during the benchmark.
"""

import argparse
import json
import re
import sys
from collections import defaultdict
from pathlib import Path

PAGE_SIZE = 4096

# bpftrace v0.14 prints tracepoint-arg map keys as a tuple even when only one
# field is used as the key. Concretely, `@pages[args->i_ino] = sum(...)` over
# tracepoint:filemap:mm_filemap_add_to_page_cache renders as either
#   @pages[<s_dev>, <i_ino>]: <pages>     (observed shape on bpftrace 0.14)
#   @pages[<i_ino>]: <pages>              (defensive — single-key alt shape)
# Accept both. The dev_t filter in the .bt script already restricts to
# chaindata's device, so s_dev is redundant — we just take i_ino.
LINE_RE_DUAL = re.compile(r'^@pages\[\s*(\d+)\s*,\s*(\d+)\s*\]:\s*(\d+)\s*$')
LINE_RE_SINGLE = re.compile(r'^@pages\[\s*(\d+)\s*\]:\s*(\d+)\s*$')


def parse_bpftrace_log(path: Path) -> dict:
    """Return {ino: pages} from a bpftrace dump. Tolerant of single- or
    dual-key @pages render formats."""
    out: dict[int, int] = {}
    with path.open() as f:
        for line in f:
            s = line.strip()
            m = LINE_RE_DUAL.match(s)
            if m:
                # group(1) is s_dev (dev filter has already constrained it);
                # group(2) is i_ino; group(3) is pages.
                ino = int(m.group(2))
                pages = int(m.group(3))
            else:
                m = LINE_RE_SINGLE.match(s)
                if not m:
                    continue
                ino = int(m.group(1))
                pages = int(m.group(2))
            out[ino] = out.get(ino, 0) + pages
    return out


def parse_inode_map(path: Path) -> dict:
    """Return {ino: (filename, type)} from inode,path,type CSV."""
    out: dict[int, tuple[str, str]] = {}
    with path.open() as f:
        header = f.readline().rstrip("\n").split(",")
        try:
            i_ino = header.index("inode")
            i_path = header.index("path")
            i_type = header.index("type")
        except ValueError as e:
            raise SystemExit(f"inode_map header missing field: {e}") from e
        for line in f:
            parts = line.rstrip("\n").split(",", 2)
            if len(parts) < 3:
                continue
            try:
                ino = int(parts[i_ino])
            except ValueError:
                continue
            out[ino] = (parts[i_path], parts[i_type])
    return out


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--bpftrace-log", required=True, type=Path)
    ap.add_argument("--inode-map", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--top", type=int, default=10,
                    help="How many top sst/vlog files to list (default 10)")
    args = ap.parse_args(argv[1:])

    if not args.bpftrace_log.exists():
        raise SystemExit(f"bpftrace log not found: {args.bpftrace_log}")
    if not args.inode_map.exists():
        raise SystemExit(f"inode map not found: {args.inode_map}")

    pages_by_ino = parse_bpftrace_log(args.bpftrace_log)
    ino_to_file = parse_inode_map(args.inode_map)

    lsm_pages = 0
    vlog_pages = 0
    chaindata_other_pages = 0  # inode in map but type unknown
    traced_total_pages = 0
    per_sst: dict[str, int] = defaultdict(int)
    per_vlog: dict[str, int] = defaultdict(int)
    unmatched: dict[int, int] = defaultdict(int)

    for ino, pages in pages_by_ino.items():
        traced_total_pages += pages
        if ino in ino_to_file:
            path, ftype = ino_to_file[ino]
            if ftype == "sst":
                lsm_pages += pages
                per_sst[path] += pages
            elif ftype == "vlog":
                vlog_pages += pages
                per_vlog[path] += pages
            else:
                chaindata_other_pages += pages
        else:
            unmatched[ino] += pages

    def top_n(d, n):
        return dict(sorted(d.items(), key=lambda kv: -kv[1])[:n])

    summary = {
        "page_size": PAGE_SIZE,
        "lsm_ssd_bytes": lsm_pages * PAGE_SIZE,
        "vlog_ssd_bytes": vlog_pages * PAGE_SIZE,
        "chaindata_other_bytes": chaindata_other_pages * PAGE_SIZE,
        "traced_total_bytes": traced_total_pages * PAGE_SIZE,
        "lsm_pages": lsm_pages,
        "vlog_pages": vlog_pages,
        "traced_total_pages": traced_total_pages,
        "n_sst_files_touched": len(per_sst),
        "n_vlog_files_touched": len(per_vlog),
        "per_sst_bytes_top": {
            p: pgs * PAGE_SIZE for p, pgs in top_n(per_sst, args.top).items()
        },
        "per_vlog_bytes_top": {
            p: pgs * PAGE_SIZE for p, pgs in top_n(per_vlog, args.top).items()
        },
        "unmatched_top": [
            {"i_ino": k, "bytes": v * PAGE_SIZE}
            for k, v in sorted(unmatched.items(), key=lambda kv: -kv[1])[:args.top]
        ],
        "notes": (
            "lsm/vlog are SSD bytes read into the page cache, attributed by "
            "inode. unmatched lines are page-cache fills on other inodes "
            "(other files / devices) captured because bpftrace is "
            "system-wide. The right sanity check is "
            "lsm_ssd_bytes + vlog_ssd_bytes vs "
            "summary.json.read_amp.proc_read_bytes."
        ),
    }

    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(summary, indent=2))
    print(f"[ebpf-parse] wrote {args.out}")
    print(f"[ebpf-parse] lsm_ssd_bytes  = {summary['lsm_ssd_bytes']:,}")
    print(f"[ebpf-parse] vlog_ssd_bytes = {summary['vlog_ssd_bytes']:,}")
    print(f"[ebpf-parse] traced_total   = {summary['traced_total_bytes']:,}")


if __name__ == "__main__":
    main(sys.argv)
