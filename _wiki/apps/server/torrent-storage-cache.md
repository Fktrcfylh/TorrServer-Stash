# Torrent storage cache (torrstor)

**App:** server
**Status:** stable
**Last reviewed:** 2026-09-14 (commit `215f6fca` + staged pin Phases 1–5)

## Summary
Custom anacrolix `storage.ClientImpl`: one `Cache` per infohash holding all pieces in RAM (`MemPiece`) or as one file per piece on disk (`DiskPiece`). Cache size is bounded by `capacity`; eviction drops oldest-accessed pieces outside active reader windows. The cache also drives anacrolix piece priorities (Now/Next/Readahead/High/Normal) around each reader, and exposes piece/reader state for the web cache map. A per-cache immutable pin set (`pin`, `{want, drop}`) exempts every piece of a pinned torrent from eviction, keeps wanted incomplete pieces at `Normal` priority and deletes dropped (watched) pieces outside files with a reader; `Close` keeps the files of torrents whose DB row is pinned — see [Torrent pin](torrent-pin.md).

## Entry points
- `server/torr/btserver.go:98` — `torrstor.NewStorage(settings.BTsets.CacheSize)`, set as anacrolix `DefaultStorage` (`:99`).
- `server/torr/storage/torrstor/storage.go:27` — `OpenTorrent`, called by anacrolix per torrent.
- `server/torr/torrent.go:125-128` — in `WaitInfo` (`server/torr/torrent.go:115-136`, called from `GotInfo` and `LoadTorrent`), `Torrent` fetches its cache via `GetCache`, calls `SetTorrent`, then `applyPin` → `Cache.SetPin` (`:140-189`; also called by the 1-minute resume tick).
- `server/torr/torrent.go:327-338` — `Torrent.NewReader` / `CloseReader` wrap `Cache.NewReader` / `Cache.CloseReader`; used by `server/torr/stream.go:85,90`, `server/torrfs/torrfile.go:46,83`, `server/tgbot/upload/torrfile.go:68`.
- `server/torr/torrent.go:261` — `progressEvent` (1 s ticker, `torrent.go:223`) reads `GetState().Filled` into `PreloadedBytes`.
- `server/torr/torrent.go:296` — `expired()` reads `Cache.PinPending()`.
- `server/torr/torrent.go:504-511` — `CacheState()` for `POST /cache {action:get}` (`server/web/api/cache.go:47-63`).
- `server/torr/torrent.go:348` — `Torrent.Drop()` in `drop()` closes the storage synchronously → `Cache.Close`.

## Key files
| File | Role |
|---|---|
| `server/torr/storage/storage.go` | Interface: anacrolix `ClientImpl` + `CloseHash` (`:8-12`) |
| `server/torr/storage/torrstor/storage.go` | `Storage`: hash→`*Cache` map, open/close |
| `server/torr/storage/torrstor/cache.go` | `Cache`: pieces, readers, eviction, priorities, pin set + drop, state, close |
| `server/torr/storage/torrstor/piece.go` | `Piece`: dispatch to mem/disk by `UseDisk`, completion flag |
| `server/torr/storage/torrstor/mempiece.go` | RAM buffer per piece |
| `server/torr/storage/torrstor/diskpiece.go` | File per piece `TorrentsSavePath/<hash>/<id>` |
| `server/torr/storage/torrstor/piecefake.go` | Stub returned for unknown piece index |
| `server/torr/storage/torrstor/reader.go` | `Reader` wrapping `torrent.Reader`; window + readahead |
| `server/torr/storage/torrstor/ranges.go` | `Range`, `inRanges`, `mergeRange` |
| `server/torr/storage/state/state.go` | `CacheState`, `ItemState`, `ReaderState` JSON for UI |
| `server/settings/torrent.go` | `IsTorrentPinned` (`:126-131`) used by `Close` |
| `server/torr/storage/torrstor/race_test.go` | Race tests |
| `server/torr/storage/torrstor/cache_test.go` | Package `TestMain` (settings DB); pin/drop/eviction/priority/close tests on a real offline client |
| `server/torr/storage/torrstor/diskpiece_test.go` | Disk piece completeness on restart |
| `server/server.go:86-137` | Startup `cleanCache` of `TorrentsSavePath` |

