package observation_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/analysis/CASTLE/observation/workload"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// ---------- env helpers ----------

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return def
}

// modeConfig resolves mode → (datadir, valueThreshold).
func modeConfig(t testing.TB, mode string) (string, int) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	switch mode {
	case "kvsep":
		return envDefault("OBS_DATADIR", filepath.Join(home, "ethereum/execution/data-kvsep")), 1
	case "nosep":
		return envDefault("OBS_DATADIR", filepath.Join(home, "ethereum/execution/data-nosep")), 1048576
	default:
		t.Fatalf("OBS_MODE must be kvsep|nosep (got %q)", mode)
		return "", 0
	}
}

// dataDir resolves where extracted bin files live (analysis/CASTLE/observation/data).
func dataDir(t testing.TB) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "data")
}

// ---------- TestExtractWorkload ----------

// TestExtractWorkload is the one-time extractor. By default it Skips if the
// expected accounts_<mode>.bin already exists; set EXTRACT_FORCE=1 to redo.
//
//	OBS_MODE=kvsep go test -run TestExtractWorkload -v -timeout 0 ./...
func TestExtractWorkload(t *testing.T) {
	if os.Getenv("OBS_EXTRACT") != "1" && os.Getenv("EXTRACT_FORCE") != "1" {
		t.Skip("set OBS_EXTRACT=1 to run extractor (or EXTRACT_FORCE=1 to redo)")
	}
	mode := envDefault("OBS_MODE", "kvsep")
	datadir, vt := modeConfig(t, mode)
	out := dataDir(t)

	accountsPath := filepath.Join(out, fmt.Sprintf("accounts_%s.bin", mode))
	if os.Getenv("EXTRACT_FORCE") != "1" {
		if _, err := os.Stat(accountsPath); err == nil {
			t.Skipf("%s already exists (set EXTRACT_FORCE=1 to redo)", accountsPath)
		}
	}

	cfg := workload.ExtractConfig{
		Datadir:        datadir,
		ValueThreshold: vt,
		OutDir:         out,
		Tag:            mode,
		SampleEvery:    envInt("OBS_SAMPLE_EVERY", 8),
		AccountsMax:    envInt("OBS_ACCOUNTS_MAX", 0),
		TxMaxBlock:     uint64(envInt("OBS_TX_MAX_BLOCK", 0)),
	}
	t.Logf("[extract] mode=%s datadir=%s valueThreshold=%d sample_every=%d", mode, datadir, vt, cfg.SampleEvery)
	mf, err := workload.Run(cfg)
	if err != nil {
		t.Fatalf("workload.Run: %v", err)
	}
	t.Logf("[extract] done: head=%d accounts=%d txs=%d wall=%ss",
		mf.HeadBlock, mf.AccountCount, mf.TxCount, mf.ExtractWallSec)
}

// ---------- TestObservation ----------

