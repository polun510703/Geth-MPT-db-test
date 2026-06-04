package workload

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/badger"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/hashdb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
)

// ExtractConfig controls one extraction run.
type ExtractConfig struct {
	Datadir        string // geth datadir, e.g. ~/ethereum/execution/data-kvsep
	ValueThreshold int    // matches the value used during sync (1 for kvsep, 1048576 for nosep)
	OutDir         string // where to write accounts_<tag>.bin / txs_<tag>.bin / manifest_<tag>.json
	Tag            string // file-name suffix, conventionally "kvsep" / "nosep"
	SampleEvery    int    // tx sample stride (1 = every tx, 8 = ChainKV default)
	AccountsMax    int    // stop after N accounts (0 = unlimited)
	TxMaxBlock     uint64 // stop tx scan after this block (0 = scan up to head)
}

// Manifest is the metadata sidecar written next to the bin files.
type Manifest struct {
	Tag             string `json:"tag"`
	Datadir         string `json:"datadir"`
	ValueThreshold  int    `json:"value_threshold"`
	HeadBlock       uint64 `json:"head_block"`
	HeadHashHex     string `json:"head_hash"`
	StateRootHex    string `json:"state_root"`
	AccountCount    int    `json:"account_count"`
	TxCount         int    `json:"tx_count"`
	TxSampleEvery   int    `json:"tx_sample_every"`
	TxScannedBlocks uint64 `json:"tx_scanned_blocks"`
	ExtractedAt     string `json:"extracted_at"`
	ExtractWallSec  string `json:"extract_wall_sec"`
}

// OpenChaindataReadOnly opens the BadgerDB-backed chaindata at
// <datadir>/geth/chaindata in read-only mode and wraps it with the freezer
// at <datadir>/geth/chaindata/ancient.
//
// The returned ethdb.Database routes header/body/receipt accesses through the
// freezer transparently; the raw KeyValueStore is also returned for callers
// that need to inspect badger-level Stat() (e.g. LSM/vlog size).
func OpenChaindataReadOnly(datadir string, valueThreshold int) (ethdb.Database, ethdb.KeyValueStore, error) {
	chaindata := filepath.Join(datadir, "geth", "chaindata")
	ancient := filepath.Join(chaindata, "ancient")

	diskdb, err := badger.New(chaindata, 1024, 256, "obs/", true /*readonly*/, valueThreshold)
	if err != nil {
		return nil, nil, fmt.Errorf("open badger %s: %w", chaindata, err)
	}
	db, err := rawdb.NewDatabaseWithFreezer(diskdb, ancient, "obs/", true)
	if err != nil {
		diskdb.Close()
		return nil, nil, fmt.Errorf("open freezer %s: %w", ancient, err)
	}
	return db, diskdb, nil
}

// MakeTrieDB constructs a read-only triedb that matches the persistent state
// scheme stored at <datadir>/geth (hash or path). For path scheme it wires the
// triedb journal directory to <datadir>/geth/triedb.
func MakeTrieDB(db ethdb.Database, datadir string) *triedb.Database {
	scheme := rawdb.ReadStateScheme(db)
	cfg := &triedb.Config{}
	if scheme == rawdb.HashScheme {
		cfg.HashDB = hashdb.Defaults
		return triedb.NewDatabase(db, cfg)
	}
	// Default + PathScheme branch.
	pc := *pathdb.ReadOnly
	pc.JournalDirectory = filepath.Join(datadir, "geth", "triedb")
	cfg.PathDB = &pc
	return triedb.NewDatabase(db, cfg)
}

// addressCollector is a state.DumpCollector that streams only address bytes
// out to disk and ignores everything else (balance, code, storage).
type addressCollector struct {
	w       *AccountWriter
	limit   int // 0 = unlimited
	stopErr error
}

func (a *addressCollector) OnRoot(_ common.Hash) {}

func (a *addressCollector) OnAccount(_ *common.Address, acct state.DumpAccount) {
	if a.stopErr != nil {
		return
	}
	// AddressHash is always populated by DumpToCollector regardless of whether
	// the preimage (the 20-byte Address) is available. We dump the 32-byte
	// hash and read by GetAccountByHash at benchmark time.
	if len(acct.AddressHash) != 32 {
		return
	}
	var k common.Hash
	copy(k[:], acct.AddressHash)
	if err := a.w.Write(k); err != nil {
		a.stopErr = err
		return
	}
	if a.limit > 0 && a.w.Count() >= a.limit {
		a.stopErr = errReachedLimit
	}
}

