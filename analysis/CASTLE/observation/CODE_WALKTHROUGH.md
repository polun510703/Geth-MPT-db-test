# observation/ 程式碼解說

本文件說明 [analysis/CASTLE/observation/](.) 底下四個核心檔案的職責、它們之間的依賴關係，以及從「抽 account / transaction → 打 workload 給 DB」的完整流程。

---

## 1. 四個檔案的關係

```
┌──────────────────────────────────────────────────────────────────┐
│ observation_test.go              (測試入口：兩個 Test*)            │
│   ├── TestExtractWorkload  ──┐                                    │
│   └── TestObservation      ──┼─→ 呼叫 workload 套件                │
└──────────────────────────────┼────────────────────────────────────┘
                               │
                               ▼
        ┌──────────────────────────────────────────────┐
        │  workload/  (package workload)                │
        │                                                │
        │  extract.go   ── 從 chaindata 抽資料寫成 .bin   │
        │     │  - OpenChaindataReadOnly()               │
        │     │  - MakeTrieDB()                          │
        │     │  - Run()  (主要 extractor)               │
        │     │  - addressCollector (吃 state trie)      │
        │     ▼                                          │
        │  workload.go  ── .bin 的 reader/writer + 工具   │
        │     │  - AccountWriter / TxWriter (寫檔)        │
        │     │  - LoadAccounts / LoadTxs   (讀檔)        │
        │     │  - ParseRatio("tx:acct")                  │
        │     │                                           │
        │  pattern.go   ── access pattern 產生器          │
        │        - Pattern 介面 (Next() int)              │
        │        - NewPattern("uniform|zipf|seq|recent")  │
        └──────────────────────────────────────────────┘
```

四個檔案分工：

| 檔案 | 角色 | 主要型別／函式 |
|------|------|----------------|
| [extract.go](workload/extract.go) | **抽資料**：開啟 chaindata，把 state trie 的 account hash 跟區塊裡的 tx hash 寫成 `.bin` | `ExtractConfig`、`Run()`、`addressCollector` |
| [workload.go](workload/workload.go) | **資料層**：把 `.bin` 序列化／反序列化的 reader、writer，以及小工具 | `AccountKey`、`TxEntry`、`LoadAccounts/LoadTxs`、`AccountWriter/TxWriter`、`ParseRatio` |
| [pattern.go](workload/pattern.go) | **存取模式**：給定資料集大小 N，回傳一個「下一個 index 是多少」的 lazy iterator | `Pattern` 介面、`NewPattern()`、四種實作 |
| [observation_test.go](observation_test.go) | **編排**：兩個 `Test*` 是進入點，把上面三個檔案組起來跑 benchmark 並輸出 latency / metrics | `TestExtractWorkload`、`TestObservation` |

關係的本質：

- `observation_test.go` 是「使用者」；它從不直接碰 BadgerDB，全部透過 `workload` 套件。
- `workload.go` 是「資料層」，被 `extract.go`（寫）跟 `observation_test.go`（讀）共用，是兩者中間的橋。
- `pattern.go` 是「access pattern 工廠」，只在 benchmark 階段被 `observation_test.go` 用到，extract 階段完全不需要。
- `extract.go` 跟 `pattern.go` 互不依賴 — 抽資料跟產生 access pattern 是兩個獨立階段。

---

## 2. 從抽 account / transaction 到打 workload 給 DB 的完整流程

整體分成 **兩階段**，對應到 `observation_test.go` 裡的兩個測試：

### 階段 A — extract（一次性，產生 `.bin` 資料集）