## Flow

### Storage
1. `NewStorage` stores `capacity` and empty `caches` map (`storage.go:20-25`). Guarded by `Storage.mu` (`storage.go:17`).
2. `OpenTorrent` returns an existing cache for the hash if present (`storage.go:33-35`), else `NewCache` + `Init` and stores it (`:36-38`).
3. `CloseHash` deletes from map under lock, then calls `Cache.Close` outside the lock (`storage.go:47-59`). No caller outside `torrstor` besides the interface (`server/torr/storage/storage.go:11`).
4. `Storage.Close` snapshots and closes all caches (`storage.go:61-73`). `Cache.Close` itself removes its entry via `removeCache` (`storage.go:75-79`, `cache.go:266`).

### Cache init
1. `Init` (`cache.go:75`): if `capacity == 0` → `PieceLength*4` (`:77-79`).
2. `UseDisk` → `os.MkdirAll(TorrentsSavePath/<hashHex>)` (`cache.go:85-91`). Only place the hash dir is created; `DiskPiece.WriteAt` creates files, not the dir (`diskpiece.go:38`).
3. Creates a `Piece` for every index with its real length `info.Piece(i).Length()` (`cache.go:93-95`). `NewPiece(id, length, cache)` picks `MemPiece` if `!UseDisk`, else `DiskPiece` (`piece.go:25-37`); memory pieces ignore `length`.
4. Existing disk pieces load in `NewDiskPiece(p, length)`: `os.Stat` → `Size` = file size, `Complete = size == length`, `Accessed` = mtime (`diskpiece.go:23-32`). No hash check. The short last piece is `Complete` at its real length.
5. Starts `priorityWatchdog` goroutine (`cache.go:97`): every 5 s, exits when closed, skips if `torrent == nil`, calls `getRemPieces` when `GetUseReaders() > 0 || pin.Load() != nil` (`cache.go:110-123`, condition `:119`).

### Cache struct / locks (`cache.go:23-54`)
- `capacity`, `filled` (plain int64, written only in `getRemPieces` `:433`, read in `cleanPieces` `:373`, no lock), `hash`, `pieceLength`, `pieceCount`.
- `pieces` + `muPieces` RWMutex: map is immutable after `Init`; lock guards the reference nilled in `Close` (`:34-38`, `getPieces` `:235-239`).
- `readers` + `muReaders` RWMutex (`:40-41`); `readersSnapshot` copies under RLock (`:241-249`).
- `isRemove`, `isClosed` atomic.Bool; `muRemove` serializes `cleanPieces`, the `SetPin` compare-and-store and each `dropPieces` release (`:43-45`).
- `muPrio` serializes `clearPriority` and `setLoadPriority` (`:46-49`).
- `pin atomic.Pointer[pinSet]` (`:52-53`); `pinSet{want, drop []bool}` never mutated after `Store`, nil = unpinned (`:56-61`). Only pin state in `torrstor`; the keep-on-close decision reads the DB row instead.
- `Piece(m)` returns `PieceFake` for unknown index (`cache.go:251-256`); `PieceFake` read/write/mark return error `"fake"` (`piecefake.go:11-27`).

### Pin API (`cache.go`)
- `SetPin(want, drop)` (`:131-170`): nil-safe; `want == nil` → store nil. Under `muRemove` (waits for a running `cleanPieces` or piece release) load current; unchanged (both nil, or `slices.Equal` on `want` and `drop`) → return (`:141-147`); else store (`:148`). On change log `Set cache pin: <hash> pinned: <bool> wanted: <n> drop: <n>` (`:151-162`) and, if `torrent != nil && !isClosed`, `go { clearPriority(); dropPieces() }` — priorities lowered before files are removed (`:163-169`).
- `PinPending()` (`:173-183`): nil or unpinned → false; true if any piece with `isWanted(id)` has `Complete == false`. Closed cache (pieces nil) → false.
- `isKept(id)` (`:187-189`): `pin != nil` — every piece of a pinned torrent is exempt from eviction.
- `isWanted(id)` (`:192-195`): pin non-nil, `id < len(want) && want[id]` — kept downloading.
- `dropPieces()` (`:199-233`): closed, `torrent == nil` or unpinned → return (`:200-202`). Protected ranges = whole piece range `[offset/pl, (offset+length-1)/pl]` of the file of every registered reader (`readersSnapshot`, regardless of `isUse`, length > 0), snapshotted once (`:206-215`). Per piece of `getPieces()`: `muRemove.Lock`, reload current set, `p.Release()` when `!isClosed && drop[id] && !want[id] && !inRanges(protected, id) && (Size > 0 || Complete)`, unlock (`:218-229`). Log `Drop pinned pieces: <hash> <count>` when count > 0 (`:230-232`).
- Caller: `Torrent.applyPin` only (`server/torr/torrent.go:140-189`); set computed by `pinPieces` (`server/torr/pin.go:128-191`); a pin paused for lack of free space is pushed as a non-nil all-false `want` with the real `drop` (`server/torr/torrent.go:182-186`) — no torrstor code for it; see [Torrent pin](torrent-pin.md).

