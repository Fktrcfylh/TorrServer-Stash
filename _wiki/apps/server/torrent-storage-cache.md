# Torrent storage cache (torrstor)

**App:** server
**Status:** stable
**Last reviewed:** 2026-10-26 (commit `e2d242e0`; pin set removed in `763a4204`)

## Summary
Custom anacrolix `storage.ClientImpl`: one `Cache` per infohash holding all pieces in RAM (`MemPiece`) or as one file per piece on disk (`DiskPiece`). Cache size is bounded by `capacity`; eviction drops complete, oldest-accessed pieces outside active reader windows first. The cache also drives anacrolix piece priorities (Now/Next/Readahead/High/Normal) around each reader, and exposes piece/reader state for the web cache map. The NAS predownload does not touch this layer: the NAS is just another peer ([Remote download](remote-download.md)).

## Entry points
- `server/torr/btserver.go:99-100` — `torrstor.NewStorage(settings.BTsets.CacheSize)`, set as anacrolix `DefaultStorage`.
- `server/torr/storage/torrstor/storage.go:27` — `OpenTorrent`, called by anacrolix per torrent.
- `server/torr/torrent.go:160-170` — `WaitInfo` snapshots `bt.storage`, `GetCache`, `SetTorrent`.
- `server/torr/torrent.go:340-351` — `Torrent.NewReader` / `CloseReader` wrap `Cache.NewReader` / `Cache.CloseReader`; used by `server/torr/stream.go:91`, `:99`, `server/torrfs/torrfile.go:46`, `:83`, `server/tgbot/upload/torrfile.go:68`.
- `server/torr/torrent.go:270-272` — `progressEvent` reads `GetState().Filled` into `PreloadedBytes`.
- `server/torr/torrent.go:486-493` — `CacheState()` for `POST /cache {action:get}` (`server/web/api/cache.go:47-55`).
- `server/torr/torrent.go:361` — `Torrent.Drop()` in `drop()` closes the storage synchronously → `Cache.Close`.

## Key files
| File | Role |
|---|---|
| `server/torr/storage/storage.go` | Interface: anacrolix `ClientImpl` + `CloseHash` (`:8-12`) |
| `server/torr/storage/torrstor/storage.go` | `Storage`: hash→`*Cache` map, open/close |
| `server/torr/storage/torrstor/cache.go` | `Cache`: pieces, readers, eviction, priorities, state, close |
| `server/torr/storage/torrstor/piece.go` | `Piece`: dispatch to mem/disk by `UseDisk`, completion flag |
| `server/torr/storage/torrstor/mempiece.go` | RAM buffer per piece |
| `server/torr/storage/torrstor/diskpiece.go` | File per piece `TorrentsSavePath/<hash>/<id>` |
| `server/torr/storage/torrstor/piecefake.go` | Stub returned for unknown piece index |
| `server/torr/storage/torrstor/reader.go` | `Reader` wrapping `torrent.Reader`; window, readahead, park/resume |
| `server/torr/storage/torrstor/ranges.go` | `Range`, `inRanges` (`:14-21`), `mergeRange` |
| `server/torr/storage/state/state.go` | `CacheState`, `ItemState`, `ReaderState` JSON for UI (`:7-30`) |
| `server/server.go:92-133` | Startup `cleanCache` of `TorrentsSavePath` |

## Flow

### Storage
1. `NewStorage` stores `capacity` and an empty `caches` map (`storage.go:20-25`), guarded by `Storage.mu`.
2. `OpenTorrent` returns an existing cache for the hash, else `NewCache` + `Init` and stores it (`storage.go:27-45`).
3. `CloseHash` deletes under lock, then `Cache.Close` outside it (`storage.go:47-59`). No caller in `server/` outside the interface.
4. `Storage.Close` snapshots and closes all caches (`storage.go:61-73`). `Cache.Close` removes its own entry via `removeCache` (`storage.go:75-79`, `cache.go:151`).