var errReachedLimit = fmt.Errorf("reached account limit")

// Run executes one extraction. It opens the chaindata, iterates the head-state
// trie writing accounts, then walks all blocks writing sampled tx hashes,
// and finally drops a manifest.
func Run(cfg ExtractConfig) (Manifest, error) {
	if cfg.SampleEvery <= 0 {
		cfg.SampleEvery = 8
	}
	if cfg.Tag == "" {
		return Manifest{}, fmt.Errorf("workload: ExtractConfig.Tag is required")
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return Manifest{}, err
	}

	start := time.Now()
	db, _, err := OpenChaindataReadOnly(cfg.Datadir, cfg.ValueThreshold)
	if err != nil {
		return Manifest{}, err
	}
	defer db.Close()

	headBlock := rawdb.ReadHeadBlock(db)
	if headBlock == nil {
		return Manifest{}, fmt.Errorf("workload: ReadHeadBlock returned nil — empty chaindata?")
	}
	headNum := headBlock.NumberU64()
	stateRoot := headBlock.Root()

	// --- 1. Accounts ---
	accountsPath := filepath.Join(cfg.OutDir, fmt.Sprintf("accounts_%s.bin", cfg.Tag))
	aw, err := NewAccountWriter(accountsPath)
	if err != nil {
		return Manifest{}, err
	}

	trieDB := MakeTrieDB(db, cfg.Datadir)
	sdb := state.NewDatabase(trieDB, nil /*snapshot*/)
	st, err := state.New(stateRoot, sdb)
	if err != nil {
		aw.Close()
		return Manifest{}, fmt.Errorf("workload: state.New(root=%x): %w", stateRoot, err)
	}

	coll := &addressCollector{w: aw, limit: cfg.AccountsMax}
	st.DumpToCollector(coll, &state.DumpConfig{
		SkipCode:          true,
		SkipStorage:       true,
		OnlyWithAddresses: false,             // we use AddressHash, not preimage
		Max:               uint64(cfg.AccountsMax), // 0 → unlimited
	})
	if coll.stopErr != nil && coll.stopErr != errReachedLimit {
		aw.Close()
		return Manifest{}, fmt.Errorf("workload: account dump: %w", coll.stopErr)
	}
	if err := aw.Close(); err != nil {
		return Manifest{}, err
	}
	accountCount := aw.Count()

	// --- 2. Transactions (sampled, ascending block order) ---
	txsPath := filepath.Join(cfg.OutDir, fmt.Sprintf("txs_%s.bin", cfg.Tag))
	tw, err := NewTxWriter(txsPath)
	if err != nil {
		return Manifest{}, err
	}
	stride := cfg.SampleEvery
	txSeen := 0
	endN := headNum
	if cfg.TxMaxBlock > 0 && cfg.TxMaxBlock < endN {
		endN = cfg.TxMaxBlock
	}
	for n := uint64(1); n <= endN; n++ {
		hash := rawdb.ReadCanonicalHash(db, n)
		if hash == (common.Hash{}) {
			continue
		}
		body := rawdb.ReadBody(db, hash, n)
		if body == nil {
			continue
		}
		for _, tx := range body.Transactions {
			if txSeen%stride == 0 {
				if err := tw.Write(tx.Hash(), n); err != nil {
					tw.Close()
					return Manifest{}, fmt.Errorf("workload: tx write at block %d: %w", n, err)
				}
			}
			txSeen++
		}
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, err
	}

	// --- 3. Manifest ---
	manifest := Manifest{
		Tag:             cfg.Tag,
		Datadir:         cfg.Datadir,
		ValueThreshold:  cfg.ValueThreshold,
		HeadBlock:       headNum,
		HeadHashHex:     headBlock.Hash().Hex(),
		StateRootHex:    stateRoot.Hex(),
		AccountCount:    accountCount,
		TxCount:         tw.Count(),
		TxSampleEvery:   stride,
		TxScannedBlocks: endN,
		ExtractedAt:     time.Now().Format(time.RFC3339),
		ExtractWallSec:  fmt.Sprintf("%.1f", time.Since(start).Seconds()),
	}
	manifestPath := filepath.Join(cfg.OutDir, fmt.Sprintf("manifest_%s.json", cfg.Tag))
	mf, err := os.Create(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	enc := json.NewEncoder(mf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&manifest); err != nil {
		mf.Close()
		return Manifest{}, err
	}
	if err := mf.Close(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}
