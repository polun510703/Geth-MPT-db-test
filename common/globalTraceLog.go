package common

import (
	"bytes"
	"expvar"
	"fmt"
	syslog "log"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Tino: global logger for trace collection
var gethLogger *syslog.Logger
var logFile *os.File
var targetStartBlockNumber uint64 = 4000000 // The start block number for trace collection
var targetEndBlockNumber uint64 = 4000002   // The end block number for trace collection
var shouldGlobalLogInUse bool = false       // Flag to enable or disable global logging, it will be set to true when the target start block number is reached

var logIsInitiated bool = false

// CASTLE: Operation type constants for trie node stats
const (
	TrieOpGet     = 0
	TrieOpInsert  = 1
	TrieOpDelete  = 2
	TrieOpGetNode = 3
	TrieOpCount   = 4
)

// CASTLE: Operation names for CSV output
var TrieOpNames = [TrieOpCount]string{"get", "insert", "delete", "getNode"}

// CASTLE: Per-operation atomic counters [TrieOpCount]
// CASTLE: Resolved: hashNode → DB read → decoded node type + blob size
var TrieResolvedShortCount [TrieOpCount]int64
var TrieResolvedShortBytes [TrieOpCount]int64
var TrieResolvedFullCount [TrieOpCount]int64
var TrieResolvedFullBytes [TrieOpCount]int64

// CASTLE: Traversed: node types encountered during get/insert/delete/getNode recursion
var TrieTraversedShort [TrieOpCount]int64
var TrieTraversedFull [TrieOpCount]int64
var TrieTraversedHash [TrieOpCount]int64
var TrieTraversedValue [TrieOpCount]int64
var TrieTraversedNil [TrieOpCount]int64

// CASTLE: Traversed bytes: actual data accessed per node type during traversal
var TrieTraversedShortBytes [TrieOpCount]int64 // len(n.Key): key bytes compared during prefix matching
var TrieTraversedFullBytes [TrieOpCount]int64  // unsafe.Sizeof(n.Children[key[pos]]): one interface slot (16 bytes)
var TrieTraversedValueBytes [TrieOpCount]int64 // len(n): value bytes returned/examined
var TrieTraversedHashBytes [TrieOpCount]int64  // len(blob): RLP blob size from resolveAndTrack (disk I/O)

// CASTLE: Independent CSV file for execute stats timing
var executeStatsFile *os.File

// CASTLE: Independent CSV file for trie node stats
var trieStatsFile *os.File
var trieStatsTotalResolvedShort [TrieOpCount]int64
var trieStatsTotalResolvedShortBytes [TrieOpCount]int64
var trieStatsTotalResolvedFull [TrieOpCount]int64
var trieStatsTotalResolvedFullBytes [TrieOpCount]int64
var trieStatsTotalTraversedShort [TrieOpCount]int64
var trieStatsTotalTraversedFull [TrieOpCount]int64
var trieStatsTotalTraversedHash [TrieOpCount]int64
var trieStatsTotalTraversedValue [TrieOpCount]int64
var trieStatsTotalTraversedNil [TrieOpCount]int64
var trieStatsTotalTraversedShortBytes [TrieOpCount]int64
var trieStatsTotalTraversedFullBytes [TrieOpCount]int64
var trieStatsTotalTraversedValueBytes [TrieOpCount]int64
var trieStatsTotalTraversedHashBytes [TrieOpCount]int64

func GetTargetStartBlockNumber() uint64 {
	return targetStartBlockNumber
}

func GetTargetEndBlockNumber() uint64 {
	return targetEndBlockNumber
}

func SetEnableGlobalLog(enable bool) {
	shouldGlobalLogInUse = enable
	if !enable {
		fmt.Println("Global log is disabled.")
	} else {
		fmt.Println("Global log is enabled.")
	}
}

// CASTLE: Check if global logging is enabled
func IsGlobalLogEnabled() bool { return shouldGlobalLogInUse }

func GoroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// "goroutine 123 [..."
	field := bytes.TrimPrefix(buf[:n], []byte("goroutine "))
	field = field[:bytes.IndexByte(field, ' ')]
	id, _ := strconv.ParseInt(string(field), 10, 64)
	return id
}