### Cache init (`cache.go:64-87`)
1. `capacity == 0` → `PieceLength*4` (`:66-68`).
2. `UseDisk` → `os.MkdirAll(TorrentsSavePath/<hashHex>)` (`:74-80`) — only place the hash dir is created.
3. A `Piece` for every index with its real length: `NewPiece(i, info.Piece(i).Length(), c)` (`:82-84`); `MemPiece` if `!UseDisk`, else `DiskPiece` (`piece.go:25-37`).
4. Existing disk pieces: `os.Stat` → `Size` = file size, `Complete = size == length`, `Accessed` = mtime (`diskpiece.go:23-32`). No hash check; short last piece is complete at its real length.
5. `go priorityWatchdog()` (`:86`): every 5 s, exits when closed, skips while `torrent == nil`, `getRemPieces` when `GetUseReaders() > 0` (`:98-111`).

### Cache struct / locks (`cache.go:22-50`)
- `capacity`, `filled` (plain int64: written in `getRemPieces` `:313` and `GetState` `:226`, read in `cleanPieces` `:252`, no lock), `hash`, `pieceLength`, `pieceCount`.
- `pieces` + `muPieces`: map immutable after `Init`; lock guards the reference nilled in `Close` (`getPieces` `:120-124`).
- `readers` + `muReaders`; `readersSnapshot` copies under RLock (`:126-134`).
- `isRemove`, `isClosed` atomic; `muRemove` serializes `cleanPieces` (`TryLock`, `:243`); `muPrio` serializes `clearPriority`/`setLoadPriority` (`:45-48`).
- `Piece(m)` returns `PieceFake` for an unknown index (`:136-141`).

### Write / read / completion
- `Piece.WriteAt/ReadAt/Release` branch on `settings.BTsets.UseDisk` at call time (`piece.go:39-53`, `:72-77`).
- `MarkComplete`/`MarkNotComplete` only flip `Complete`; `Completion()` returns it with `Ok: true` (`piece.go:55-70`). Not persisted; disk restart infers it from file size.
- `MemPiece.WriteAt`: first write allocates `pieceLength` and fires `go cleanPieces()` (`mempiece.go:24-27`); `ReadAt` reaching piece end fires it too (`:53-55`).
- `DiskPiece.WriteAt` opens with `O_RDWR|O_CREATE` (`diskpiece.go:38`) and **does not** call `cleanPieces`; `ReadAt`: missing file → `io.EOF`, read reaching piece end → `go cleanPieces()` (`:54-75`).
- `Release`: mem drops buffer; disk `os.Remove(file)`; both `Size=0`, `Complete=false` (`mempiece.go:62-70`, `diskpiece.go:77-85`); then priority `None` + `UpdateCompletion` (`piece.go:79-80`). Caller: `removePiece` (`cache.go:177-181`, no-op when closed).

### Reader (`reader.go`)
1. `newReader` wraps `file.NewReader()`, readahead 0, `isUse=true`, registers in `cache.readers` (`:32-45`).
2. `Seek`/`Read` bracket the anacrolix call with `beginIO`/`endIO` (`inFlight` under `mu`, resumes a parked reader) and update `offset`, `lastAccess` (`:47-101`, `:178-189`).
3. `SetReadahead` caps at `cache.capacity`, applied only if `isUse` (`:103-111`). `Cache.AdjustRA` sets every reader's readahead and, if `CacheSize == 0`, `capacity = readahead*3` (`cache.go:183-193`); caller `updateRA` every tick with 16 MB.
4. Window `getOffsetRange` = `offset - (capacity/useReaders)*(100-ReaderReadAHead)/100` .. `offset + (capacity/useReaders)*ReaderReadAHead/100`, clamped (`:148-166`); `getPiecesRange` → torrent-global piece ids (`:131-146`).
5. `checkReader`: idle >60 s and more than one reader → `readerOff`, else `readerOn` (`:168-174`). `readerOff` skips a reader with I/O in flight (upstream fix: parking seeks the anacrolix reader to 0 under a running read, `:208-223`, `TestReaderOffDoesNotParkAReaderWithIOInFlight` `reader_park_test.go:32`).
6. `Cache.CloseReader`: remove from map, `Reader.Close` outside `muReaders` (fires `go getRemPieces`, `reader.go:121-129`), then `go clearPriority` (`cache.go:409-416`).

