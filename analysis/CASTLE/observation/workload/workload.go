package workload

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/ethereum/go-ethereum/common"
)

// AccountKey is the keccak256(address) used as the state-trie lookup key.
// 32 bytes. We store hashes (not 20-byte addresses) because preimages aren't
// always available in a synced node, but AddressHash always is.
type AccountKey = common.Hash

// TxEntry is a tx hash plus the block number it was seen in.
// Block number lets the "recent" pattern bias toward the latest blocks.
type TxEntry struct {
	Hash  common.Hash
	Block uint64
}

const (
	accountKeyLen = 32
	txEntryLen    = 32 + 8 // hash(32) + big-endian block number(8)
)

// LoadAccounts reads accounts_<mode>.bin written by extract.go.
// The file is a flat sequence of 32-byte address hashes with no header.
func LoadAccounts(path string) ([]AccountKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size()%accountKeyLen != 0 {
		return nil, fmt.Errorf("workload: %s has size %d not divisible by %d", path, fi.Size(), accountKeyLen)
	}
	n := int(fi.Size() / accountKeyLen)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make([]AccountKey, n)
	buf := make([]byte, accountKeyLen)
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, fmt.Errorf("workload: accounts read at %d: %w", i, err)
		}
		copy(out[i][:], buf)
	}
	return out, nil
}

// LoadTxs reads txs_<mode>.bin written by extract.go.
// Each entry is 32-byte hash + 8-byte big-endian block number.
// The slice is returned in file order (which extract.go writes sorted by
// ascending block number — so the tail is the most recent).
func LoadTxs(path string) ([]TxEntry, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size()%txEntryLen != 0 {
		return nil, fmt.Errorf("workload: %s has size %d not divisible by %d", path, fi.Size(), txEntryLen)
	}
	n := int(fi.Size() / txEntryLen)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make([]TxEntry, n)
	buf := make([]byte, txEntryLen)
	for i := 0; i < n; i++ {
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, fmt.Errorf("workload: txs read at %d: %w", i, err)
		}
		copy(out[i].Hash[:], buf[:32])
		out[i].Block = binary.BigEndian.Uint64(buf[32:])
	}
	return out, nil
}

// AccountWriter streams 32-byte address hashes to disk as the extractor walks
// the state trie, avoiding the need to hold all of them in memory.
type AccountWriter struct {
	f *os.File
	n int
}

// NewAccountWriter opens path for writing accounts.bin.
func NewAccountWriter(path string) (*AccountWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &AccountWriter{f: f}, nil
}

// Write appends a 32-byte address hash.
func (w *AccountWriter) Write(key AccountKey) error {
	if _, err := w.f.Write(key[:]); err != nil {
		return err
	}
	w.n++
	return nil
}

// Count returns the number of addresses written.
func (w *AccountWriter) Count() int { return w.n }

// Close flushes and closes the underlying file.
func (w *AccountWriter) Close() error { return w.f.Close() }

// TxWriter streams 32-byte tx hashes + 8-byte block numbers to disk.
type TxWriter struct {
	f   *os.File
	buf [txEntryLen]byte
	n   int
}

// NewTxWriter opens path for writing txs.bin.
func NewTxWriter(path string) (*TxWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &TxWriter{f: f}, nil
}

// Write appends one tx-entry. Caller must invoke entries in ascending block
// order so the trailing slice is the "recent" zone for the recent pattern.
func (w *TxWriter) Write(hash common.Hash, block uint64) error {
	copy(w.buf[:32], hash[:])
	binary.BigEndian.PutUint64(w.buf[32:], block)
	if _, err := w.f.Write(w.buf[:]); err != nil {
		return err
	}
	w.n++
	return nil
}

// Count returns the number of entries written.
func (w *TxWriter) Count() int { return w.n }

// Close flushes and closes the underlying file.
func (w *TxWriter) Close() error { return w.f.Close() }

// ParseRatio splits "tx:acct" into (tx, acct). Returns an error if either side
// is negative or both are zero.
func ParseRatio(s string) (txN, acctN int, err error) {
	_, err = fmt.Sscanf(s, "%d:%d", &txN, &acctN)
	if err != nil {
		return 0, 0, fmt.Errorf("workload: parse ratio %q: %w", s, err)
	}
	if txN < 0 || acctN < 0 || (txN == 0 && acctN == 0) {
		return 0, 0, fmt.Errorf("workload: invalid ratio %q (need non-negative, at least one >0)", s)
	}
	return txN, acctN, nil
}
