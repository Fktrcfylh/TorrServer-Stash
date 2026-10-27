# Wiki Index

## Apps

| Slug | Path | Type | Purpose | Status |
|---|---|---|---|---|
| [server](apps/server/_overview.md) | /server | core | Go torrent streaming server + REST API + NAS predownload (Transmission, qBittorrent) | partial |
| [web](apps/web/_overview.md) | /web | core | React admin UI (incl. NAS settings tab and NAS checkbox) | partial |

## Features

### server
- [Torrent lifecycle](apps/server/torrent-lifecycle.md) — `Torrent` struct/states, `WaitInfo`/`GotInfo` (locked, no state flip when working), watch/`progressEvent` (NAS peer re-add)/expired unload, `Status()` (sorted file copy, `remote_*`), `BTServer` Connect/configure
- [Torrent API helpers](apps/server/torrent-api-helpers.md) — Get/Load/Add/Rem/Drop/SetSettings (`reconnect()` retry), `RemTorrent` error + NAS-first removal, cache dir deletion map, TorrentDB copies, `POST /torrents` actions
- [Remote download](apps/server/remote-download.md) — NAS predownload: `remotedl` Downloader + Transmission legacy RPC (409 CAS retry, auth, timeouts, 40-hex guard) + qBittorrent Web API v2 (cookie login, no resend of rejected creds, no Referer, `Fails.` → duplicate check), `BTSets.RemoteDownloader` + `test_remote`, row-owned `remote_download` flag, `SetTorrentRemote`/`remoteAddRequest` (retrackers stripped, hash check, magnet fallback), `rem` removes on NAS first, lock stripes, NAS peer cache (best-effort; LPD is the working channel), `remote_*` status cache (refresh only on web list/get), live results
- [Streaming](apps/server/streaming.md) — `Torrent.Stream`, buffered reader, `SetViewed`, error statuses, RPC-free stream path, `LoopbackBaseURL` probes
- [Preload](apps/server/preload.md) — head/tail buffering before playback
- [Torrent storage cache](apps/server/torrent-storage-cache.md) — torrstor mem/disk pieces, eviction (complete pieces first), priorities, reader park/resume, `Close`/startup `cleanCache`

### web
- [Torrent details dialog](apps/web/torrent-details-dialog.md) — Table, TorrentFunctions (NAS checkbox + untick confirm + state/progress), TorrentCard NAS badge + flagged delete + comparator, RemoveAll `wipe`, TorrentCache, `/cache` polling
- [NAS settings](apps/web/remote-downloader-settings.md) — Settings tab "NAS": type (Transmission/qBittorrent, per-type hints)/URL/credentials/download dir, test connection with unsaved form values

## Concepts
- [Torrent status JSON](concepts/torrent-status-json.md) — `TorrentStatus`/`TorrentFileStat`, server ↔ web, `remote_*` contract, web consumers
- [CI / test gate](concepts/ci-test-gate.md) — Go (brew go1.27.3) / web gates, fixtures incl. remote download, test gotchas, baseline failures, web + NAS live smoke recipes