### Write / read / completion
- `Piece.WriteAt/ReadAt/Release` branch on `settings.BTsets.UseDisk` at call time (`piece.go:39-53,72-77`).
- `MarkComplete`/`MarkNotComplete` only flip `Complete`; `Completion()` returns it with `Ok: true` (`piece.go:55-70`). Not persisted; disk restart infers it from file size.
- `MemPiece.WriteAt`: first write allocates `pieceLength` buffer and fires `go cleanPieces()` (`mempiece.go:24-27`); `Size += n` clamped to `pieceLength`; `Accessed = now` (`:29-33`).
- `MemPiece.ReadAt`: sets `Accessed`; if read reaches piece end fires `go cleanPieces()` (`mempiece.go:52-55`).
- `DiskPiece.WriteAt`: open/create file (`O_RDWR|O_CREATE`, `diskpiece.go:38`), `WriteAt`, `Size += n` clamped to `pieceLength`, `Accessed` (`diskpiece.go:34-52`). **Does not call `cleanPieces`.**
- `DiskPiece.ReadAt`: missing file → `io.EOF` (`diskpiece.go:59-61`); read reaching piece end fires `go cleanPieces()` (`:71-73`).
- `Release`: mem → drop buffer, `Size=0`, `Complete=false` (`mempiece.go:62-70`); disk → `Size=0`, `Complete=false`, `os.Remove(file)` (`diskpiece.go:77-85`). Then `Piece.Release` sets anacrolix priority `None` and `UpdateCompletion` (`piece.go:79-80`). Callers: `removePiece` (eviction) and `dropPieces` (pin drop).

### Reader (`reader.go`)
1. `newReader` wraps `file.NewReader()`, readahead 0, `isUse=true`, registers in `cache.readers` (`reader.go:29-42`).
2. `Seek`/`Read` call `readerOn`, update `offset` and `lastAccess` (`reader.go:44-96`).
3. `SetReadahead` caps at `cache.capacity`; applied to anacrolix only if `isUse` (`reader.go:98-106`). `Cache.AdjustRA` sets every reader's readahead and, if `CacheSize == 0`, `capacity = readahead*3` (`cache.go:304-314`). Caller: `Torrent.updateRA` every progress tick with fixed 16 MB (`server/torr/torrent.go:287-288`).
4. Window: `getOffsetRange` = `offset - (capacity/useReaders)*(100-ReaderReadAHead)/100` .. `offset + (capacity/useReaders)*ReaderReadAHead/100`, clamped to `[0, file.Length]` (`reader.go:143-161`); `getPiecesRange` converts to torrent-global piece ids via `file.Offset()` (`reader.go:126-141`).
5. `checkReader`: idle >60 s and more than one reader → `readerOff` (readahead 0, `isUse=false`, seek to 0), else `readerOn` (`reader.go:163-193`).
6. `Cache.CloseReader`: remove from map, `Reader.Close` (fires `go getRemPieces`, `reader.go:123`), then `go clearPriority` (`cache.go:529-536`).

### Ranges (`ranges.go`)
- `Range{Start, End, File}`; `inRanges` is inclusive on both ends (`ranges.go:14-21`).
- `mergeRange` copies, sorts by Start/End, merges overlaps (`ranges.go:23-52`).