func WriteGlobalLog(msg string) {
	if shouldGlobalLogInUse {
		if logIsInitiated && gethLogger != nil {
			gethLogger.Println(msg)
		}
	}
}

func InitGlobalLog() bool {
	currentLogTime := time.Now().Format("2006-01-02-15-04-05")
	currentLogFileName := fmt.Sprintf("./blktrace_%d_%d_%s", targetStartBlockNumber, targetEndBlockNumber, currentLogTime)

	file, err := os.OpenFile(currentLogFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if err != nil {
		fmt.Println("Error opening global log file:", err)
		logIsInitiated = false
		return false
	}
	logFile = file
	gethLogger = syslog.New(file, "geth: ", syslog.Lshortfile|syslog.Ldate|syslog.Ltime)
	fmt.Println("Global log file opened successfully")
	logIsInitiated = true
	WriteGlobalLog("Global log file opened successfully")

	// CASTLE: Open independent CSV file for execute stats timing
	execStatsFileName := fmt.Sprintf("./execute_stats_%d_%d_%s.csv", targetStartBlockNumber, targetEndBlockNumber, currentLogTime)
	execStatsF, execStatsErr := os.OpenFile(execStatsFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if execStatsErr != nil {
		fmt.Println("Error opening execute stats CSV file:", execStatsErr)
	} else {
		executeStatsFile = execStatsF
		fmt.Fprintln(executeStatsFile, "block_id,execution_us,account_reads_us,storage_reads_us,code_reads_us,ptime_us,validation_us,account_hashes_us,account_updates_us,storage_updates_us,vtime_us,account_commits_us,storage_commits_us,snapshot_commit_us,triedb_commit_us,block_write_us,wtime_us,total_time_us")
		fmt.Println("Execute stats CSV file opened:", execStatsFileName)
	}

	// CASTLE: Open independent CSV file for trie node stats
	csvFileName := fmt.Sprintf("./trie_node_stats_%d_%d_%s.csv", targetStartBlockNumber, targetEndBlockNumber, currentLogTime)
	csvFile, csvErr := os.OpenFile(csvFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if csvErr != nil {
		fmt.Println("Error opening trie stats CSV file:", csvErr)
	} else {
		trieStatsFile = csvFile
		// CASTLE: Write CSV header
		fmt.Fprintln(trieStatsFile, "block_id,op,traversed_short,traversed_full,traversed_value,traversed_hash,traversed_short_bytes,traversed_full_bytes,traversed_value_bytes,traversed_hash_bytes,resolved_short,resolved_full,resolved_short_bytes,resolved_full_bytes")
		fmt.Println("Trie node stats CSV file opened:", csvFileName)
	}

	return true
}

// CASTLE: FlushTrieNodeStats swaps all atomic counters, writes one CSV row per op, and accumulates totals.
func FlushTrieNodeStats(blockID string) {
	if !shouldGlobalLogInUse || trieStatsFile == nil {
		return
	}
	for op := 0; op < TrieOpCount; op++ {
		ts := atomic.SwapInt64(&TrieTraversedShort[op], 0)
		tf := atomic.SwapInt64(&TrieTraversedFull[op], 0)
		tv := atomic.SwapInt64(&TrieTraversedValue[op], 0)
		th := atomic.SwapInt64(&TrieTraversedHash[op], 0)
		tsb := atomic.SwapInt64(&TrieTraversedShortBytes[op], 0)
		tfb := atomic.SwapInt64(&TrieTraversedFullBytes[op], 0)
		tvb := atomic.SwapInt64(&TrieTraversedValueBytes[op], 0)
		thb := atomic.SwapInt64(&TrieTraversedHashBytes[op], 0)
		rsc := atomic.SwapInt64(&TrieResolvedShortCount[op], 0)
		rfc := atomic.SwapInt64(&TrieResolvedFullCount[op], 0)
		rsb := atomic.SwapInt64(&TrieResolvedShortBytes[op], 0)
		rfb := atomic.SwapInt64(&TrieResolvedFullBytes[op], 0)

		fmt.Fprintf(trieStatsFile, "%s,%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
			blockID, TrieOpNames[op],
			ts, tf, tv, th,
			tsb, tfb, tvb, thb,
			rsc, rfc, rsb, rfb)

		// CASTLE: Accumulate totals
		trieStatsTotalTraversedShort[op] += ts
		trieStatsTotalTraversedFull[op] += tf
		trieStatsTotalTraversedValue[op] += tv
		trieStatsTotalTraversedHash[op] += th
		trieStatsTotalTraversedShortBytes[op] += tsb
		trieStatsTotalTraversedFullBytes[op] += tfb
		trieStatsTotalTraversedValueBytes[op] += tvb
		trieStatsTotalTraversedHashBytes[op] += thb
		trieStatsTotalResolvedShort[op] += rsc
		trieStatsTotalResolvedFull[op] += rfc
		trieStatsTotalResolvedShortBytes[op] += rsb
		trieStatsTotalResolvedFullBytes[op] += rfb
	}
}

// CASTLE: FlushExecuteStats writes one CSV row with per-block timing breakdown (in microseconds).
// The columns follow the insertChain flow: processing → validation → write → total.
func FlushExecuteStats(blockID string,
	execution, accountReads, storageReads, codeReads, ptime time.Duration,
	validation, accountHashes, accountUpdates, storageUpdates, vtime time.Duration,
	accountCommits, storageCommits, snapshotCommit, trieDBCommit, blockWrite, wtime time.Duration,
	totalTime time.Duration,
) {
	if !shouldGlobalLogInUse || executeStatsFile == nil {
		return
	}
	fmt.Fprintf(executeStatsFile, "%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
		blockID,
		execution.Microseconds(), accountReads.Microseconds(), storageReads.Microseconds(), codeReads.Microseconds(), ptime.Microseconds(),
		validation.Microseconds(), accountHashes.Microseconds(), accountUpdates.Microseconds(), storageUpdates.Microseconds(), vtime.Microseconds(),
		accountCommits.Microseconds(), storageCommits.Microseconds(), snapshotCommit.Microseconds(), trieDBCommit.Microseconds(), blockWrite.Microseconds(), wtime.Microseconds(),
		totalTime.Microseconds(),
	)
}

// CASTLE: badgerMetricCategory classifies a BadgerDB expvar key into a
// category. Rules are checked in order — first match wins — so that
// sub-categories (e.g. write_bytes_compaction) are caught before their
// generic parent (write).
func badgerMetricCategory(key string) string {
	switch {
	case key == "badger_write_bytes_compaction" || strings.HasPrefix(key, "badger_compaction_"):
		return "compaction"
	case key == "badger_hit_num_lsm_bloom_filter":
		return "bloom_filter"
	case key == "badger_write_pending_num_memtable":
		return "memtable"
	case strings.HasPrefix(key, "badger_iterator_"):
		return "iterator"
	case strings.HasPrefix(key, "badger_size_bytes_"):
		return "size"
	case strings.HasPrefix(key, "badger_get_"), strings.HasPrefix(key, "badger_read_"):
		return "read"
	case strings.HasPrefix(key, "badger_put_"), strings.HasPrefix(key, "badger_write_"):
		return "write"
	default:
		return "other"
	}
}

// CASTLE: SnapshotBadgerMetrics returns the current value of every badger_*
// expvar as an int64 map. Map-typed counters (e.g. badger_get_num_lsm whose
// value is an expvar.Map of per-level counters) are flattened to
// "<name>.<sub>" entries. Sub-values that are not *expvar.Int are skipped
// (badger uses *expvar.Int throughout, so this loses nothing in practice).
// Snapshot the map before and after a measurement window and subtract to get
// the delta attributable to that window.
func SnapshotBadgerMetrics() map[string]int64 {
	out := make(map[string]int64)
	expvar.Do(func(kv expvar.KeyValue) {
		if !strings.HasPrefix(kv.Key, "badger_") {
			return
		}
		switch v := kv.Value.(type) {
		case *expvar.Int:
			out[kv.Key] = v.Value()
		case *expvar.Map:
			v.Do(func(sub expvar.KeyValue) {
				if iv, ok := sub.Value.(*expvar.Int); ok {
					out[kv.Key+"."+sub.Key] = iv.Value()
				}
			})
		}
	})
	return out
}

// CASTLE: BadgerDB Get-phase read timing accumulators. A badger Get is two
// distinct phases: txn.Get (LSM lookup — memtable + SST levels, bloom, block
// reads) and item.ValueCopy (vlog read for kv-separated values, or an inline
// memcpy when the value lives in the LSM). The badger wrapper times each phase
// and accumulates here so a benchmark can snapshot before/after a window and
// subtract to attribute wall-clock read time to LSM vs vlog.
var (
	badgerLSMReadNanos  int64
	badgerVlogReadNanos int64
	badgerLSMReadCount  int64
	badgerVlogReadCount int64
)

// AddBadgerLSMReadNanos records one LSM-lookup phase (txn.Get) duration.
func AddBadgerLSMReadNanos(ns int64) {
	atomic.AddInt64(&badgerLSMReadNanos, ns)
	atomic.AddInt64(&badgerLSMReadCount, 1)
}

// AddBadgerVlogReadNanos records one value-materialize phase (item.ValueCopy)
// duration. For kv-separated values this is the vlog read; otherwise it is an
// inline memcpy and will be near-zero.
func AddBadgerVlogReadNanos(ns int64) {
	atomic.AddInt64(&badgerVlogReadNanos, ns)
	atomic.AddInt64(&badgerVlogReadCount, 1)
}

// SnapshotBadgerReadTiming returns the cumulative Get-phase timing counters.
// Snapshot before and after a measurement window and subtract to get the delta.
func SnapshotBadgerReadTiming() map[string]int64 {
	return map[string]int64{
		"lsm_read_time_ns":  atomic.LoadInt64(&badgerLSMReadNanos),
		"vlog_read_time_ns": atomic.LoadInt64(&badgerVlogReadNanos),
		"lsm_read_count":    atomic.LoadInt64(&badgerLSMReadCount),
		"vlog_read_count":   atomic.LoadInt64(&badgerVlogReadCount),
	}
}

// CASTLE: DumpBadgerMetricsTo writes all BadgerDB expvar metrics to the given
// path as CSV, grouped by category with section-header rows
// (# === CATEGORY ===) between groups. Metrics within each group are sorted
// alphabetically. Returns the resolved path written.
func DumpBadgerMetricsTo(path string) (string, error) {
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fmt.Fprintln(f, "metric,value")

	grouped := make(map[string][]string)
	values := make(map[string]string)
	expvar.Do(func(kv expvar.KeyValue) {
		if !strings.HasPrefix(kv.Key, "badger_") {
			return
		}
		cat := badgerMetricCategory(kv.Key)
		grouped[cat] = append(grouped[cat], kv.Key)
		values[kv.Key] = kv.Value.String()
	})

	emitOrder := []string{"read", "write", "compaction", "size", "iterator", "bloom_filter", "memtable", "other"}
	for _, cat := range emitOrder {
		keys := grouped[cat]
		if len(keys) == 0 {
			continue
		}
		sort.Strings(keys)
		fmt.Fprintf(f, "# === %s ===\n", strings.ToUpper(cat))
		for _, k := range keys {
			fmt.Fprintf(f, "%s,%s\n", k, values[k])
		}
	}
	return path, nil
}

// dumpBadgerMetrics is the legacy auto-path entry used by CloseGlobalLog.
func dumpBadgerMetrics() {
	fileName := fmt.Sprintf("./badger_metrics_%d_%d_%s.csv",
		targetStartBlockNumber, targetEndBlockNumber,
		time.Now().Format("2006-01-02-15-04-05"))
	written, err := DumpBadgerMetricsTo(fileName)
	if err != nil {
		fmt.Println("Error creating badger metrics file:", err)
		return
	}
	fmt.Println("BadgerDB metrics dumped to:", written)
}

func CloseGlobalLog() {
	// CASTLE: Dump BadgerDB expvar metrics before closing
	dumpBadgerMetrics()

	// CASTLE: Write TOTAL rows (one per op) and close trie stats CSV
	if trieStatsFile != nil {
		for op := 0; op < TrieOpCount; op++ {
			fmt.Fprintf(trieStatsFile, "TOTAL,%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
				TrieOpNames[op],
				trieStatsTotalTraversedShort[op], trieStatsTotalTraversedFull[op],
				trieStatsTotalTraversedValue[op], trieStatsTotalTraversedHash[op],
				trieStatsTotalTraversedShortBytes[op], trieStatsTotalTraversedFullBytes[op],
				trieStatsTotalTraversedValueBytes[op], trieStatsTotalTraversedHashBytes[op],
				trieStatsTotalResolvedShort[op], trieStatsTotalResolvedFull[op],
				trieStatsTotalResolvedShortBytes[op], trieStatsTotalResolvedFullBytes[op])
		}
		// CASTLE: Write a grand TOTAL row summing all operations
		var grandTS, grandTF, grandTV, grandTH int64
		var grandTSB, grandTFB, grandTVB, grandTHB int64
		var grandRSC, grandRFC, grandRSB, grandRFB int64
		for op := 0; op < TrieOpCount; op++ {
			grandTS += trieStatsTotalTraversedShort[op]
			grandTF += trieStatsTotalTraversedFull[op]
			grandTV += trieStatsTotalTraversedValue[op]
			grandTH += trieStatsTotalTraversedHash[op]
			grandTSB += trieStatsTotalTraversedShortBytes[op]
			grandTFB += trieStatsTotalTraversedFullBytes[op]
			grandTVB += trieStatsTotalTraversedValueBytes[op]
			grandTHB += trieStatsTotalTraversedHashBytes[op]
			grandRSC += trieStatsTotalResolvedShort[op]
			grandRFC += trieStatsTotalResolvedFull[op]
			grandRSB += trieStatsTotalResolvedShortBytes[op]
			grandRFB += trieStatsTotalResolvedFullBytes[op]
		}
		fmt.Fprintf(trieStatsFile, "TOTAL,all,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
			grandTS, grandTF, grandTV, grandTH,
			grandTSB, grandTFB, grandTVB, grandTHB,
			grandRSC, grandRFC, grandRSB, grandRFB)

		trieStatsFile.Close()
		fmt.Println("Trie node stats CSV file closed")
	}

	// CASTLE: Close execute stats CSV
	if executeStatsFile != nil {
		executeStatsFile.Close()
		fmt.Println("Execute stats CSV file closed")
	}

	if logFile != nil {
		logFile.Close()
		fmt.Println("Global log file closed")
	}
}

func StopChainManually() {
	pid := os.Getpid()
	fmt.Printf("Current process PID: %d\n", pid)
	err := syscall.Kill(pid, syscall.SIGINT)
	if err != nil {
		fmt.Println("Failed to send SIGINT:", err)
		return
	}
	time.Sleep(2 * time.Second)
	fmt.Println("SIGINT sent. Process should be interrupted if it handles SIGINT.")
}
