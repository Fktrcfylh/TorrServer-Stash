# Torrent API helpers

**App:** server
**Status:** stable
**Last reviewed:** 2026-10-26 (commit `e2d242e0`)

## Summary
Package-level functions in `server/torr/apihelper.go` operate on the global `bts *BTServer` (`server/torr/apihelper.go:19`); REST, tgbot, MCP, DLNA and torrfs call them. `dbwrapper.go` converts between in-memory `Torrent` and persisted `settings.TorrentDB`. `POST /torrents` dispatches `action` to these helpers. NAS-specific helpers (`SetTorrentRemote`, `removeRemote`) live in `server/torr/remote.go` — [Remote download](remote-download.md).

## Entry points
- `server/web/api/route.go:26` — `authorized.POST("/torrents", torrents)`
- `server/web/api/torrents.go:60` — `torrents` handler, `switch req.Action` (`:67-104`)
- `server/torr/apihelper.go:25` — `InitApiHelper` sets global `bts` once (`bts == bt` → no write, avoids a `-race` report); called from `Connect` (`server/torr/btserver.go:72`)
- `server/web/api/settings.go:54`, `:62` — `SetSettings` / `SetDefSettings` callers

## Key files
| File | Role |
|---|---|
| `server/torr/apihelper.go` | Load/add/get/set/rem/drop/list, settings reload, shutdown, preload sizing |
| `server/torr/remote.go` | `SetTorrentRemote`, `removeRemote` (called by `RemTorrent`) — [Remote download](remote-download.md) |
| `server/torr/dbwrapper.go` | `Torrent` <-> `TorrentDB` field copy |
| `server/settings/torrent.go` | `TorrentDB` model; `AddTorrent`/`SetTorrentRemoteDownload`/`ListTorrent`/`RemTorrent` on `tdb` bucket `"Torrents"` |
| `server/web/api/torrents.go` | REST action dispatch, `torrReqJS`, `remoteErrorStatus` |
| `server/torr/utils/webImageChecker.go` | `CheckImgUrl(link) (ok, verified)` (`:24`) |
| `server/torr/utils/torrent.go` | `InvalidateTrackersCache` (`:101`) used on settings change |
| `server/torr/apihelper_test.go` | `RemTorrent` dir/ReadOnly tests (`:86-134`) |
| `server/torr/dbwrapper_test.go` | DB copy, no pin keys in status, `Data` file layout, natural file ids (`:86-146`) |
| `server/web/api/torrents_test.go` | `TestTorrentsRejectionsCarryJSONBody` (`:46`), `TestPinActionIsUnknown` (`:137`), remote action/rem/wipe/list tests (`:246-508`) |

## apihelper.go
| Func | Lines | Behaviour |
|---|---|---|
| `LoadTorrent(tor)` | `:32-49` | nil if no spec; `NewTorrent`; copies the remote flag **before** `WaitInfo` (`:41`); `WaitInfo` (not `GotInfo`); then `Title`, `Poster`, `Data`. Callers: `server/web/api/m3u.go:157`, `server/mcp/helpers.go:66`, `:82` |
| `AddTorrent(spec,title,poster,data,category)` | `:51-97` | `NewTorrent` (returns existing if loaded); for each of Title/Category/Poster/Data only if empty: request arg, else DB value (`:60-89`); Title fallback `Info().Name` (`:65-67`). DB row → remote flag mirror (`:91-94`). Does not save to DB |
| `SaveTorrentToDB` | `:99-102` | log + `AddTorrentDB` |
| `GetTorrent(hashHex)` | `:104-135` | Loaded → extend expiry (capped 1 min) and return (`:110-114`). Else DB copy (`Stat=InDB`) returned immediately while a goroutine `NewTorrent` + copies Title/Poster/Data/Size/Timestamp/Category/remote flag + `GotInfo()` (`:119-132`) |
| `SetTorrent` | `:137-176` | Updates loaded torrent fields and DB row (`AddTorrentDB(torrDb)` `:169`); `data` only when non-empty |
| `RemTorrent(hashHex) error` | `:181-231` | See Flow |
| `ListTorrent()` | `:233-257` | `bts.ListTorrents()` + DB copies for hashes not loaded; sort `Timestamp` desc then `Title` desc |
| `DropTorrent` | `:259-262` | `bts.RemoveTorrent(hash)` → `Close` only; DB untouched |
| `SetSettings(set)` | `:264-280` | ReadOnly guard; `SetBTSets`; `InvalidateTrackersCache`; `dropAllTorrent`; sleep 1 s; `Disconnect`; `reconnect()`; sleep 1 s |
| `reconnect()` | `:286-301` | Up to 30 `Connect` attempts 1 s apart (uTP port of the old client closes lazily: "address already in use"); logs first error / final failure |
| `SetDefSettings()` | `:303-319` | Same with `SetDefaultConfig` |
| `dropAllTorrent` | `:321-326` | For each loaded torrent: `drop()` then block on `<-torr.closed` |
| `Shutdown` | `:328-340` | Embedded → `EmbeddedStop`; else `Disconnect`, `CloseDB`, `os.Exit(0)` |
| `WriteStatus` | `:347-356` | Under `bts.mu.RLock`; nil client → message |
| `Preload(torr,index)` | `:358-369` | Size calc, see [Preload](preload.md) |

