// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package badger implements the key-value database layer based on BadgerDB.
package badger

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v4"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

const (
	// minCache is the minimum amount of memory in megabytes to allocate to badger
	// read and write caching, split half and half.
	minCache = 16

	// minHandles is the minimum number of files handles to allocate to the open
	// database files.
	minHandles = 16

	// metricsGatheringInterval specifies the interval to retrieve badger database
	// stats to report to the user.
	metricsGatheringInterval = 3 * time.Second

	// gcInterval specifies how often the value log GC should run.
	gcInterval = 5 * time.Minute

	// gcDiscardRatio is the threshold for value log GC. A file will be rewritten
	// if this fraction of its space can be reclaimed.
	gcDiscardRatio = 0.5
)

// Database is a persistent key-value store based on the BadgerDB storage engine.
// Apart from basic data storage functionality it also supports batch writes and
// iterating over the keyspace in binary-alphabetical order.
type Database struct {
	fn        string     // filename for reporting
	db        *badger.DB // Underlying BadgerDB storage engine
	namespace string     // Namespace for metrics

	compTimeMeter  *metrics.Meter // Meter for measuring the total time spent in database compaction
	diskSizeGauge  *metrics.Gauge // Gauge for tracking the size of all the levels in the database
	diskReadMeter  *metrics.Meter // Meter for measuring the effective amount of data read
	diskWriteMeter *metrics.Meter // Meter for measuring the effective amount of data written

	quitLock sync.RWMutex    // Mutex protecting the quit channel and the closed flag
	quitChan chan chan error  // Quit channel to stop the metrics collection before closing the database
	closed   bool            // keep track of whether we're Closed

	log log.Logger // Contextual logger tracking the database path

	// BadgerDB-specific: value log GC goroutine control
	gcQuit chan struct{}
}

// New returns a wrapped BadgerDB object. The namespace is the prefix that the
// metrics reporting should use for surfacing internal stats.
func New(file string, cache int, handles int, namespace string, readonly bool, valueThreshold int) (*Database, error) {
	if cache < minCache {
		cache = minCache
	}
	if handles < minHandles {
		handles = minHandles
	}
	logger := log.New("database", file)
	logger.Info("Allocated cache and file handles", "cache", common.StorageSize(cache*1024*1024), "handles", handles)

	opts := badger.DefaultOptions(file)
	opts.ReadOnly = readonly
	opts.Logger = nil // Disable BadgerDB's internal logging

	// Cache configuration: split between block cache and index cache
	opts.BlockCacheSize = int64(cache/2) * 1024 * 1024
	opts.IndexCacheSize = int64(cache/2) * 1024 * 1024

	// KV separation threshold: values larger than this go to vlog.
	if valueThreshold > 0 {
		opts.ValueThreshold = int64(valueThreshold)
	} else {
		opts.ValueThreshold = 1024
	}
	logger.Info("BadgerDB value threshold", "bytes", opts.ValueThreshold)

	// Use all available CPUs for compaction
	opts.NumCompactors = runtime.NumCPU()

	// Open the database
	innerDB, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}

	db := &Database{
		fn:        file,
		db:        innerDB,
		namespace: namespace,
		log:       logger,
		quitChan:  make(chan chan error),
		gcQuit:    make(chan struct{}),
	}

	db.compTimeMeter = metrics.GetOrRegisterMeter(namespace+"compact/time", nil)
	db.diskSizeGauge = metrics.GetOrRegisterGauge(namespace+"disk/size", nil)
	db.diskReadMeter = metrics.GetOrRegisterMeter(namespace+"disk/read", nil)
	db.diskWriteMeter = metrics.GetOrRegisterMeter(namespace+"disk/write", nil)

	// Start background value log GC and metrics collection
	if !readonly {
		go db.runGC()
	}
	go db.meter(metricsGatheringInterval)
	return db, nil
}

// runGC periodically triggers BadgerDB's value log garbage collection.
// Without this, disk usage grows unbounded as old values are not reclaimed.
func (d *Database) runGC() {
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Run GC until there's nothing more to collect
			for {
				if err := d.db.RunValueLogGC(gcDiscardRatio); err != nil {
					break
				}
			}
		case <-d.gcQuit:
			return
		}
	}
}

// Close stops the metrics collection and value log GC, flushes any pending data
// to disk and closes all io accesses to the underlying key-value store.
func (d *Database) Close() error {
	d.quitLock.Lock()
	defer d.quitLock.Unlock()

	if d.closed {
		return nil
	}
	d.closed = true

	// Stop value log GC
	close(d.gcQuit)

	// Stop metrics collection
	if d.quitChan != nil {
		errc := make(chan error)
		d.quitChan <- errc
		if err := <-errc; err != nil {
			d.log.Error("Metrics collection failed", "err", err)
		}
		d.quitChan = nil
	}
	common.WriteGlobalLog("Closing database")
	return d.db.Close()
}

