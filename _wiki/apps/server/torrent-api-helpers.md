# Torrent API helpers

**App:** server
**Status:** stable
**Last reviewed:** 2026-09-14 (commit `215f6fca` + staged pin Phases 1–5)

## Summary
Package-level functions in `server/torr/apihelper.go` operate on the global `bts *BTServer` (`server/torr/apihelper.go:19`); REST, tgbot, MCP, DLNA and torrfs call them. `dbwrapper.go` converts between in-memory `Torrent` and persisted `settings.TorrentDB`. `POST /torrents` dispatches `action` to these helpers.

## Entry points
- `server/web/api/route.go:25` — `authorized.POST("/torrents", torrents)`
- `server/web/api/torrents.go:50` — `torrents` handler, `switch req.Action` (`:57-94`)
- `server/torr/apihelper.go:21` — `InitApiHelper` sets global `bts`; called from `Connect` (`server/torr/btserver.go:71`)
- `server/web/api/settings.go:49`, `:63` — `SetSettings` / `SetDefSettings` callers

## Key files
| File | Role |
|---|---|
| `server/torr/apihelper.go` | Load/add/get/set/pin/rem/drop/list, cache dir removal, settings reload, shutdown, preload sizing |
| `server/torr/resume.go` | `resumePinned` called by settings reload (loads unfinished pins, re-applies loaded ones) — see [Torrent pin](torrent-pin.md) |
| `server/torr/dbwrapper.go` | `Torrent` <-> `TorrentDB` field copy |
| `server/settings/torrent.go` | `TorrentDB` model; `AddTorrent`/`SetTorrentPin`/`SetTorrentPinAnchor`/`SetTorrentPinDownloaded`/`IsTorrentPinned`/`ListTorrent`/`RemTorrent` on `tdb` bucket `"Torrents"` |
| `server/web/api/torrents.go` | REST action dispatch, `torrReqJS` |
| `server/torr/utils/webImageChecker.go` | `CheckImgUrl`: HTTP GET of poster, 5 s timeout (`:19-37`) |
| `server/torr/utils/torrent.go` | `InvalidateTrackersCache` (`:101`) used on settings change |
| `server/web/api/torrents_test.go` | `TestTorrentsRejectionsCarryJSONBody` (`:41`), pin tests — see [Torrent pin](torrent-pin.md), [CI test gate](../../concepts/ci-test-gate.md) |
| `server/torr/apihelper_test.go` | Pin, `RemTorrent`/`off` dir + no-space tests — see [Torrent pin](torrent-pin.md#tests) |
| `server/torr/dbwrapper_test.go` | DB copy pin tests, status pin JSON, `AddTorrentDB` layout-only `Data` — see [Torrent pin](torrent-pin.md#tests) |

## apihelper.go
| Func | Lines | Behaviour |
|---|---|---|
| `LoadTorrent(tor)` | `:25-41` | nil if no spec; `NewTorrent` + `WaitInfo` (not `GotInfo`); copies only `Title`, `Poster`, `Data` (`:36-38`) + pin via `copyPin` (`:39`). Callers: `server/web/api/m3u.go:110`, `server/mcp/helpers.go:66`, `:82` |
| `AddTorrent(spec,title,poster,data,category)` | `:43-89` | `NewTorrent` (returns existing if loaded); for each of Title/Category/Poster/Data only if the torrent field is empty: request arg, else DB value (`:50-81`); Title final fallback `Info().Name` (`:57-59`). DB row exists → all 4 pin fields from DB (`:84-86`). Does not save to DB |
| `copyPin(dst,src)` | `:92-100` | 4 pin fields under `dst.muTorrent`; `PinDownloaded` cloned; then `dst.applyPin()` after unlock (`:99`). See [Torrent pin](torrent-pin.md) |
| `SaveTorrentToDB` | `:102-105` | log + `AddTorrentDB` |
| `GetTorrent(hashHex)` | `:107-138` | Loaded → extend expiry (capped 1 min) and return (`:113-117`). Else DB copy (`Stat=InDB`) returned immediately while a goroutine `NewTorrent` + copies Title/Poster/Data/Size/Timestamp/Category (`:126-131`) + `copyPin` (`:132`) + `GotInfo()` (`:122-135`). Also the resume load path |
| `SetTorrent` | `:140-179` | Updates loaded torrent fields and DB record (`AddTorrentDB(torrDb)` `:172`); `data` only when non-empty (`:160-162`, `:169-171`) |
| `SetTorrentPin(hashHex,mode,next)` | `:183-230` | ReadOnly → nil; `off` → `""`/0; `settings.SetTorrentPin` on the row (off also clears row `PinDownloaded`); `off` → `pinNoSpaceSet(hash, false)` loaded or not (`:198-200`); `wasPinned` = row pinned before the write (`:196`) or loaded mirror pinned (`:211`); not loaded + `off` + row + `wasPinned` → `removeTorrentDir` (`:205-207`); loaded → mirror (off: + `PinDownloaded = nil`, `:214-216`) + `applyPin()` (`:218`), no row → `AddTorrentDB` + pin again, `off` + `wasPinned` → `go removeTorrentDirAfterClose` (`:226-228`); never starts a load. See [Torrent pin](torrent-pin.md) |
| `removeTorrentDir(hashHex)` | `:234-242` | `UseDisk && hashHex != "" && != "/"` and `TorrentsSavePath/<hash>` exists → log `Removing cache files for:` + `os.RemoveAll`. Callers: `RemTorrent` (both branches), `SetTorrentPin(off)` DB-only previously pinned. Must not run while a loaded cache uses the dir |
| `removeTorrentDirAfterClose(torr,hash,name)` | `:246-268` | Wait `closed`, `muTorrent` barrier + mirror check, identity re-check in `torr.bt`, `IsTorrentPinned` re-check, then `RemoveAll`. See [Torrent pin](torrent-pin.md) |
| `RemTorrent(hashHex)` | `:270-307` | See Flow |
| `ListTorrent()` | `:309-333` | `bts.ListTorrents()` + DB copies for hashes not loaded (`:313-317`); sort `Timestamp` desc then `Title` desc (`:324-330`) |
| `DropTorrent` | `:335-338` | `bts.RemoveTorrent(hash)` → `Close` only; DB untouched |
| `SetSettings(set)` | `:340-357` | ReadOnly guard; `SetBTSets`; `InvalidateTrackersCache`; `dropAllTorrent`; sleep 1 s; `Disconnect`; `Connect`; `resumePinned()` (`:354`); sleep 1 s |
| `SetDefSettings()` | `:359-376` | Same with `SetDefaultConfig`; `resumePinned()` at `:373` |
| `dropAllTorrent` | `:378-383` | For each loaded torrent: `drop()` then block on `<-torr.closed` |
| `Shutdown` | `:385-397` | Embedded → `EmbeddedStop`; else `Disconnect`, `CloseDB`, `os.Exit(0)` |
| `WriteStatus` | `:399-401` | anacrolix client status dump |
| `Preload(torr,index)` | `:403-414` | Size calc, see [Preload](preload.md) |

## dbwrapper.go — field copy
| Direction | Func | Copied | Notes |
|---|---|---|---|
| Torrent → DB | `AddTorrentDB` `:19-57` | `TorrentSpec` (`:21`), `Title` (`:22`), `Category` (`:23`), `Data` (`:24-37`), `Poster` (`:39-41`), `Size` (`:42-45`), `Timestamp` (`:47`), pin ×4 under `muTorrent` (`:48-54`) | Empty `Data` → JSON `{"TorrServer":{"Files":[...]}}` built from `Status().FileStats` copying only `Id`/`Path`/`Length` per file (`:26-29`, Phase 5: live `pinned`/`completed`/`downloaded` never stored; no-op for unpinned), stored and written back to `torr.Data` (`:30-34`, type `:13-17`). `Poster` saved only if `CheckImgUrl` succeeds. `Size` falls back to nil-safe `torr.Length()` when `torr.Torrent != nil` (`:43-44`; 0 before metadata — C1 fix, [Torrent pin](torrent-pin.md#gotchas--decisions)). Pin used only for a new row (`settings.AddTorrent` keeps an existing row's pin) |
| DB → Torrent | `GetTorrentDB` `:59-80` | `TorrentSpec`, `Title`, `Poster`, `Category`, `Timestamp`, `Size`, `Data` (`:64-70`), pin ×4 (`:71-74`); `Stat=TorrentInDB` (`:75`) | Linear scan of full `settings.ListTorrent()` (`:60-62`). `bt`, `cache`, `Torrent` stay nil |
| DB → Torrent | `ListTorrentsDB` `:86-106` | Same 7 fields (`:91-97`) + pin ×4 (`:98-101`); `Stat=TorrentInDB` (`:102`) | Map keyed by `TorrentSpec.InfoHash` (`:103`) |
| delete | `RemTorrentDB` `:82-84` | — | `settings.RemTorrent(hash)` |

Not persisted: `Stat`, `PreloadSize`, `PreloadedBytes`, speeds, `BitRate`, `DurationSeconds` (struct `server/torr/torrent.go:23-62`), the no-space set (`server/torr/pin.go:29-32`).

## settings/torrent.go
- `TorrentDB` (`server/settings/torrent.go:21-36`): embedded `*torrent.TorrentSpec`; `title`, `category`, `poster`, `data`, `timestamp`, `size`, `pin_mode`, `pin_next`, `pin_anchor`, `pin_downloaded` (all `omitempty`, pin `:32-35`). Pin constants `:15-19`.
- `File` struct (`:38-42`) — no `settings.File` reference found in `server/`.
- `AddTorrent` (`:46-61`): under package `mu`, reads the stored row (`getTorrentRow` `:134-144`); if present, **overwrites the given pin with the stored pin** (`:50-55`); marshals and `tdb.Set`s **only this row** (`:57-60`). Changed in pin Phase 1: previously re-marshalled every row from a `ListTorrent()` snapshot taken before `mu`, so a save of X could revert a pin just written for Y.
- `SetTorrentPin` (`:65-84`): mode/N of one existing row under `mu`; `mode == ""` also sets `PinDownloaded = nil` (`:74-76`), anchor kept; false if missing. `SetTorrentPinAnchor` (`:87-102`): same shape, `PinAnchor` only; caller `savePinAnchor`. `SetTorrentPinDownloaded` (`:106-123`): same shape, `PinDownloaded` only, false for a missing **or unpinned** row (`:112-114`); caller `savePinDownloaded`. See [Torrent pin](torrent-pin.md).
- `IsTorrentPinned` (`:126-131`): read-only, under `mu`; true when row exists and `PinMode != ""`. Callers: `Cache.Close`, `removeTorrentDirAfterClose`, `SetTorrentPin` (`wasPinned`).
- `ListTorrent` (`:146-170`): `dbMigrationLock.RLock` + `mu`; unmarshals every key of `"Torrents"`; sort by `Timestamp` desc.
- `RemTorrent` (`:172-176`): `tdb.Rem("Torrents", hex)` under `mu`.

## REST `/torrents` (`server/web/api/torrents.go`)
Request `torrReqJS` (`:23-34`): `action` (embedded `requestI`, `server/web/api/route.go:10-12`), `link`, `hash`, `title`, `category`, `poster`, `data`, `save_to_db`, `pin_mode`, `pin_next *int` (`:32-33`).

| action | Handler | Calls |
|---|---|---|
| `add` | `:97-168` | parse `link` (`torrs://` or magnet/hash/url `:110-133`) → `torr.AddTorrent` (`:135`); goroutine `GotInfo`, title fallback from `DisplayName`/`Name()`, `SaveTorrentToDB` if `save_to_db` (`:142-161`); restarts DLNA if enabled; responds `tor.Status()` (`:167`) |
| `get` | `:170-183` | `torr.GetTorrent(hash)` → `Status()` or 404 |
| `set` | `:185-192` | `torr.SetTorrent(hash,title,poster,category,data)` |
| `rem` | `:227-240` | `torr.RemTorrent` + `gstreamer.Remove` + DLNA restart |
| `list` | `:242-253` | `torr.ListTorrent()` → `[]Status()` (`[]` when empty) |
| `drop` | `:255-263` | `torr.DropTorrent` + `gstreamer.Remove` |
| `wipe` | `:265-278` | `RemTorrent` for every listed torrent |
| `pin` | `:194-225` | validate (400 hash/mode/negative N/UseDisk off for `all`/`next`, 403 ReadOnly) → `torr.SetTorrentPin` → 404 or `Status()`. See [Torrent pin](torrent-pin.md) |
| other | `:90-93` | 400 JSON `unknown action` |

## Flow — RemTorrent (`server/torr/apihelper.go:270-307`)
1. ReadOnly → log and return (`:271-274`).
2. `pinNoSpaceSet(hash, false)` for both branches (`:276`).
3. Not loaded: `RemTorrentDB` then `removeTorrentDir` (`:280-285`).
4. Loaded: remember `closed` chan (`:287`); **`RemTorrentDB` first** (`:291`) so a resume tick cannot reload a pinned torrent during removal; `bts.RemoveTorrent` (`:294`); if true, wait `closed` or 5 s (`:296-302`); then `removeTorrentDir` (`:305`). If `RemoveTorrent` returns false, the row is already gone and the dir stays.

## Cache dir `TorrentsSavePath/<hash>` deletion map
| Path | Deletes dir? | Cite |
|---|---|---|
| `RemTorrent` (API `rem`, `wipe`) | Yes, if `UseDisk` (pinned too) | `server/torr/apihelper.go:283`, `:305` |
| `SetTorrentPin(off)` (API `pin` `off`) | Yes, if `UseDisk` and pinned before the call (row or loaded mirror, C2): DB-only immediately; loaded after close unless loaded/pinned again | `server/torr/apihelper.go:205-207`, `:226-228`, `:246-268` |
| Pin anchor move (GET stream of a later episode) | No — only piece files lying solely in episodes before the anchor (`Cache.dropPieces`) | `server/torr/storage/torrstor/cache.go:199-233` |
| `DropTorrent` (API `drop`), timeout unload | Only via `Cache.Close` when `RemoveCacheOnDrop` and row not pinned | `server/torr/storage/torrstor/cache.go:268-283` |
| `SetSettings`/`SetDefSettings` | `dropAllTorrent` → `drop()` → `Cache.Close` rule above | `server/torr/apihelper.go:378-383` |
| Startup `cleanCache` | `RemoveCacheOnDrop=false`: 40-char entries whose name matches no DB hash (`:112-125`). `true`: every 40-char dir except pinned DB rows (`:126-135`) | `server/server.go:86-137` |

## Dependencies
- Overview: [server](_overview.md)
- [Torrent pin](torrent-pin.md), [Torrent lifecycle](torrent-lifecycle.md), [Streaming](streaming.md), [Preload](preload.md), [Torrent storage cache](torrent-storage-cache.md)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md), [CI test gate](../../concepts/ci-test-gate.md)
- Web consumer: [Torrent details dialog](../web/torrent-details-dialog.md)

## Gotchas / decisions
- `GetTorrent` returns a DB copy (`Stat=InDB`, no `Torrent`) and loads asynchronously (`server/torr/apihelper.go:119-135`); callers check `Stat == TorrentInDB` and call `AddTorrent` (`server/web/api/play.go:56-62`, `server/web/api/stream.go:138-144`), which reuses the goroutine's torrent if already in the map (`server/torr/torrent.go:90-92`). A `get` action therefore starts loading an unloaded torrent.
- Settings reload: `dropAllTorrent` + `Connect` replaces the map (`server/torr/btserver.go:70`); `resumePinned()` right after `bts.Connect()` reloads unfinished DB torrents pinned `all`/`next` (`server/torr/apihelper.go:354`, `:373`); unpinned torrents stay unloaded. A `TorrentsSavePath` change makes finished pins unfinished (data dir missing in the new path, `pinFinished`), so they reload and recompute their ids. Call sites have no unit test (real `Connect` opens listeners + 2 s sleeps); live smoke only. `dropAllTorrent` blocks forever if `closed` never fires (`server/torr/apihelper.go:381`).
- `AddTorrentDB` is expensive for frequent use: `Status()` when `Data` empty, synchronous poster HTTP GET (`server/torr/utils/webImageChecker.go:24-37`), row read + write under `settings.mu` (`server/settings/torrent.go:46-61`). A poster that fails the check is dropped from DB on every save (`server/torr/dbwrapper.go:39-41`).
- New `TorrentDB` fields need copying in 3 places: `AddTorrentDB`, `GetTorrentDB`, `ListTorrentsDB`; plus `GetTorrent` async copy (`server/torr/apihelper.go:125-132`) and `LoadTorrent` (`server/torr/apihelper.go:36-39`), which currently copy different subsets. Pin fields follow this pattern plus DB-owned rules (never change pin via `AddTorrentDB`) — see [Torrent pin](torrent-pin.md). `SetTorrent` round-trips `GetTorrentDB` → `AddTorrentDB` (`server/torr/apihelper.go:143`, `server/torr/apihelper.go:172`), so a new field is lost on `set` unless both copy it (pin: kept by `settings.AddTorrent` regardless).
- `NewTorrent` mutates `spec.Trackers` (`server/torr/torrent.go:69-81`) and `AddTorrentDB` persists `TorrentSpec` (`server/torr/dbwrapper.go:21`).
- `removeTorrentDir` deletes only when `UseDisk` is currently true (`server/torr/apihelper.go:235`); data written while `UseDisk` was on stays if the setting is later off.
- `RemTorrent` for a loaded torrent deletes the dir only if `RemoveTorrent` returned true (`server/torr/apihelper.go:294`); the DB row is removed before that regardless (`:291`). Residual race: a resume tick that copied the row just before the delete can still load it.
- `RemTorrent` clears the no-space entry before the up-to-5 s close wait (`:276`); a resume tick in that window can re-add it (accepted, [Torrent pin](torrent-pin.md#gotchas--decisions)).
- `RemTorrent` not-loaded branch gained the `os.Stat` + log line when extracted into `removeTorrentDir` (behaviour otherwise unchanged).
- ReadOnly (`sets.ReadOnly`) disables `RemTorrent`, `SetTorrentPin`, `SetSettings`, `SetDefSettings` (`server/torr/apihelper.go:271`, `:184`, `:341`, `:360`).

## Open questions
- `SetTorrent` with empty title on an unloaded torrent calls `GotInfo()` on the DB copy (`server/torr/apihelper.go:146-147`); the copy has `Torrent == nil`, so `WaitInfo` returns false (`server/torr/torrent.go:116-118`) and the copy is `Close()`d. Title then stays empty. Intended?
- `tdb` read caching (`server/settings/dbreadcache.go`) not reviewed beyond `Get` returning nil after `CloseDB` (`:34`); cost of `ListTorrent` per 1 s web poll (and per resume tick) unknown.