// TestObservation is the read benchmark. Controlled entirely by env vars; see
// the plan file for the full table. Skipped unless OBS_RUN=1 is set so that
// the unit-test layer (go test ./... at repo root) doesn't accidentally fire
// a several-minute disk-bound run.
//
//	OBS_RUN=1 OBS_MODE=nosep OBS_PATTERN=uniform OBS_RATIO=1:1 OBS_NOPS=10000 \
//	  go test -run TestObservation -v -timeout 0 ./...
func TestObservation(t *testing.T) {
	if os.Getenv("OBS_RUN") != "1" {
		t.Skip("set OBS_RUN=1 to run the read benchmark")
	}

	mode := envDefault("OBS_MODE", "kvsep")
	pattern := envDefault("OBS_PATTERN", "zipf")
	ratio := envDefault("OBS_RATIO", "1:1")
	nops := envInt("OBS_NOPS", 2_000_000)
	seed := envInt64("OBS_SEED", 42)
	zipfS := envFloat("OBS_ZIPF_S", 1.1)
	recentFrac := envFloat("OBS_RECENT_FRAC", 0.1)

	datadir, vt := modeConfig(t, mode)
	outDir := envDefault("OBS_OUTPUT_DIR", "")
	if outDir == "" {
		ts := time.Now().Format("20060102_150405")
		outDir = filepath.Join(dataDir(t), "..",
			fmt.Sprintf("raOutput_obs_%s_%s_%s_%s", mode, pattern, ratioFileTag(ratio), ts))
	}
	if err := os.MkdirAll(filepath.Join(outDir, "badger_metrics"), 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(outDir, "latency"), 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}

	// --- ratio ---
	txR, acctR, err := workload.ParseRatio(ratio)
	if err != nil {
		t.Fatalf("ratio: %v", err)
	}

	// --- load datasets ---
	accountsPath := filepath.Join(dataDir(t), fmt.Sprintf("accounts_%s.bin", mode))
	txsPath := filepath.Join(dataDir(t), fmt.Sprintf("txs_%s.bin", mode))
	accounts, err := workload.LoadAccounts(accountsPath)
	if err != nil {
		t.Fatalf("load accounts %s: %v (run TestExtractWorkload first)", accountsPath, err)
	}
	txs, err := workload.LoadTxs(txsPath)
	if err != nil {
		t.Fatalf("load txs %s: %v", txsPath, err)
	}
	t.Logf("[obs] accounts=%d txs=%d mode=%s pattern=%s ratio=%s nops=%d",
		len(accounts), len(txs), mode, pattern, ratio, nops)

	// --- patterns: separate RNG state for tx and acct so they don't lockstep ---
	pp := workload.PatternParams{Seed: seed, ZipfS: zipfS, RecentFrac: recentFrac}
	pTx := pp
	pTx.N = len(txs)
	pTx.Seed = seed
	pAcct := pp
	pAcct.N = len(accounts)
	pAcct.Seed = seed ^ 0x1E3779B97F4A7C15 // distinct sub-seed (fits in int64)
	txSeq, err := workload.NewPattern(pattern, pTx)
	if err != nil {
		t.Fatalf("tx pattern: %v", err)
	}
	acctSeq, err := workload.NewPattern(pattern, pAcct)
	if err != nil {
		t.Fatalf("acct pattern: %v", err)
	}

	// --- open chaindata ---
	db, diskdb, err := workload.OpenChaindataReadOnly(datadir, vt)
	if err != nil {
		t.Fatalf("open chaindata: %v", err)
	}
	defer db.Close()

	headBlock := rawdb.ReadHeadBlock(db)
	if headBlock == nil {
		t.Fatalf("ReadHeadBlock nil")
	}
	stateRoot := headBlock.Root()
	trieDB := workload.MakeTrieDB(db, datadir)
	st, err := trie.NewStateTrie(trie.StateTrieID(stateRoot), trieDB)
	if err != nil {
		t.Fatalf("trie.NewStateTrie(root=%x): %v", stateRoot, err)
	}

	// --- per-op latency buffer ---
	lat := make([]int64, nops)
	opTypes := make([]byte, nops) // 't' or 'a'
	bytesRet := make([]int, nops) // RLP-encoded size of the result returned to the test

	// --- read-amp pre-snapshot ---
	// Counters are cumulative since process start; we delta against post-snapshot
	// so the reported numbers reflect ONLY the main read loop, not DB open /
	// freezer init / accounts.bin load.
	preBadger := common.SnapshotBadgerMetrics()
	preTiming := common.SnapshotBadgerReadTiming()
	preProcIO, preProcIOErr := readProcSelfIO()
	if preProcIOErr != nil {
		t.Logf("[obs] /proc/self/io pre-snapshot failed: %v (read_amp.proc_* will be zero)", preProcIOErr)
	}

	// --- optional periodic sampler (badger expvars + /proc/self/io) ---
	// OBS_SAMPLE_MS=0 disables. Default 1000ms.
	sampleMs := envInt("OBS_SAMPLE_MS", 1000)
	var samplerWG sync.WaitGroup
	samplerStop := make(chan struct{})
	if sampleMs > 0 {
		tsPath := filepath.Join(outDir, "latency", "badger_timeseries.csv")
		tsFile, err := os.Create(tsPath)
		if err != nil {
			t.Logf("create timeseries: %v (skipping sampler)", err)
		} else {
			samplerWG.Add(1)
			go func() {
				defer samplerWG.Done()
				defer tsFile.Close()
				fmt.Fprintln(tsFile,
					"ts_ns,badger_read_bytes_lsm,badger_read_bytes_vlog,"+
						"badger_get_num_user,badger_read_num_vlog,"+
						"proc_rchar,proc_read_bytes,"+
						"badger_lsm_read_time_ns,badger_vlog_read_time_ns")
				writeRow := func() {
					m := common.SnapshotBadgerMetrics()
					tm := common.SnapshotBadgerReadTiming()
					io, _ := readProcSelfIO()
					fmt.Fprintf(tsFile, "%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
						time.Now().UnixNano(),
						m["badger_read_bytes_lsm"], m["badger_read_bytes_vlog"],
						m["badger_get_num_user"], m["badger_read_num_vlog"],
						io.Rchar, io.ReadBytes,
						tm["lsm_read_time_ns"], tm["vlog_read_time_ns"])
				}
				writeRow() // initial sample at loop start
				tick := time.NewTicker(time.Duration(sampleMs) * time.Millisecond)
				defer tick.Stop()
				for {
					select {
					case <-samplerStop:
						writeRow() // final sample at loop end
						return
					case <-tick.C:
						writeRow()
					}
				}
			}()
		}
	}

	// --- main loop ---
	startWall := time.Now()
	fmt.Printf("[OBS] start=%d\n", startWall.UnixNano())

	opsDone := 0
	skipTx := txR == 0
	skipAcct := acctR == 0
	for opsDone < nops {
		if !skipTx {
			for k := 0; k < txR && opsDone < nops; k++ {
				h := txs[txSeq.Next()].Hash
				t0 := time.Now()
				tx, _, _, _ := rawdb.ReadCanonicalTransaction(db, h)
				lat[opsDone] = time.Since(t0).Nanoseconds()
				opTypes[opsDone] = 't'
				if tx != nil {
					// tx.Size() returns the RLP-encoded byte length of the
					// transaction (cached after first call). Measured AFTER
					// the latency stop so encoding doesn't pollute the timing.
					bytesRet[opsDone] = int(tx.Size())
				}
				opsDone++
			}
		}
		if !skipAcct {
			for k := 0; k < acctR && opsDone < nops; k++ {
				hash := accounts[acctSeq.Next()]
				t0 := time.Now()
				acct, _ := st.GetAccountByHash(hash) // trie read keyed by keccak256(address)
				lat[opsDone] = time.Since(t0).Nanoseconds()
				opTypes[opsDone] = 'a'
				if acct != nil {
					// Encode the StateAccount AFTER the latency stop so the
					// extra allocation doesn't pollute timing. Comparable in
					// units to tx.Size() (both are RLP-encoded byte lengths).
					if enc, err := rlp.EncodeToBytes(acct); err == nil {
						bytesRet[opsDone] = len(enc)
					}
				}
				opsDone++
			}
		}
		if skipTx && skipAcct {
			t.Fatalf("ratio %s has zero on both sides", ratio)
		}
	}

	wall := time.Since(startWall)
	endWall := time.Now()
	fmt.Printf("[OBS] end=%d\n", endWall.UnixNano())
	t.Logf("[obs] wall=%s qps=%.0f", wall, float64(opsDone)/wall.Seconds())

	// --- read-amp post-snapshot ---
	postBadger := common.SnapshotBadgerMetrics()
	postTiming := common.SnapshotBadgerReadTiming()
	postProcIO, _ := readProcSelfIO()

	// Stop the periodic sampler (if started) so badger_timeseries.csv flushes
	// cleanly before we touch the rest of outDir.
	close(samplerStop)
	samplerWG.Wait()

	// --- per-op CSV ---
	latPath := filepath.Join(outDir, "latency", "latency_per_op.csv")
	if err := writeLatencyCSV(latPath, lat, opTypes, bytesRet); err != nil {
		t.Fatalf("write latency csv: %v", err)
	}

	// --- summary ---
	summary := buildSummary(lat, opTypes, wall, len(accounts), len(txs), diskdb)
	summary["read_amp"] = buildReadAmp(preBadger, postBadger, preTiming, postTiming, preProcIO, postProcIO, bytesRet)
	summary["mode"] = mode
	summary["pattern"] = pattern
	summary["ratio"] = ratio
	summary["nops"] = opsDone
	summary["seed"] = seed
	summary["zipf_s"] = zipfS
	summary["recent_frac"] = recentFrac
	summary["start_unix_ns"] = startWall.UnixNano()
	summary["end_unix_ns"] = endWall.UnixNano()
	summary["state_root"] = stateRoot.Hex()
	summary["head_block"] = headBlock.NumberU64()

	sumPath := filepath.Join(outDir, "latency", "summary.json")
	if err := writeJSON(sumPath, summary); err != nil {
		t.Fatalf("write summary: %v", err)
	}

	// --- BadgerDB expvar metrics ---
	ts := time.Now().Format("2006-01-02-15-04-05")
	bmPath := filepath.Join(outDir, "badger_metrics", fmt.Sprintf("badger_metrics_obs_%s.csv", ts))
	if written, err := common.DumpBadgerMetricsTo(bmPath); err != nil {
		t.Logf("dump badger metrics: %v", err)
	} else {
		t.Logf("[obs] badger metrics → %s", written)
	}

	t.Logf("[obs] output → %s", outDir)
}