// Has retrieves if a key is present in the key-value store.
func (d *Database) Has(key []byte) (bool, error) {
	d.quitLock.RLock()
	defer d.quitLock.RUnlock()

	s := fmt.Sprintf("OPType: Has, key: %x, size: %d", key, len(key))
	common.WriteGlobalLog(s)

	if d.closed {
		return false, badger.ErrDBClosed
	}
	var found bool
	err := d.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		if err == nil {
			found = true
		}
		return err
	})
	if err == badger.ErrKeyNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return found, nil
}

// Get retrieves the given key if it's present in the key-value store.
func (d *Database) Get(key []byte) ([]byte, error) {
	d.quitLock.RLock()
	defer d.quitLock.RUnlock()

	if d.closed {
		return nil, badger.ErrDBClosed
	}
	var val []byte
	err := d.db.View(func(txn *badger.Txn) error {
		// Split-time the two Get phases: txn.Get is the LSM lookup (memtable +
		// SST levels, bloom, block reads); item.ValueCopy is the vlog read for
		// kv-separated values (or an inline memcpy otherwise). Recording the LSM
		// time before the err check counts misses (ErrKeyNotFound still read LSM).
		t0 := time.Now()
		item, err := txn.Get(key)
		common.AddBadgerLSMReadNanos(time.Since(t0).Nanoseconds())
		if err != nil {
			return err
		}
		// Must use ValueCopy; the value slice is only valid within this callback
		t1 := time.Now()
		val, err = item.ValueCopy(nil)
		common.AddBadgerVlogReadNanos(time.Since(t1).Nanoseconds())
		return err
	})
	if err != nil {
		return nil, err
	}

	s := fmt.Sprintf("OPType: Get, key: %x, size: %d, gid: %d",
		key, len(key), common.GoroutineID())
	common.WriteGlobalLog(s)

	return val, nil
}

// Put inserts the given value into the key-value store.
func (d *Database) Put(key []byte, value []byte) error {
	d.quitLock.RLock()
	defer d.quitLock.RUnlock()

	s := fmt.Sprintf("OPType: Put, key: %x, size: %d, valueSize: %d, gid: %d", key, len(key), len(value), common.GoroutineID())
	common.WriteGlobalLog(s)

	if d.closed {
		return badger.ErrDBClosed
	}
	return d.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, value)
	})
}

// Delete removes the key from the key-value store.
func (d *Database) Delete(key []byte) error {
	d.quitLock.RLock()
	defer d.quitLock.RUnlock()

	s := fmt.Sprintf("OPType: Delete, key: %x, size: %d, gid: %d", key, len(key), common.GoroutineID())
	common.WriteGlobalLog(s)

	if d.closed {
		return badger.ErrDBClosed
	}
	return d.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
}