## dbwrapper.go — field copy
| Direction | Func | Copied | Notes |
|---|---|---|---|
| Torrent → DB | `AddTorrentDB` `:19-61` | `TorrentSpec`, `Title`, `Category`, `Data` (`:24-34`), `Poster` (`:36-48`), `Size` (`:49-52`), `Timestamp` (`:54`), `RemoteDownload` under `muTorrent` (`:55-58`) | Empty `Data` → `{"TorrServer":{"Files":[...]}}` from `Status().FileStats` (`id/path/length`), written back to `torr.Data`. Poster checked by `CheckImgUrl` only when it differs from the stored one; failure keeps the stored poster (`:42-46`). `RemoteDownload` used only for a new row (`settings.AddTorrent` keeps the stored flag) |
| DB → Torrent | `GetTorrentDB` `:63-81` | `TorrentSpec`, `Title`, `Poster`, `Category`, `Timestamp`, `Size`, `Data`, `RemoteDownload` (`:68-75`); `Stat=TorrentInDB` | Linear scan of full `settings.ListTorrent()`. `bt`, `cache`, `Torrent` stay nil |
| DB → Torrent | `ListTorrentsDB` `:87-104` | Same 8 fields (`:92-99`); `Stat=TorrentInDB` | Map keyed by `TorrentSpec.InfoHash` |
| delete | `RemTorrentDB` `:83-85` | — | `settings.RemTorrent(hash)` |

Not persisted: `Stat`, `PreloadSize`, `PreloadedBytes`, speeds, `BitRate`, `DurationSeconds`, `remotePeerAt` (struct `server/torr/torrent.go:25-65`).

## settings/torrent.go
- `TorrentDB` (`server/settings/torrent.go:14-28`): embedded `*torrent.TorrentSpec`; `title`, `category`, `poster`, `data`, `timestamp`, `size`, `remote_download` (all `omitempty`).
- `AddTorrent` (`:38-50`): under `mu`, keeps the stored row's `RemoteDownload` (`:41-44`), marshals and writes **only this row**. Legacy `pin_*` keys in old rows load fine and vanish on the next save (`TestLegacyPinRowLoads` `server/settings/torrent_test.go:52`, `TestAddTorrentDropsLegacyPinKeys` `:67`).
- `SetTorrentRemoteDownload` (`:54-69`): flag-only write of an existing row; `getTorrentRow` (`:72-82`) with `mu` held.
- `ListTorrent` (`:84-108`): `dbMigrationLock.RLock` + `mu`; unmarshals every key; sort by `Timestamp` desc.
- `RemTorrent` (`:110-114`): `tdb.Rem("Torrents", hex)` under `mu`.
- `File` struct (`:30-34`) — no `settings.File` reference found in `server/`.

## REST `/torrents` (`server/web/api/torrents.go`)
Request `torrReqJS` (`:24-37`): `action` (embedded `requestI`), `link`, `hash`, `title`, `category`, `poster`, `data`, `save_to_db`, `remote_download *bool`.