// ---------- helpers ----------

func ratioFileTag(r string) string {
	// "3:7" → "3_7" (filesystem-friendly)
	out := make([]byte, 0, len(r))
	for i := 0; i < len(r); i++ {
		if r[i] == ':' {
			out = append(out, '_')
		} else {
			out = append(out, r[i])
		}
	}
	return string(out)
}

func writeLatencyCSV(path string, lat []int64, types []byte, bytesRet []int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, "op_idx,type,latency_ns,bytes_returned"); err != nil {
		return err
	}
	for i, v := range lat {
		if _, err := fmt.Fprintf(f, "%d,%c,%d,%d\n", i, types[i], v, bytesRet[i]); err != nil {
			return err
		}
	}
	return nil
}

// procIOStats mirrors the fields of /proc/<pid>/io. rchar / wchar are
// syscall-level (include page-cache hits); read_bytes / write_bytes are
// device-attributable (page-cache hits are excluded).
type procIOStats struct {
	Rchar, Wchar, Syscr, Syscw, ReadBytes, WriteBytes int64
}

// readProcSelfIO parses /proc/self/io. Linux-only; on other platforms returns
// a zero struct and an error, which callers downgrade to a Logf.
func readProcSelfIO() (procIOStats, error) {
	var s procIOStats
	f, err := os.Open("/proc/self/io")
	if err != nil {
		return s, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		switch parts[0] {
		case "rchar":
			s.Rchar = n
		case "wchar":
			s.Wchar = n
		case "syscr":
			s.Syscr = n
		case "syscw":
			s.Syscw = n
		case "read_bytes":
			s.ReadBytes = n
		case "write_bytes":
			s.WriteBytes = n
		}
	}
	return s, sc.Err()
}