// DeleteRange deletes all of the keys (and values) in the range [start,end)
// (inclusive on start, exclusive on end).
func (d *Database) DeleteRange(start, end []byte) error {
	d.quitLock.RLock()
	defer d.quitLock.RUnlock()

	if d.closed {
		return badger.ErrDBClosed
	}
	if end == nil {
		end = ethdb.MaximumKey
	}
	// Collect keys first via a read transaction, then delete via WriteBatch.
	// BadgerDB has no native range delete operation.
	wb := d.db.NewWriteBatch()
	defer wb.Cancel()

	err := d.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false // key-only iteration for efficiency
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(start); it.Valid(); it.Next() {
			key := it.Item().KeyCopy(nil)
			if bytes.Compare(key, end) >= 0 {
				break
			}
			if err := wb.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return wb.Flush()
}

// NewBatch creates a write-only key-value store that buffers changes to its host
// database until a final write is called.
func (d *Database) NewBatch() ethdb.Batch {
	s := fmt.Sprintf("OPType: NewBatch, gid: %d", common.GoroutineID())
	common.WriteGlobalLog(s)
	return &batch{
		b:   d.db.NewWriteBatch(),
		db:  d,
		ops: make([]batchOp, 0),
	}
}

// NewBatchWithSize creates a write-only database batch with pre-allocated buffer.
// BadgerDB's WriteBatch does not support pre-allocation, so this is equivalent to NewBatch.
func (d *Database) NewBatchWithSize(size int) ethdb.Batch {
	s := fmt.Sprintf("OPType: NewBatchWithSize, size: %d, gid: %d", size, common.GoroutineID())
	common.WriteGlobalLog(s)
	return &batch{
		b:   d.db.NewWriteBatch(),
		db:  d,
		ops: make([]batchOp, 0, size/64), // rough estimate of ops count
	}
}

// upperBound returns the upper bound for the given prefix
func upperBound(prefix []byte) []byte {
	for i := len(prefix) - 1; i >= 0; i-- {
		c := prefix[i]
		if c == 0xff {
			continue
		}
		limit := make([]byte, i+1)
		copy(limit, prefix)
		limit[i] = c + 1
		return limit
	}
	return nil
}

// NewIterator creates a binary-alphabetical iterator over a subset
// of database content with a particular key prefix, starting at a particular
// initial key (or after, if it does not exist).
func (d *Database) NewIterator(prefix []byte, start []byte) ethdb.Iterator {
	s := fmt.Sprintf("OPType: NewIterator, prefix: %x, start key: %x, gid: %d", prefix, start, common.GoroutineID())
	common.WriteGlobalLog(s)

	txn := d.db.NewTransaction(false) // read-only transaction

	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix

	it := txn.NewIterator(opts)
	seekKey := make([]byte, 0, len(prefix)+len(start))
	seekKey = append(seekKey, prefix...)
	seekKey = append(seekKey, start...)
	it.Seek(seekKey)

	iter := &badgerIterator{
		txn:  txn,
		iter: it,
		moved: true,
	}
	// Pre-cache the first entry if valid
	if it.Valid() {
		iter.cacheCurrentEntry()
	}
	return iter
}

// Stat returns the internal metrics of BadgerDB in a text format.
func (d *Database) Stat() (string, error) {
	lsm, vlog := d.db.Size()
	return fmt.Sprintf("LSM size: %d, Value log size: %d", lsm, vlog), nil
}

// Compact flattens the underlying data store for the given key range.
// BadgerDB does not support range-based compaction, so we use Flatten
// which compacts all levels.
func (d *Database) Compact(start []byte, limit []byte) error {
	s := fmt.Sprintf("OPType: Compact, start key: %x, end key: %x", start, limit)
	common.WriteGlobalLog(s)
	return d.db.Flatten(runtime.NumCPU())
}

// Path returns the path to the database directory.
func (d *Database) Path() string {
	return d.fn
}

// SyncKeyValue flushes all pending writes to disk, ensuring data durability.
func (d *Database) SyncKeyValue() error {
	return d.db.Sync()
}

// meter periodically retrieves internal badger counters and reports them to
// the metrics subsystem.
func (d *Database) meter(refresh time.Duration) {
	var errc chan error
	timer := time.NewTimer(refresh)
	defer timer.Stop()

	for errc == nil {
		lsm, vlog := d.db.Size()
		d.diskSizeGauge.Update(lsm + vlog)

		select {
		case errc = <-d.quitChan:
		case <-timer.C:
			timer.Reset(refresh)
		}
	}
	errc <- nil
}

// batchOp records a single operation for replay support.
type batchOp struct {
	del        bool
	rangeStart []byte // non-nil for range deletes
	rangeEnd   []byte
	key        []byte
	value      []byte
}

// batch is a write-only batch that commits changes to its host database
// when Write is called. A batch cannot be used concurrently.
type batch struct {
	b    *badger.WriteBatch
	db   *Database
	size int
	ops  []batchOp // needed for Replay since WriteBatch is opaque
}

// Put inserts the given value into the batch for later committing.
//
// BadgerDB's WriteBatch.Set does NOT copy key/value — it stores the slice
// references and only reads them when Flush triggers the actual commit.
// The geth batch contract (matching pebble/leveldb) lets callers reuse the
// buffers after Put returns, so we must copy here. Failing to do so causes
// silent data corruption when callers (e.g. trie commit reusing RLP encoder
// buffers) overwrite the buffer between Put and Write, manifesting as
// "Unexpected trie node" mismatches at random blocks.
func (b *batch) Put(key, value []byte) error {
	s := fmt.Sprintf("OPType: BatchPut, key: %x, size: %d, valueSize: %d, gid: %d", key, len(key), len(value), common.GoroutineID())
	common.WriteGlobalLog(s)

	kc := common.CopyBytes(key)
	vc := common.CopyBytes(value)
	if err := b.b.Set(kc, vc); err != nil {
		return err
	}
	b.size += len(kc) + len(vc)
	b.ops = append(b.ops, batchOp{key: kc, value: vc})
	return nil
}

// Delete inserts the key removal into the batch for later committing.
// See Put for why we copy.
func (b *batch) Delete(key []byte) error {
	s := fmt.Sprintf("OPType: BatchDelete, key: %x, size: %d, gid: %d", key, len(key), common.GoroutineID())
	common.WriteGlobalLog(s)

	kc := common.CopyBytes(key)
	if err := b.b.Delete(kc); err != nil {
		return err
	}
	b.size += len(kc)
	b.ops = append(b.ops, batchOp{del: true, key: kc})
	return nil
}

// DeleteRange removes all keys in the range [start, end) from the batch for
// later committing, inclusive on start, exclusive on end.
func (b *batch) DeleteRange(start, end []byte) error {
	if end == nil {
		end = ethdb.MaximumKey
	}
	// BadgerDB WriteBatch has no native range delete. We must collect keys
	// in the range and delete them individually.
	err := b.db.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(start); it.Valid(); it.Next() {
			key := it.Item().KeyCopy(nil)
			if bytes.Compare(key, end) >= 0 {
				break
			}
			if err := b.b.Delete(key); err != nil {
				return err
			}
			b.size += len(key)
		}
		return nil
	})
	if err != nil {
		return err
	}
	b.ops = append(b.ops, batchOp{rangeStart: start, rangeEnd: end})
	return nil
}