### Eviction
1. `cleanPieces` (`cache.go:358-384`): return if removing or closed; `muRemove.TryLock` else return (`:364`); calls `getRemPieces`; only if `filled > capacity` removes `(filled-capacity)/pieceLength + 1` pieces from the candidate list (oldest `Accessed` first) via `removePiece` (`:373-383`).
2. `removePiece` (`cache.go:297-302`): no-op if closed **or** `isKept(piece.Id)`; else `Piece.Release`.
3. `getRemPieces` (`cache.go:386-435`):
   - Ranges = merged `getPiecesRange` of readers with `isUse` after `checkReader` (`:387-397`).
   - Kept pieces (every piece of a pinned cache) skipped entirely: not candidates, `Size` not added to `fill` (`:405-407`).
   - With ranges: candidate = `Size > 0`, id not in any range, and not in a read file's begin/end protection zone (`:411-416`).
   - Without ranges: candidate = every piece with `Size > 0` of an unpinned cache (`isIdInFileBE` with empty ranges is always false) (`:417-422`, `:476-495`).
   - **Always calls `clearPriority()` then `setLoadPriority(ranges)`** (`:425-426`).
   - Sorts candidates by `Accessed` ascending (`:429-431`); sets `c.filled = fill` (`:433`) — unpinned bytes only.
4. `isIdInFileBE`: protects first and last `max(pieceLength, 8 MB)` of each file that has an active range (`cache.go:476-495`).
5. Callers of `cleanPieces`: `MemPiece.WriteAt` first alloc, `MemPiece.ReadAt` / `DiskPiece.ReadAt` at piece end (all `go`). Callers of `getRemPieces` alone: `cleanPieces`, `priorityWatchdog`, `Reader.Close`.

### Priorities
- `clearPriority` (`cache.go:538-586`): needs `torrent != nil`; under `muPrio`; builds merged reader ranges (`:548-555`). Per piece:
  - Wanted (`isWanted`, `:558-573`): `PieceState.Complete` → skip (`:562-564`); no ranges or id outside ranges → `Normal` if priority `!= Normal`; inside a range → `Normal` only if priority is `None` (never lowers Now/Next/Readahead/High).
  - Not wanted — unpinned cache, or pinned cache outside the window (`:574-584`): with ranges sets `None` outside ranges; with no active readers sets `None` on **all** such pieces.
- `setLoadPriority` (`cache.go:437-474`, unchanged by pin): returns if no readers or pieces nil; under `muPrio`; per reader with `isUse`, skipped when reader's piece is in a file begin/end zone (`:449-451`). From reader piece to range `End`, only incomplete pieces, at most `ConnectionsLimit/len(readers)` (`:455`): reader piece `Now`, +1 `Next`, up to readahead piece `Readahead`, next 5 `High`, beyond `Normal` (`:457-471`). Reader priorities therefore still win on a not wanted piece of a pinned cache.

### State for UI
- `GetState` (`cache.go:316-356`): `Pieces` only for `Size > 0` with `Size`, `Length`, `Completed`, `Priority` from `torrent.PieceState` (`:322-333`); `Readers` with `Start`/`End`/`Reader` piece; `Capacity`, `Filled` (sum of all non-empty pieces, pinned included, `:352`), `PiecesLength`, `PiecesCount`, `Hash`. Does **not** write `c.filled` (comment `:347`). Types in `server/torr/storage/state/state.go:7-30`.
- `GetUseReaders`, `Readers`, `GetCapacity` are nil-safe (`cache.go:505-527,588-593`). `Torrent.expired` uses `Readers() == 0` (`server/torr/torrent.go:299`).