### Eviction
1. `cleanPieces` (`cache.go:237-263`): return if removing or closed; `muRemove.TryLock` else return; `getRemPieces`; only if `filled > capacity` removes `(filled-capacity)/pieceLength + 1` candidates via `removePiece`.
2. `getRemPieces` (`:265-315`):
   - Ranges = merged `getPiecesRange` of readers with `isUse` after `checkReader` (`:266-276`).
   - With ranges: candidate = `Size > 0`, outside every range and outside a read file's begin/end zone (`:286-291`). Without ranges: every piece with `Size > 0` (`isIdInFileBE` with no ranges is false, `:292-297`).
   - **Always `clearPriority()` then `setLoadPriority(ranges)`** (`:300-301`).
   - Sort: complete pieces first, then oldest `Accessed` (`:306-311`) — upstream fix: evicting a downloading piece's received chunks makes a returning responsive reader read zeros (anacrolix never re-requests them; `TestGetRemPiecesEvictsDownloadingPiecesLast` `eviction_test.go:9`).
   - `c.filled = fill` (`:313`).
3. `isIdInFileBE`: protects first and last `max(pieceLength, 8 MB)` of each file with an active range (`:356-375`).

### Priorities
- `clearPriority` (`cache.go:418-450`): needs `torrent != nil`; under `muPrio`; with ranges sets `None` on pieces outside them; with no active reader sets `None` on **all** pieces.
- `setLoadPriority` (`:317-354`): no readers/pieces → return; under `muPrio`; per `isUse` reader not in a file begin/end zone, from reader piece to range `End`, only incomplete pieces, at most `ConnectionsLimit/len(readers)`: reader piece `Now`, +1 `Next`, up to readahead piece `Readahead`, next 5 `High`, beyond `Normal`.

### State for UI
- `GetState` (`cache.go:195-235`): `Pieces` for `Size > 0` with `Size`, `Length`, `Completed`, `Priority` from `torrent.PieceState`; `Readers` with `Start`/`End`/`Reader`; `Capacity`, `Filled`, `PiecesLength`, `PiecesCount`, `Hash`. **Writes `c.filled`** (`:226`).
- `GetUseReaders`, `Readers`, `GetCapacity` are nil-safe (`:385-407`, `:452-457`).

### Close (`cache.go:143-175`)
1. `isClosed=true` (stops watchdog, `cleanPieces`, `removePiece`), `storage.removeCache`.
2. `RemoveCacheOnDrop` → `os.Remove` each disk piece file, then the hash dir (`:153-163`); otherwise files stay.
3. Nils `readers` and `pieces`, `FreeOSMemGC`.
4. Called synchronously from anacrolix `Torrent.Drop()`.

### Startup cleanCache (`server/server.go:92-133`)
- `go cleanCache()` from `Start` (`:78`), before `web.Start` connects.
- Returns if `!UseDisk` or save path empty or `/` (`:93-95`); only 40-char entries (`:107-110`).
- `RemoveCacheOnDrop=false`: removes hash dirs matching no DB row (`:112-125`; map `keep` actually holds "delete" flags).
- `RemoveCacheOnDrop=true`: removes every hash dir (`:126-131`).
- `removeAllFiles` is non-recursive: entries then the dir (`:135-145`).