// buildReadAmp computes window-local deltas from pre/post snapshots and
// returns the read_amp block that is merged into summary.json. The ratios
// (ra_engine / ra_device_over_engine / ra_device_over_logical) are only
// included when their denominator is positive — keeps the JSON parseable
// when something didn't fire (e.g. all-misses run).
func buildReadAmp(preBadger, postBadger, preTiming, postTiming map[string]int64, preIO, postIO procIOStats, bytesRet []int) map[string]any {
	d := func(k string) int64 { return postBadger[k] - preBadger[k] }
	dt := func(k string) int64 { return postTiming[k] - preTiming[k] }

	var logical int64
	for _, b := range bytesRet {
		logical += int64(b)
	}
	lsmBytes := d("badger_read_bytes_lsm")
	vlogBytes := d("badger_read_bytes_vlog")
	engineBytes := lsmBytes + vlogBytes
	procRchar := postIO.Rchar - preIO.Rchar
	procReadBytes := postIO.ReadBytes - preIO.ReadBytes

	// Get-phase read timing (engine perspective): wall-clock attributed to the
	// LSM lookup vs the vlog read across all badger Gets in this window.
	lsmTimeNs := dt("lsm_read_time_ns")
	vlogTimeNs := dt("vlog_read_time_ns")
	lsmCount := dt("lsm_read_count")
	vlogCount := dt("vlog_read_count")

	out := map[string]any{
		"logical_bytes_returned": logical,
		"badger_lsm_bytes_read":  lsmBytes,
		"badger_vlog_bytes_read": vlogBytes,
		"badger_user_gets":       d("badger_get_num_user"),
		"badger_user_gets_with_result": d("badger_get_with_result_num_user"),
		"badger_read_num_vlog":   d("badger_read_num_vlog"),
		"badger_lsm_gets_by_level": map[string]int64{
			"l0": d("badger_get_num_lsm.l0"),
			"l5": d("badger_get_num_lsm.l5"),
			"l6": d("badger_get_num_lsm.l6"),
		},
		"badger_bloom_filter_hits": map[string]int64{
			"DoesNotHave_ALL": d("badger_hit_num_lsm_bloom_filter.DoesNotHave_ALL"),
			"DoesNotHave_HIT": d("badger_hit_num_lsm_bloom_filter.DoesNotHave_HIT"),
			"l0":              d("badger_hit_num_lsm_bloom_filter.l0"),
			"l5":              d("badger_hit_num_lsm_bloom_filter.l5"),
			"l6":              d("badger_hit_num_lsm_bloom_filter.l6"),
		},
		"proc_rchar_bytes": procRchar,
		"proc_read_bytes":  procReadBytes,
		"lsm_read_time_ns":  lsmTimeNs,
		"vlog_read_time_ns": vlogTimeNs,
		"lsm_read_count":    lsmCount,
		"vlog_read_count":   vlogCount,
	}
	if logical > 0 {
		out["ra_engine"] = float64(engineBytes) / float64(logical)
		out["ra_device_over_logical"] = float64(procReadBytes) / float64(logical)
	}
	if engineBytes > 0 {
		out["ra_device_over_engine"] = float64(procReadBytes) / float64(engineBytes)
	}
	// Time fraction spent in vlog vs total engine Get time — the direct
	// "is the bottleneck in vlog?" signal.
	if engineTimeNs := lsmTimeNs + vlogTimeNs; engineTimeNs > 0 {
		out["vlog_time_frac"] = float64(vlogTimeNs) / float64(engineTimeNs)
	}
	if lsmCount > 0 {
		out["lsm_avg_read_ns"] = float64(lsmTimeNs) / float64(lsmCount)
	}
	if vlogCount > 0 {
		out["vlog_avg_read_ns"] = float64(vlogTimeNs) / float64(vlogCount)
	}
	return out
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// statsFor computes count, mean, p50/p90/p95/p99/p999/max over a copy of xs.
// Mutates the copy (sorts in place).
func statsFor(xs []int64) map[string]any {
	if len(xs) == 0 {
		return map[string]any{"count": 0}
	}
	cp := make([]int64, len(xs))
	copy(cp, xs)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	pick := func(q float64) int64 {
		idx := int(math.Ceil(q*float64(len(cp)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(cp) {
			idx = len(cp) - 1
		}
		return cp[idx]
	}
	var sum int64
	for _, v := range cp {
		sum += v
	}
	return map[string]any{
		"count":  len(cp),
		"mean":   float64(sum) / float64(len(cp)),
		"p50":    pick(0.50),
		"p90":    pick(0.90),
		"p95":    pick(0.95),
		"p99":    pick(0.99),
		"p999":   pick(0.999),
		"max":    cp[len(cp)-1],
		"min":    cp[0],
	}
}

func buildSummary(lat []int64, types []byte, wall time.Duration, accountCount, txCount int, diskdb interface {
	Stat() (string, error)
}) map[string]any {
	var txLat, acctLat []int64
	for i, v := range lat {
		switch types[i] {
		case 't':
			txLat = append(txLat, v)
		case 'a':
			acctLat = append(acctLat, v)
		}
	}
	out := map[string]any{
		"total_wall_ns": wall.Nanoseconds(),
		"qps":           float64(len(lat)) / wall.Seconds(),
		"overall":       statsFor(lat),
		"tx":            statsFor(txLat),
		"acct":          statsFor(acctLat),
		"account_count": accountCount,
		"tx_count":      txCount,
	}
	if diskdb != nil {
		if s, err := diskdb.Stat(); err == nil {
			out["badger_stat"] = s
		}
	}
	return out
}