進入點：[observation_test.go:94 TestExtractWorkload](observation_test.go#L94)
要求環境變數 `OBS_EXTRACT=1` 才會跑（避免一般 `go test ./...` 不小心觸發數分鐘的 disk 工作）。

1. **決定 mode → datadir / valueThreshold**
   [observation_test.go:61 modeConfig()](observation_test.go#L61) 把 `OBS_MODE=kvsep|nosep` 映射到對應的 datadir 跟 BadgerDB 的 `valueThreshold`（kvsep=1，nosep=1048576，這要跟同步時用的值一致，否則讀不出來）。

2. **組 ExtractConfig 並呼叫 workload.Run()**
   [observation_test.go:109-122](observation_test.go#L109-L122)

3. **Run() 主流程** — [extract.go:123 Run()](workload/extract.go#L123)
   1. `OpenChaindataReadOnly()` 以 read-only 開 `<datadir>/geth/chaindata`（Badger）並掛上 `ancient/` freezer，回傳 `ethdb.Database`。[extract.go:54](workload/extract.go#L54)
   2. 從 `rawdb.ReadHeadBlock(db)` 拿到 head block 跟 state root。[extract.go:141-146](workload/extract.go#L141-L146)
   3. **抽 accounts**：
      - 建 `AccountWriter`（[workload.go:90](workload/workload.go#L90)），會把每個 32-byte address hash 直接寫進 `accounts_<tag>.bin`，**不在記憶體裡累積**，避免上千萬個 account 爆掉。
      - 用 `MakeTrieDB()` + `state.New(stateRoot, sdb)` 開出 head 的 state trie。[extract.go:155-161](workload/extract.go#L155-L161)
      - 把 `addressCollector`（[extract.go:89](workload/extract.go#L89)）丟給 `st.DumpToCollector(...)`，它會 walk 整個 state trie，每碰到一個 account 就呼叫 `OnAccount()`，把 32-byte `AddressHash`（也就是 `keccak256(address)`）寫進 writer。注意：寫的是 **hash 而不是 20-byte 原始 address**，因為 preimage 在一個同步好的節點上不一定存在，但 `AddressHash` 一定有。[extract.go:97-116](workload/extract.go#L97-L116)
   4. **抽 transactions**：
      - 從 block 1 走到 head（或 `TxMaxBlock`）。
      - 對每個 block：`ReadCanonicalHash` → `ReadBody` → 對 body 裡每筆 tx，每 `SampleEvery` 筆抽一筆（預設 `8`，跟 ChainKV 對齊），寫進 `txs_<tag>.bin`。
      - 每筆 entry 格式：32-byte tx hash + 8-byte big-endian block number（共 40 byte）。block number 是後面 `recent` pattern 要用的。[extract.go:185-209](workload/extract.go#L185-L209)
      - 因為是 block 升冪走訪，**檔尾就是最新的 tx**，這個順序保證很重要。
   5. **寫 manifest**：sidecar JSON，記錄 head block / state root / counts / wall time。[extract.go:215-242](workload/extract.go#L215-L242)

   產出：
   ```
   analysis/CASTLE/observation/data/
     ├── accounts_kvsep.bin       (32 bytes × N)
     ├── txs_kvsep.bin            (40 bytes × M)
     └── manifest_kvsep.json
   ```

### 階段 B — observation（讀 benchmark，可重複跑）

進入點：[observation_test.go:136 TestObservation](observation_test.go#L136)
要求 `OBS_RUN=1` 才會跑。實際上是被 [run_observation.sh](run_observation.sh) 包起來：腳本順便起背景 `iostat`、抓 benchmark 區間、產生 `iostat_summary.csv`。

1. **讀環境變數**：mode / pattern / ratio / nops / seed / zipf_s / recent_frac。
   [observation_test.go:141-147](observation_test.go#L141-L147)

2. **解析 ratio**：`workload.ParseRatio("3:7")` → `txR=3, acctR=7`。
   [workload.go:155 ParseRatio()](workload/workload.go#L155)

3. **載入 `.bin`** 成為記憶體切片：
   - `workload.LoadAccounts(...)` → `[]AccountKey`（32-byte hash 陣列）[workload.go:31](workload/workload.go#L31)
   - `workload.LoadTxs(...)` → `[]TxEntry{Hash, Block}` [workload.go:61](workload/workload.go#L61)

4. **建兩個獨立的 Pattern**（tx 跟 account 各一個），刻意用不同 seed 避免兩條序列「鎖步」。
   [observation_test.go:184-198](observation_test.go#L184-L198)
   ```go
   pTx.Seed   = seed
   pAcct.Seed = seed ^ 0x1E3779B97F4A7C15
   txSeq,   _ := workload.NewPattern(pattern, pTx)
   acctSeq, _ := workload.NewPattern(pattern, pAcct)
   ```

5. **開 chaindata（read-only）+ 建 state trie**：
   - `workload.OpenChaindataReadOnly(...)` 拿 `db, diskdb`（`diskdb` 是底層 Badger，後面要拉 expvar metrics 用）。
   - `MakeTrieDB(db, datadir)` → `triedb.Database`
   - `trie.NewStateTrie(trie.StateTrieID(stateRoot), trieDB)` → 可以用 `GetAccountByHash` 查詢。[observation_test.go:201-216](observation_test.go#L201-L216)

6. **主迴圈 — 按 ratio 交錯送 tx read 跟 account read**：
   [observation_test.go:229-254](observation_test.go#L229-L254)
   ```
   while opsDone < nops:
       for k in 0..txR:    ← 連送 txR 個 tx 讀
           h = txs[txSeq.Next()].Hash
           rawdb.ReadCanonicalTransaction(db, h)
       for k in 0..acctR:  ← 連送 acctR 個 account 讀
           hash = accounts[acctSeq.Next()]
           st.GetAccountByHash(hash)
   ```
   - tx 讀走 `rawdb.ReadCanonicalTransaction` — 進入 freezer/chaindata 撈 body / receipt。
   - account 讀走 state trie `GetAccountByHash(keccak256(address))` — 觸發 trie node 載入跟 LSM lookup。
   - 每一次讀都用 `time.Now()` 量 latency，寫進 `lat[]` 跟 `opTypes[]`（`'t'` 或 `'a'`）。

7. **輸出**：
   - `latency/latency_per_op.csv` — 每一筆 op 的 latency。[observation_test.go:262-265](observation_test.go#L262-L265)
   - `latency/summary.json` — overall / tx / acct 的 p50/p90/p95/p99/p999/max + qps + BadgerDB stat。[observation_test.go:268-284](observation_test.go#L268-L284)
   - `badger_metrics/badger_metrics_obs_<ts>.csv` — 從 `common.DumpBadgerMetricsTo` 拉 expvar。[observation_test.go:287-293](observation_test.go#L287-L293)
   - `[OBS] start=<ns>` / `[OBS] end=<ns>` 印到 stdout，給 `run_observation.sh` 切 iostat window 用。

### 整體資料流圖

```
chaindata (Badger + freezer)
        │
        │ extract.go: Run()
        │   - state trie walk → AddressHash
        │   - rawdb.ReadBody → tx hashes (每 SampleEvery 筆抽 1)
        ▼
   accounts_<mode>.bin   txs_<mode>.bin   manifest_<mode>.json
        │                     │
        │ workload.LoadAccounts  workload.LoadTxs
        ▼                     ▼
     []AccountKey         []TxEntry
        │                     │
        │       pattern.NewPattern(kind, params) → 各自一個 Pattern
        │                     │
        ▼                     ▼
  acctSeq.Next() = i_a   txSeq.Next() = i_t
        │                     │
        │  accounts[i_a]          txs[i_t].Hash
        ▼                     ▼
  st.GetAccountByHash()   rawdb.ReadCanonicalTransaction()
        └──────────┬──────────┘
                   ▼
           Badger / freezer 讀取
                   ▼
              量 latency
                   ▼
         summary.json / per_op.csv
```

---

## 3. Access pattern 一覽

定義都在 [pattern.go](workload/pattern.go)。共四種，由字串 `kind` 決定要用哪一個；同一支 `NewPattern()` 可以同時拿來當 tx 序列或 account 序列。

### `Pattern` 介面

```go
type Pattern interface {
    Next() int   // 回傳下一個 index，範圍 [0, N)
}
```

`Next()` 回傳的是 **資料集裡的 index**，呼叫端再用這個 index 去 `accounts[i]` 或 `txs[i]` 拿真正的 hash。所有 pattern 都是 lazy iterator，沒有預先把 N 個 index 算好。

### 共用參數 `PatternParams`

[pattern.go:41](workload/pattern.go#L41)

```go
type PatternParams struct {
    N           int     // 資料集大小 (len(accounts) 或 len(txs))
    Seed        int64   // RNG 種子
    ZipfS       float64 // zipf 偏度，必須 > 1 (預設 1.1)
    RecentFrac  float64 // recent 模式的 hot zone 比例 (0,1]，預設 0.1
}
```

並不是每個 pattern 都用到所有欄位，只有 `N` 是必要的。

### 三種 pattern 的選法

| pattern | index 怎麼選 | 對 account / tx 的意義 |
|---------|--------------|------------------------|
| `uniform` | `rng.Intn(N)` — 在 `[0, N)` 之間均勻隨機 | 任何 account / tx 被選中的機率都一樣。代表「亂讀」、cache 友善度最差的對照組 |
| `seq` | `i % N`，每次呼叫 `i++` | 從 0 走到 N-1 後折返。資料集本身就是「state trie 走訪順序」（account）或「block 升冪」（tx），所以這代表「按順序掃」，會碰上很多空間區域性 |
| `zipf` | `rand.Zipf(s=ZipfS, v=1, imax=N-1)` | 標準 zipf 偏斜，小 index 出現機率高。**注意**：因為 account / tx 在 `.bin` 裡的順序是 trie/block 自然順序，「小 index」並沒有特別語意 — 主要就是用來模擬「少數熱點 key、長尾冷 key」的存取分佈 |
| `recent` | 一樣是 zipf，但 zipf 的範圍從 `[0, N-1]` 縮到 **`[N - hot, N)`**，其中 `hot = floor(N * RecentFrac)` | 把 zipf 偏向「序列尾端」。對 **tx** 來說特別有意義：因為 extract 階段是按 block 升冪寫的，序列尾端 = 最新的 tx，這就是「最近區塊的存取更熱」的模擬。對 account 來說「尾端」沒有特別語意，只是 hot zone 在後段而已 |

### `uniform` 實作

[pattern.go:15-20](workload/pattern.go#L15-L20)
```go
type uniformPattern struct {
    n   int
    rng *rand.Rand
}
func (p *uniformPattern) Next() int { return p.rng.Intn(p.n) }
```
每次 `rng.Intn(N)`，沒有狀態。

### `seq` 實作

[pattern.go:29-38](workload/pattern.go#L29-L38)
```go
type seqPattern struct {
    n int
    i int
}
func (p *seqPattern) Next() int {
    v := p.i % p.n
    p.i++
    return v
}
```
內部維護 `i`，wrap-around。完全 deterministic，不吃 seed。

### `zipf` 實作

[pattern.go:63-72](workload/pattern.go#L63-L72)
```go
case "zipf":
    s := p.ZipfS
    if s <= 1.0 { s = 1.1 }     // Go 標準庫要求 s > 1
    z := rand.NewZipf(rng, s, 1.0, uint64(p.N-1))
    return &zipfPattern{z: z}, nil
```
直接用 `math/rand` 的 `Zipf`，`imax = N-1` 表示輸出範圍 `[0, N-1]`。
偏度 `s` 越大 → 熱點越尖；`s=1.1` 是一個還挺實際的設定。

### `recent` 實作（最有研究意義的一個）

[pattern.go:73-91](workload/pattern.go#L73-L91)
```go
case "recent":
    frac := p.RecentFrac
    if frac <= 0 || frac > 1 { frac = 0.1 }
    hot := int(float64(p.N) * frac)            // hot zone 大小
    if hot <= 0 { hot = 1 }
    offset := p.N - hot                        // hot zone 的起點
    s := p.ZipfS
    if s <= 1.0 { s = 1.1 }
    z := rand.NewZipf(rng, s, 1.0, uint64(hot-1))
    return &zipfPattern{z: z, offset: offset}, nil
```

`zipfPattern.Next()` 是：

[pattern.go:27](workload/pattern.go#L27)
```go
func (p *zipfPattern) Next() int { return int(p.z.Uint64()) + p.offset }
```

所以 `recent` 的選法等於：
1. 先用 zipf 在 `[0, hot-1]` 抽一個小範圍偏斜的數
2. 加上 `offset = N - hot`，平移到資料集的尾端
3. 最終 index 落在 `[N - hot, N)`，且尾端更熱

對 tx 來說，因為 `txs_<mode>.bin` 在 extract 階段是 block 升冪寫入（[extract.go:191](workload/extract.go#L191)），所以 `recent` 真正模擬的是「最近的區塊讀得最多，越早期越少」這種真實節點的存取行為。

### tx 跟 account 為什麼要兩個獨立的 Pattern？

[observation_test.go:184-198](observation_test.go#L184-L198)

```go
pTx.Seed   = seed
pAcct.Seed = seed ^ 0x1E3779B97F4A7C15
```

兩條序列共用同一個 RNG 會「鎖步」 — 第 k 次 tx 的選擇跟第 k 次 account 選擇會綁定（因為 zipf/uniform 都讀同一支 `rand.Rand`），可能產生意外的相關性。所以用 XOR 一個常數（黃金比例魔術數）拆出兩個獨立 seed，讓兩條序列彼此獨立。

---

## 4. 兩個小細節，講解時可以順便提到

1. **account 用 hash 而不是 address**
   [extract.go:97-110](workload/extract.go#L97-L110) — 同步好的節點不一定保留 address preimage，但 `AddressHash` 一定有；查詢時也用 `GetAccountByHash` 配對，所以整條路徑都用 32-byte hash。

2. **ratio 跑法是「burst」不是逐筆交錯**
   [observation_test.go:229-254](observation_test.go#L229-L254) — `3:7` 是「先連讀 3 筆 tx、再連讀 7 筆 account」、然後重複，**不是** 30% / 70% 的隨機混合。這對 cache 行為的影響不可忽略，講解時值得特別點出來。