## Conventions
- Go: global `bts` registry (`server/torr/apihelper.go:19`); `bt.mu` guards the torrent map — `Torrent.Close` takes it, never call it holding `bt.mu` (`server/torr/btserver.go:285`).
- Go: logging via `log.TLogln` (`server/log/log.go:107`); tests `_test.go` next to code.
- Only time-based unload: `progressEvent` → `expired()` → `bt.RemoveTorrent` (`server/torr/torrent.go:249-255`, `:305-313`).
- File id = 1-based index after natural path sort of a **copy** of `t.Files()` (`server/torr/torrent.go:446-454`), recomputed in every `Status()`; viewed records use it.
- New `TorrentDB` field must be copied in `AddTorrentDB`, `GetTorrentDB`, `ListTorrentsDB` (`server/torr/dbwrapper.go`) and the async loads in `GetTorrent`/`LoadTorrent` (`server/torr/apihelper.go:119-132`, `:32-49`); `RemoteDownload` is copied in all of them plus `AddTorrent` (`:91-94`).
- `settings.AddTorrent` writes only its own row and keeps the stored `RemoteDownload` (`server/settings/torrent.go:38-50`); the flag changes only via `SetTorrentRemoteDownload` (`:54-69`) after a successful NAS RPC — [Remote download](apps/server/remote-download.md).
- `SetSettings`/`SetDefSettings` drop all torrents and `reconnect()` (≤ 30 tries) with a fresh map + storage (`server/torr/apihelper.go:264-319`, `server/torr/btserver.go:71`, `:99`); torrents are not reloaded.
- Disk piece = file `TorrentsSavePath/<hash>/<pieceId>`; mem vs disk chosen per call by `BTsets.UseDisk` (`server/torr/storage/torrstor/diskpiece.go:24`, `piece.go:31-53`). Hash dir deleted by `rem` and by `Cache.Close`/`cleanCache` with `RemoveCacheOnDrop`; never under a loaded cache.
- **No RPC or DNS on the stream path:** `Status()` reads only the NAS status snapshot; NAS peer added from a background-resolved cache; refresh only from web `list`/`get` — [Remote download](apps/server/remote-download.md).
- **Remote writes are synchronous** (tick/untick/`rem` in the request, 30 s budget); no reconcile loop; flag saved only on RPC success.
- **Lock order:** per-hash remote lock (`lockRemote`, `server/torr/remote.go:41-47`) → `bt.mu` / `muTorrent` / `settings.mu`; `remotePeers.mu`, `remoteStatuses.mu` are leaf locks.
- **Secret `BTSets` fields are masked in `(*BTSets).String()`** (`server/settings/btsets.go:129-136`) — it is what `Client config:` logs; add every new secret there.
- `clearPriority`+`setLoadPriority` run on every `getRemPieces` (`server/torr/storage/torrstor/cache.go:300-301`); with no active reader every piece goes to priority `None`.
- Status JSON uses `omitempty` — zero values vanish (`server/torr/state/state.go:33-81`); use a pointer when `0` must show (`remote_progress`). Unflagged JSON has no `remote_*` keys.
- Package-var test seams (`resolveRemotePeer`, `addPeers`, `fetchRemoteStatus`, `retrackers`, timings) are swapped under their cache mutex — [CI / test gate](concepts/ci-test-gate.md#remote-download-fixtures).
- Web: `axios.post(xHost(), {action, …})` (e.g. `web/src/components/DialogTorrentDetailsContent/TorrentFunctions/index.jsx:51`; host helpers `web/src/utils/Hosts.js:5-21`); MUI v4 + styled-components; i18next with 7 locales (`web/src/i18n.js:4-10`).
- Web: settings toggles are `FormControlLabel` + `Switch` (`web/src/components/Settings/SecondarySettingsComponent.jsx:200-207`); numeric settings are outlined `TextField id=<key> type=number onChange={inputForm}` (`:218-229`); nested settings objects (NAS) via `updateSettings({ RemoteDownloader: {...} })` ([NAS settings](apps/web/remote-downloader-settings.md)).
- Web: memo comparators gate re-render — `TorrentCard` compares a fixed field list incl. `remote_*` + file `id/path/length` (`web/src/components/TorrentCard/index.jsx:673-692`, `:141-151`). A new status field shown in the card/dialog must be added there. `TorrentFunctions` uses the default shallow `memo`.
- Web: derive control state from polled server status + minimal local state (pending/error/confirm); don't copy props into state via effects. After a write, `invalidateQueries('torrents', undefined, { cancelRefetch: true })` (`.../TorrentFunctions/index.jsx:52`). Requests that can fail on the NAS (`rem` of flagged, `wipe`) are awaited and show the server error.
- Web: feature PRs change `web/src` only; `server/web/pages/template/*` is regenerated by maintainer "update static web" commits — [CI / test gate](concepts/ci-test-gate.md).
- User-facing setup lives in `README.md` (NAS: `:5-73`); wiki links there instead of repeating it.
- Wiki/specs/code in English.