// ValueSize retrieves the amount of data queued up for writing.
func (b *batch) ValueSize() int {
	s := fmt.Sprintf("OPType: GetBatchValueSize, size: %d, gid: %d", b.size, common.GoroutineID())
	common.WriteGlobalLog(s)
	return b.size
}

// Write flushes any accumulated data to disk.
func (b *batch) Write() error {
	b.db.quitLock.RLock()
	defer b.db.quitLock.RUnlock()

	s := fmt.Sprintf("OPType: BatchPutCommit, gid: %d", common.GoroutineID())
	common.WriteGlobalLog(s)

	if b.db.closed {
		return badger.ErrDBClosed
	}
	return b.b.Flush()
}

// Reset resets the batch for reuse.
func (b *batch) Reset() {
	b.b.Cancel()
	b.b = b.db.db.NewWriteBatch()
	b.size = 0
	b.ops = b.ops[:0]
}

// Replay replays the batch contents.
func (b *batch) Replay(w ethdb.KeyValueWriter) error {
	for _, op := range b.ops {
		if op.rangeStart != nil {
			// Range delete operation
			if rangeDeleter, ok := w.(ethdb.KeyValueRangeDeleter); ok {
				if err := rangeDeleter.DeleteRange(op.rangeStart, op.rangeEnd); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("ethdb.KeyValueWriter does not implement DeleteRange")
			}
		} else if op.del {
			if err := w.Delete(op.key); err != nil {
				return err
			}
		} else {
			if err := w.Put(op.key, op.value); err != nil {
				return err
			}
		}
	}
	return nil
}

// badgerIterator is a wrapper of underlying iterator in storage engine.
// BadgerDB iterators must live within a transaction, so we hold a reference
// to the read-only transaction and discard it on Release.
//
// The badger iterator is not thread-safe.
type badgerIterator struct {
	txn       *badger.Txn
	iter      *badger.Iterator
	moved     bool
	released  bool
	exhausted bool

	// Cached current key/value. BadgerDB values are only valid within the
	// transaction callback or until the iterator advances, so we must copy them.
	curKey   []byte
	curValue []byte
	err      error
}

// cacheCurrentEntry copies the current key and value from the iterator.
func (iter *badgerIterator) cacheCurrentEntry() {
	item := iter.iter.Item()
	iter.curKey = item.KeyCopy(nil)
	iter.curValue, iter.err = item.ValueCopy(nil)
}

// Next moves the iterator to the next key/value pair. It returns whether the
// iterator is exhausted. Safe to call after exhaustion or release — both
// short-circuit without touching the underlying badger iterator, whose Next
// would nil-deref it.item once the range is drained.
func (iter *badgerIterator) Next() bool {
	s := "OPType: IteratorNext"
	common.WriteGlobalLog(s)

	if iter.exhausted || iter.released {
		return false
	}
	if iter.moved {
		iter.moved = false
		if !iter.iter.Valid() {
			iter.exhausted = true
			iter.curKey, iter.curValue = nil, nil
			return false
		}
		return true
	}
	iter.iter.Next()
	if !iter.iter.Valid() {
		iter.exhausted = true
		iter.curKey, iter.curValue = nil, nil
		return false
	}
	iter.cacheCurrentEntry()
	return true
}

// Error returns any accumulated error.
func (iter *badgerIterator) Error() error {
	return iter.err
}

// Key returns the key of the current key/value pair, or nil if done.
func (iter *badgerIterator) Key() []byte {
	return iter.curKey
}

// Value returns the value of the current key/value pair, or nil if done.
func (iter *badgerIterator) Value() []byte {
	return iter.curValue
}

// Release releases associated resources. Release should always succeed and can
// be called multiple times without causing error.
func (iter *badgerIterator) Release() {
	if !iter.released {
		iter.iter.Close()
		iter.txn.Discard()
		iter.released = true
	}
}