### Close
1. `Cache.Close` (`cache.go:258-295`): `isClosed=true` (stops watchdog, `cleanPieces`, `removePiece`, `dropPieces` — also mid-pass — and `SetPin`'s async work) (`:264`), `storage.removeCache` (`:266`).
2. If `RemoveCacheOnDrop` (`:269`): `settings.IsTorrentPinned(c.hash)` → log `Keep pinned cache on close:` and keep everything (`:270-271`); else `os.Remove` each piece file then `os.Remove(TorrentsSavePath/<hash>)` (`:272-281`). Option checked first, so no DB read when it is off. Checks `UseDisk` only indirectly (`dPiece != nil`).
3. Nils `readers` and `pieces`, `FreeOSMemGC` (`cache.go:285-293`).
4. Called synchronously from anacrolix `Torrent.Drop()` under the client lock (`server/torr/torrent.go:348`; `closeWithPieceFiles` asserts `isClosed` right after `Drop`, `cache_test.go:751-762`).

### Startup cleanCache (`server/server.go`)
- Launched as `go cleanCache()` from `Start` (`server/server.go:72`), before `web.Start` connects and starts the resume loop (`:83`).
- Returns if `!UseDisk` or save path empty or `/` (`server/server.go:87-89`).
- Builds `pinned` hex set from `settings.ListTorrent()` rows with `PinMode != ""` (`:96-102`).
- Only 40-char entries considered (`:107-110`).
- `RemoveCacheOnDrop=false`: removes hash dirs not matching any torrent in the DB list (`:112-125`). Map named `keep` actually holds "delete" flags. Pinned and unpinned DB dirs both kept.
- `RemoveCacheOnDrop=true`: pinned hash → log `Keep pinned cache:` + skip (`:127-130`); every other hash dir removed with `Remove unused cache:` (`:131-134`).
- `removeAllFiles` is non-recursive: removes entries then the dir (`server/server.go:139-149`).

## Tests
- `cache_test.go` `TestMain` (`:25`): opens one settings DB in a temp dir (`settings.InitSets`), restores `Path/ReadOnly/SearchWA/BTsets` before `m.Run()` so `race_test.go` still sees its own `BTsets`; `CloseDB` after. Pattern from `server/torr/dbwrapper_test.go:19`.
- `race_test.go` (zeroed `settings.BTsets`, memory mode, `race_test.go:21-26`; `testInfo` `:12`):
  - `TestStorageCachesConcurrentOpenClose` — `OpenTorrent`/`GetCache`/`Cache.Close` vs `OpenTorrent`/`CloseHash` on `Storage.caches` (`race_test.go:30-65`).
  - `TestCachePiecesConcurrentStateClose` — `GetState` + `cleanPieces` vs `Close` nilling `pieces` (`race_test.go:68-93`).
- `cache_test.go` fixtures: `pinTestPieceLength = 16384` (`:20`); `withDiskSettings` (disk mode, `TorrentsSavePath=t.TempDir()`, `ConnectionsLimit 25`, `ReaderReadAHead 95`, restored in cleanup, `:43`); `pinAllSet` (`:56`, every piece wanted, no drop); `boolSet` (`:65`); `newPinTestCache(t, numPieces, capacity, pinned)` (`:76`) → `newPinSetTestCache(t, numPieces, capacity, want, drop)` (`:88`) → `newPinFilesTestCache(t, filePieces, capacity, want, drop)` (`:95`, consecutive files of N full pieces each; single-file when one entry) — spec with info bytes (zero hashes) on a real offline anacrolix client (NoDHT, no trackers/TCP/uTP/LSD/port forwarding) whose `DefaultStorage` is a `torrstor.Storage`; pin set before `SetTorrent`, so no async `clearPriority`/`dropPieces` runs. Helpers `fillPiece` `:151`, `pieceFileExists` `:160`, `completePiece` `:170`, `assertPriority` `:181`, `waitPriority` (deadline poll) `:189`, `assertReleased` `:534` (no file, `Size 0`, not complete in cache and anacrolix), `assertKept` `:541`, `runBlockedDrop` `:611` (starts `dropPieces` under a held `muRemove`, waits via goroutine stack until it blocks, applies a change, releases).
  - Eviction: `TestCleanPiecesEvictsOldestUnpinned` (`:212`), `TestCleanPiecesKeepsPinned` (`:231`), `TestGetStateFilledIncludesPinned` (`:253`), `TestRemovePiecePinnedIsNoop` (`:269`), `TestRemovePieceUnpinnedReleases` (`:280`), `TestCleanPiecesKeepsNotWantedOfPinned` (`:499`, `isKept` covers not wanted pieces, `cleanPieces` + `removePiece`)
  - Priorities: `TestClearPriorityUnpinnedNoReaders` (`:291`), `TestClearPriorityPinnedNoReaders` (`:301`), `TestClearPriorityPinnedWithReader` (`:318`), `TestClearPriorityUnpinnedWithReader` (`:342`), `TestClearPriorityNotWantedOfPinned` (`:520`, not wanted → `None`)
  - `SetPin`: `TestSetPinRearmsPriorities` (`:356`), `TestSetPinUnchangedKeepsSet` (`:376`, equal set not re-stored; changed `drop` or `want` stored), `TestSetPinWithoutTorrent` (`:396`, nil cache + cache without torrent), `TestSetPinWaitsForRunningEviction` (`:416`), `TestSetPinDropsAsync` (`:669`, deadline poll), `TestSetPinUnpinRemovesNothing` (`:691`), `TestSetPinConcurrent` (`:704`, `-race` toggles all/window/nil vs `clearPriority`/`GetState`/`cleanPieces`/`PinPending`/`dropPieces`)
  - `PinPending`: `TestPinPending` (`:447`), `TestPinPendingCountsOnlyWanted` (`:479`)
  - `dropPieces` (synchronous): `TestDropPieces` (`:548`, drop released incl. complete; drop+want, want, neither kept; no file created for drop without data), `TestDropPiecesKeepsReaderFile` (`:569`, files 4/4/2 pieces, reader at start of file 2 with window ending before piece 7 → whole file 2 kept, files 1 and 3 released), `TestDropPiecesUnpinnedOrClosed` (`:593`), `TestDropPiecesReloadsCurrentSet` (`:641`), `TestDropPiecesStopsWhenClosed` (`:656`)
  - Close helpers: `withTorrentRow` (`:742`, row + pin, removed in cleanup), `closeWithPieceFiles` (`:751`, two piece files, `tor.Drop()`, asserts closed), `assertPathsExist` (`:764`).
  - `TestCloseRemoveCacheOnDropKeepsPinned` (`:777`, `all` and `next`), `TestCloseRemoveCacheOnDropRemovesUnpinned` (`:790`, unpinned row and no row), `TestCloseWithoutRemoveCacheOnDropKeepsFiles` (`:808`)
  - Paused pin (Phase 5): `TestPausedPinSetRaisesNothing` (`:818`, all-false `want` + `drop` 7: `clearPriority` with a reader keeps the reader's `Now` and sets the rest `None`; `getRemPieces` with and without a reader, reader-close path run synchronously → all `None`; `PinPending` false; over-capacity `cleanPieces` keeps every piece; `dropPieces` still releases piece 7)
- `diskpiece_test.go` — `openWithPieceFiles` (`:16`, 3 pieces of 16 KiB, last 8 KiB, pre-created files): `TestNewDiskPieceFullFilesComplete` (`:52`), `TestNewDiskPieceShortFilesIncomplete` (`:61`).
- `cleanCache` tests: `server/server_test.go` — [Torrent pin](torrent-pin.md#tests). Window/anchor end-to-end tests on a loaded torrent: `server/torr/pin_test.go` — [Torrent pin](torrent-pin.md#tests).
- Not tested: watchdog `|| pin != nil` condition (needs >5 s wait; watchdog reads `c.torrent` unsynchronised) and `SetPin` nil→nil branch. Live smoke is the evidence — [Torrent pin](torrent-pin.md). CI gate: [CI test gate](../../concepts/ci-test-gate.md).

## Dependencies
- Overview: [server](_overview.md)
- Features: [Torrent lifecycle](torrent-lifecycle.md), [Torrent pin](torrent-pin.md), [Streaming](streaming.md), [Preload](preload.md), [Torrent API helpers](torrent-api-helpers.md)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md), [CI test gate](../../concepts/ci-test-gate.md)
- UI consumer: [Torrent details dialog](../web/torrent-details-dialog.md)

## Gotchas / decisions
- **Priority reset:** every `getRemPieces` call (piece reads, 5 s watchdog with readers or pin, `Reader.Close`) runs `clearPriority` (`cache.go:425`). Not wanted pieces get `None` outside reader ranges, or all of them when no reader is active (`cache.go:574-584`); `CloseReader` also fires `clearPriority` (`cache.go:535`). Any other externally set priority on a not wanted piece is wiped. Wanted incomplete pieces are raised to `Normal` instead (`:558-573`); reader-window priorities are re-applied by `setLoadPriority` right after (`:426`).
- **Pin state = immutable set behind an atomic pointer** (Phase 4, replaces the Phase 2 `pinAll` flag). Hot eviction/priority paths read it lock-free; `SetPin` stores a new set, never mutates. Still a copy pushed by `torr` (`torrstor` cannot import `torr`).
- **Kept vs wanted split.** `isKept` (`pin != nil`) = no eviction for any piece of a pinned torrent; `isWanted` (`want[id]`) = download priority + `PinPending`. Data outside the `next` window survives because eviction never runs for a pinned torrent; only `dropPieces`, `rem` and `off` delete (curator-delegated, [Torrent pin](torrent-pin.md#gotchas--decisions)).
- **Kept pieces are never evicted.** Three guards: skipped in `getRemPieces` candidates (`:405-407`), excluded from `c.filled` so they do not push unpinned pieces out (`:408-410`, `:433`), and `removePiece` re-checks (`:298`) to close the window where the pin is set between candidate selection and release.
- **`SetPin` compares and stores under `muRemove`** (`:141-149`): waits for a running `cleanPieces` (which holds `muRemove` for the whole pass), so a piece selected while unpinned cannot be released after `SetPin` returns (Codex audit found a re-download window otherwise). Cost: a `pin` request, `WaitInfo` or anchor save that changes the set may wait one eviction pass. Lock order: `muTorrent` → `muRemove` → `muPrio`/anacrolix client lock; `torrstor` never takes `muTorrent`; anacrolix storage callbacks only spawn `cleanPieces` with `go`.
- **Unchanged set → no effect** (`slices.Equal`, `:143-147`): no log, no re-arm, no drop pass. `WaitInfo`/`copyPin` re-applies are therefore cheap.
- **`dropPieces` takes `muRemove` per piece** (`:221-228`), so `SetPin` (under `muTorrent`, which `Status()`/`Stream` need) waits at most one release. The set is re-loaded per piece, so a piece is dropped only when the current set drops it (`TestDropPiecesReloadsCurrentSet`); `isClosed` re-checked per piece (`TestDropPiecesStopsWhenClosed`).
- **Whole-file reader protection (amended after Phase 4 5e):** every piece of a file with a registered reader stays, not only the reader window — a reader advancing/seeking during the pass or a second viewer on an earlier episode would otherwise lose data. `isUse` is ignored (an idle reader still protects its file). Reader set snapshotted once per pass; a reader created during the pass is not protected (accepted).
- **Drop order: priorities first, then files** (`:165-168`), so anacrolix is not asked to keep downloading pieces being removed. An in-flight write may recreate a partial file (priority `None`, not continued); it stays until the next drop.
- **Accepted (Phase 4 5a):** `Release` racing an in-flight anacrolix hash check can leave `Complete=true` without a file (same as `removePiece`); `dropPieces` reads `Piece.Size/Complete` unlocked (pattern of `GetState`/`PinPending`).
- **`Close` keep check reads the DB row, not the pin set** (Phase 3): the row survives drop and covers every mode; the set is runtime-only and gone after close. New lock edge: anacrolix client lock (and `muTorrent` from `drop()`) → `settings.mu` in `IsTorrentPinned`; no `settings` code takes the client lock, so no cycle. After `settings.CloseDB` the row read returns nil (`server/settings/dbreadcache.go:34`) → treated as unpinned.
- **Pin change re-arms via `go clearPriority`, not `go getRemPieces`** (`:166`): `getRemPieces` writes unsynchronised `c.filled` and reads `Piece.Size`; an async call made that pre-existing race fail `-race`. `clearPriority` carries the whole pin rule and writes no counters. Watchdog `getRemPieces` every 5 s is the steady re-arm (no new loop).
- **Complete wanted pieces are skipped in `clearPriority`** (`:562-564`): anacrolix reports `Priority None` for complete pieces (`tsynik/torrent piece.go:217`, per plan), so without the skip every watchdog tick would `SetPriority` every complete piece.
- **`cState.Filled` keeps pinned bytes; `c.filled` does not.** UI/tgbot/`PreloadedBytes` of unpinned torrents unchanged; pinned torrents show real disk usage (`:324`, `:352`).
- **`GetState` no longer writes `c.filled`.** Only `cleanPieces` reads it, right after `getRemPieces` recomputes it.
- **Paused pin = non-nil all-false `want` (Phase 5):** `isKept` still true (no eviction), `isWanted` false everywhere (no `Normal` raise via `SetPin`, watchdog or `CloseReader`), `PinPending` false (normal expiry), `drop` still honoured. Chosen over `SetPin(nil, nil)`, which would re-enable eviction and stop the anchor drop. The watchdog keeps running every 5 s (`pin != nil`).
- **Unpin in the cache does not delete or force eviction.** `SetPin(nil, nil)` → priorities fall back via async `clearPriority`, `dropPieces` returns (unpinned); dir deletion is done by `torr` after close ([Torrent pin](torrent-pin.md)).
- **Never delete the hash dir under a loaded cache:** `Init` is the only `MkdirAll`; `DiskPiece.WriteAt` with `O_CREATE` fails once the dir is gone (`diskpiece.go:38`). `dropPieces` removes piece files only.
- **Not fixed (cosmetic):** `PinPending` reads `Piece.Complete` unsynchronised (`:178`), like `setLoadPriority`/`GetState`. Premature expiry needs every other wanted piece complete and the last one flipped back inside one read; accepted.
- **Eviction is capacity-bounded, not window-bounded:** removes only `(filled-capacity)/pieceLength+1` oldest candidates, and only when `filled > capacity` (`cache.go:373-383`). With no active reader every non-empty piece of an unpinned cache is a candidate (`cache.go:417-422`).
- **Disk mode evicts only on reads:** `DiskPiece.WriteAt` never triggers `cleanPieces` (`diskpiece.go:34-52`); only `ReadAt` at piece end does (`diskpiece.go:71-73`). Background download without reads does not evict, but the next read can evict unpinned pieces down to capacity.
- **Release deletes the piece file** (`diskpiece.go:84`) and sets priority `None` (`piece.go:79`).
- **`Close` with `RemoveCacheOnDrop` deletes piece files and the hash dir** unless the DB row is pinned (`cache.go:268-283`); startup `cleanCache` with `RemoveCacheOnDrop` deletes all unpinned hash dirs, without it deletes dirs not in DB (`server/server.go:112-135`).
- **`diskpiece.go` trusts file size on restart:** `Complete = size == length` (`diskpiece.go:28`), no hash verification. Length is the real piece length passed from `Init` (`cache.go:94`), so the short last piece is `Complete` when its file is full; a shorter file is not.
- `UseDisk` is read on every piece call (`piece.go:40,48,73`); toggling it while a cache is open would dispatch to a nil `mPiece`/`dPiece` (inferred from `piece.go:31-35`).
- `capacity` default: `CacheSize` from settings; `0` → `PieceLength*4` at init, then `16MB*3` via `AdjustRA` every tick (`cache.go:77-79`, `:308-310`, `server/torr/torrent.go:287-288`).
- Reader window is split by number of active readers (`reader.go:145-151`); idle readers are switched off only when there are ≥2 readers (`reader.go:164`).
- `setLoadPriority` skips readers positioned in file begin/end zones (`cache.go:449-451`), so preload of head/tail relies on anacrolix reader readahead.
- `GetState` dereferences `c.torrent` for any piece with `Size > 0` (`cache.go:330`); a disk cache with existing pieces must have `SetTorrent` called first. Current callers set it together (`server/torr/torrent.go:126-127`). `SetPin` on a cache without a torrent only stores the set (`cache.go:163`).
- `CloseHash` must not be called under `Storage.mu` (`storage.go:54-55`); `Cache.Close` re-enters `removeCache`.

## Open questions
- Whether anacrolix `Client.Close()` calls `Cache.Close` (module source `github.com/tsynik/torrent v1.2.31`, `server/go.mod:6`, not available locally). `Torrent.Drop()` does, synchronously (test + Phase 3 live smoke: settings save logged `Keep pinned cache on close:`).
- Whether anacrolix re-verifies pieces reported `Complete` by `Completion()` after restart.
- `c.filled` / `Piece.Size` race (`cache.go:373`, `:433`) is real: `-race` flagged it once `getRemPieces` ran from a second goroutine (Phase 2 plan). Pre-existing, out of scope, no demonstrated defect.