| action | Handler | Calls |
|---|---|---|
| `add` | `:107-178` | parse `link` (`torrs://` or magnet/hash/url) → `torr.AddTorrent` (`:145`); goroutine `GotInfo`, title fallback, `SaveTorrentToDB` if `save_to_db` (`:152-171`); restarts DLNA if enabled; responds `Status()` |
| `get` | `:180-196` | `torr.GetTorrent` → `Status()` (+ `RefreshRemoteStatus` if flagged) or 404 |
| `set` | `:198-205` | `torr.SetTorrent` |
| `rem` | `:207-223` | `torr.RemTorrent`; error → `remoteErrorStatus`; then `gstreamer.Remove` + DLNA restart |
| `list` | `:225-242` | `torr.ListTorrent()` → `[]Status()` (`[]` when empty); any flagged → `RefreshRemoteStatus` |
| `drop` | `:244-252` | `torr.DropTorrent` + `gstreamer.Remove` |
| `wipe` | `:254-280` | `RemTorrent` for every listed torrent; failures counted → 502 |
| `remote` | `:282-297` | [Remote download](remote-download.md#rest-mapping) |
| other | `:100-103` | 400 JSON `unknown action` (incl. the removed `pin`) |

## Flow — RemTorrent
`server/torr/apihelper.go:181-231`:
1. ReadOnly → log, return `nil` (`:182-185`).
2. `lockRemote(hash)` for the whole call; `removeRemote(hash)` error → return it, torrent kept (`:188-191`) — [Remote download](remote-download.md#flow--remtorrent).
3. Not loaded: `RemTorrentDB`, then `os.RemoveAll(TorrentsSavePath/<hash>)` if `UseDisk` (`:194-203`).
4. Loaded: remember `closed`; `bts.RemoveTorrent`; if true wait `closed` or 5 s, then remove the dir if it exists (`:205-226`). DB row removed **last** (`:229`).

## Cache dir `TorrentsSavePath/<hash>` deletion map
| Path | Deletes dir? | Cite |
|---|---|---|
| `RemTorrent` (API `rem`, `wipe`) | Yes, if `UseDisk` (loaded: only when `RemoveTorrent` returned true) | `server/torr/apihelper.go:198-201`, `:219-225` |
| `DropTorrent`, timeout unload, settings reload | Only via `Cache.Close` when `RemoveCacheOnDrop` | `server/torr/storage/torrstor/cache.go:153-163` |
| Startup `cleanCache` | `RemoveCacheOnDrop=false`: 40-char dirs matching no DB hash. `true`: every 40-char dir | `server/server.go:92-133` |

NAS data is deleted by `torrent-remove` on untick/`rem`, not by any of these ([Remote download](remote-download.md)).

## Dependencies
- Overview: [server](_overview.md)
- [Remote download](remote-download.md), [Torrent lifecycle](torrent-lifecycle.md), [Streaming](streaming.md), [Preload](preload.md), [Torrent storage cache](torrent-storage-cache.md)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md), [CI test gate](../../concepts/ci-test-gate.md)
- Web consumer: [Torrent details dialog](../web/torrent-details-dialog.md)

## Gotchas / decisions
- `GetTorrent` returns a DB copy (`Stat=InDB`, no `Torrent`) and loads asynchronously (`server/torr/apihelper.go:116-132`); callers check `Stat == TorrentInDB` and call `AddTorrent` (`server/web/api/play.go:56`, `server/web/api/stream.go:147`, `:282`), which reuses the goroutine's torrent if already in the map (`server/torr/torrent.go:108-110`). A `get` action therefore starts loading an unloaded torrent.
- Settings reload: `dropAllTorrent` + `reconnect()` replaces the map (`server/torr/btserver.go:71`); torrents are not reloaded. `dropAllTorrent` blocks forever if `closed` never fires (`server/torr/apihelper.go:324`). Remote caches (`Shared` client, peer, status) rebuild on the next use when `RemoteDownloader` changed.
- `AddTorrentDB` is expensive for frequent use: `Status()` when `Data` empty, poster HTTP GET when the poster changed, row read + write under `settings.mu`.
- New `TorrentDB` fields need copying in `AddTorrentDB`, `GetTorrentDB`, `ListTorrentsDB`, plus the `GetTorrent` async load and `LoadTorrent`, which copy different subsets; `RemoteDownload` is copied in all five plus `AddTorrent`. `SetTorrent` round-trips `GetTorrentDB` → `AddTorrentDB`, so a field is lost on `set` unless both copy it (`RemoteDownload` survives anyway: row-owned).
- `NewTorrent` mutates `spec.Trackers` with retrackers (`server/torr/torrent.go:72-84`) and `AddTorrentDB` persists `TorrentSpec` — hence `originalTrackers` strips them before `torrent-add` ([Remote download](remote-download.md#payload--remoteaddrequest)).
- Dir removal checks `UseDisk` at call time; data written while `UseDisk` was on stays if the setting is later off.
- ReadOnly (`sets.ReadOnly`) disables `RemTorrent`, `SetTorrentRemote`, `SetSettings`, `SetDefSettings`.

## Open questions
- `SetTorrent` with empty title on an unloaded torrent calls `GotInfo()` on the DB copy (`server/torr/apihelper.go:142-144`); the copy has `Torrent == nil`, so `WaitInfo` returns false and `GotInfo` calls `Close()` on the copy. Title then stays empty. Intended?
- `tdb` read caching (`server/settings/dbreadcache.go`) not reviewed; cost of `ListTorrent` per 1 s web poll unknown (status refresh adds one more call per 5 s while a flagged torrent is listed).