## Tests
- `cache_test.go`: `TestMain` (`:20`, one settings DB, restores `Path/ReadOnly/SearchWA/BTsets` right after `InitSets` so `race_test.go` keeps its zeroed `BTsets`); fixtures `testPieceLength = 16384` (`:15`), `withDiskSettings` (`:38`), `newTestCache(t, numPieces, capacity)` (`:52`, real offline client, `torrstor.Storage` as `DefaultStorage`), `fillPiece` (`:97`), `pieceFileExists` (`:106`), `completePiece` (`:116`), `assertPriority` (`:127`), `closeWithPieceFiles` (`:190`), `assertPathsExist` (`:203`). Tests `:134-231` (evict oldest, release, clear priority with/without reader, close with/without `RemoveCacheOnDrop`, `GetState` filled).
- `race_test.go`: `TestStorageCachesConcurrentOpenClose` (`:30`), `TestCachePiecesConcurrentStateClose` (`:68`).
- `diskpiece_test.go`: `TestNewDiskPieceFullFilesComplete` (`:52`), `TestNewDiskPieceShortFilesIncomplete` (`:61`).
- `eviction_test.go:9`, `reader_park_test.go:32` — see above. `cleanCache`: `server/server_test.go:95-117`.

## Dependencies
- Overview: [server](_overview.md)
- Features: [Torrent lifecycle](torrent-lifecycle.md), [Streaming](streaming.md), [Preload](preload.md), [Torrent API helpers](torrent-api-helpers.md)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md), [CI test gate](../../concepts/ci-test-gate.md)
- UI consumer: [Torrent details dialog](../web/torrent-details-dialog.md)

## Gotchas / decisions
- **Priority reset:** every `getRemPieces` (piece reads, 5 s watchdog with readers, `Reader.Close`) runs `clearPriority`; with no active reader every piece goes to `None`. Any externally set priority is wiped; reader priorities are re-applied by `setLoadPriority` right after.
- **Eviction is capacity-bounded, not window-bounded:** removes only `(filled-capacity)/pieceLength+1` candidates, and only when `filled > capacity`.
- **Disk mode evicts only on reads:** `DiskPiece.WriteAt` never triggers `cleanPieces`.
- **Release deletes the piece file** and sets priority `None`.
- **`Close` with `RemoveCacheOnDrop` deletes all piece files and the hash dir** (no keep exceptions since the pin removal); startup `cleanCache` likewise.
- **Never delete the hash dir under a loaded cache:** `Init` is the only `MkdirAll`; `DiskPiece.WriteAt` fails once the dir is gone.
- **`diskpiece.go` trusts file size on restart** (`Complete = size == length`), no hash verification.
- `UseDisk` is read on every piece call; toggling it while a cache is open would dispatch to a nil `mPiece`/`dPiece` (inferred from `piece.go:31-35`). Settings changes reload the client anyway.
- `capacity`: `CacheSize`; `0` → `PieceLength*4` at init, then `16MB*3` via `AdjustRA` every tick.
- `setLoadPriority` skips readers positioned in file begin/end zones, so preload of head/tail relies on anacrolix readahead.
- `GetState` dereferences `c.torrent` for any piece with `Size > 0` (`:209`); `SetTorrent` must run first (callers do it in `WaitInfo`).
- `CloseHash` must not be called under `Storage.mu` (`storage.go:54-55`).

## Open questions
- Whether anacrolix `Client.Close()` calls `Cache.Close` (fork source in module cache, unchecked). `Torrent.Drop()` does, synchronously.
- Whether anacrolix re-verifies pieces reported `Complete` by `Completion()` after restart.
- Unsynchronised `c.filled` (`cache.go:226`, `:252`, `:313`), `c.capacity` (`AdjustRA` writes it, `:187-189`; `GetState` reads it) and `Piece.Size` — pre-existing; `-race` reported `GetState` vs `AdjustRA` when a real watch loop ticked in NAS Phase 4 tests. No demonstrated defect.
