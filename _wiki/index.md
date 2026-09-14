# Wiki Index

## Apps

| Slug | Path | Type | Purpose | Status |
|---|---|---|---|---|
| [server](apps/server/_overview.md) | /server | core | Go torrent streaming server + REST API | partial |
| [web](apps/web/_overview.md) | /web | core | React admin UI | partial |

## Features

### server
- [Torrent lifecycle](apps/server/torrent-lifecycle.md) — `Torrent` states, watch/`progressEvent` (downloaded ids before expiry)/expired unload, unload paths + resume after `Connect`, `Status()` pin fields, `sortedFiles` natural-sort file ids
- [Torrent API helpers](apps/server/torrent-api-helpers.md) — GetTorrent/LoadTorrent/AddTorrent/RemTorrent/DropTorrent/SetSettings, `removeTorrentDir`, cache dir deletion map, TorrentDB copies, `POST /torrents`
- [Torrent pin](apps/server/torrent-pin.md) — pin mode/N/anchor on DB row + mirror, `action:"pin"`, status `pin_mode`/`pin_next`; `pinPieces` window (`all` / `next` N from anchor, watched episodes dropped); on disk: no eviction, background download of wanted pieces, no unload until complete; GET of an episode moves the anchor (not HEAD/`?probe=1`/preload) and deletes pieces before it; `rem`/`off` (previously pinned) delete data dir, drop/timeout/`RemoveCacheOnDrop`/`cleanCache` keep pinned data; resume loop reloads unfinished `all`/`next` pins and re-applies loaded ones; status `pin_progress`/`pin_error` + per-file `pinned`/`completed`/`downloaded`; persisted `pin_downloaded` ids (finished pins skipped); free-space pause/resume (1 GiB margin, `no_space`) (Phases 1–6 complete; web UI in [Torrent details dialog](apps/web/torrent-details-dialog.md))
- [Streaming](apps/server/streaming.md) — `Torrent.Stream`, reader lifecycle, `SetViewed`, pin anchor hook + `ProbeQuery`
- [Preload](apps/server/preload.md) — head/tail buffering before playback
- [Torrent storage cache](apps/server/torrent-storage-cache.md) — torrstor mem/disk pieces, eviction, priorities, pin set (`SetPin`/`isKept`/`isWanted`/`PinPending`/`dropPieces`; paused = all-false want), `Close` keeps pinned rows, startup `cleanCache`

### web
- [Torrent details dialog](apps/web/torrent-details-dialog.md) — Table (+ predownload Disk column), TorrentFunctions (+ pin block, off confirm, comparator), TorrentCard pin badge + comparator, `DefaultPinNext` setting, TorrentCache, `/cache` polling

## Concepts
- [Torrent status JSON](concepts/torrent-status-json.md) — `TorrentStatus`/`TorrentFileStat`, server ↔ web, pin fields + Phase 6 UI contract + web consumers
- [CI / test gate](concepts/ci-test-gate.md) — Go/web gates (web needs `NODE_OPTIONS=--openssl-legacy-provider`), baseline failures, embedded pages not regenerated

## Conventions
- Go: global `bts` registry (`server/torr/apihelper.go:19`); `bt.mu` guards the torrent map — `Torrent.Close` takes it, never call it holding `bt.mu` (`server/torr/btserver.go:278`).
- Go: logging via `log.TLogln` (`server/log/log.go:107`); tests `_test.go` next to code.
- Only time-based unload: `progressEvent` → `expired()` → `bt.RemoveTorrent` (`server/torr/torrent.go:239-243`, `:291-300`); `cache.PinPending()` blocks it (`:296`); `progressEvent` records pin downloaded ids before the check (`:238`).
- File id = 1-based index after natural path sort (`sortedFiles`, `server/torr/pin.go:195-200`), recomputed in every `Status()` (`server/torr/torrent.go:434-441`) and `applyPin` (`:159`); viewed records, the pin anchor and `PinDownloaded` use it; stub ids come from the `Data` TorrServer file list stored at save time.
- New `TorrentDB` field must be copied in `AddTorrentDB`, `GetTorrentDB`, `ListTorrentsDB` (`server/torr/dbwrapper.go`) and the async loads in `GetTorrent`/`LoadTorrent` (`server/torr/apihelper.go:36-39`, `:124-131`).
- `settings.AddTorrent` writes only its own row and keeps an existing row's pin (`server/settings/torrent.go:46-61`); pin/anchor/downloaded change only via per-row setters under `settings.mu` (`SetTorrentPin` `:65-84`, `SetTorrentPinAnchor` `:87-102`, `SetTorrentPinDownloaded` `:106-123` pinned rows only), never `AddTorrentDB`; `AddTorrentDB` stores only `id`/`path`/`length` per file in `Data` — [Torrent pin](apps/server/torrent-pin.md).
- `SetSettings`/`SetDefSettings` drop all torrents and reconnect with a fresh map + storage (`server/torr/apihelper.go:378-383`, `server/torr/btserver.go:70`, `:98`), then `resumePinned()` (`server/torr/apihelper.go:354`, `:373`).
- Disk piece = file `TorrentsSavePath/<hash>/<pieceId>`; mem vs disk chosen per call by `BTsets.UseDisk` (`server/torr/storage/torrstor/diskpiece.go:24`, `piece.go:31-53`).
- Hash dir deleted only by `rem` and `pin off` of a previously pinned torrent (`removeTorrentDir`, `server/torr/apihelper.go:234-242`); never under a loaded cache (dir created only in `Cache.Init`); a DB row with any pin keeps data on close/`cleanCache` (`settings.IsTorrentPinned`, `server/settings/torrent.go:126-131`); piece files of a pinned torrent are deleted otherwise only by `Cache.dropPieces` (anchor rule) — [Torrent pin](apps/server/torrent-pin.md).
- Stream path never waits for the anchor DB write or piece deletion (goroutines; a concurrent `Status()` may wait one `applyPin` recompute on `muTorrent`) (`server/torr/pin.go:224`, `server/torr/storage/torrstor/cache.go:165-168`); loopback ffprobe URLs carry `?probe=1` (`torr.ProbeQuery`) — [Torrent pin](apps/server/torrent-pin.md).
- Tests starting `torr.StartResumePinned` must stop it via `t.Cleanup(StopResumePinned)` registered after fixture cleanups (runs first) — [CI / test gate](concepts/ci-test-gate.md).
- `clearPriority`+`setLoadPriority` run on every `getRemPieces` (`server/torr/storage/torrstor/cache.go:425-426`); non-reader not wanted pieces get priority `None`, wanted incomplete ones `Normal` (`cache.go:557-584`). Pinned torrent: every piece kept from eviction (`isKept`), only wanted ones downloaded (`isWanted`).
- Status JSON uses `omitempty` — zero values vanish (`server/torr/state/state.go:33-86`); pin progress fields appear only for pinned torrents (unpinned JSON unchanged).
- Pin free space: `torr.freeSpace` (`utils.FreeSpace`, per-OS build files) is a package-var test seam; tests install a double via `withFreeSpace`/`withDiskMode` — [CI / test gate](concepts/ci-test-gate.md).
- Web: `axios.post(xHost(), {action, …})` (e.g. `web/src/components/DialogTorrentDetailsContent/TorrentFunctions/index.jsx:116`; host helpers `web/src/utils/Hosts.js:5-19`); MUI v4 + styled-components; i18next with 7 locales (`web/src/i18n.js:4-26`).
- Web: settings toggles are `FormControlLabel` + `Switch` (`web/src/components/Settings/SecondarySettingsComponent.jsx:315-321`); numeric settings are outlined `TextField id=<key> type=number onChange={inputForm}` (`:234-245`).
- Web: memo comparators gate re-render — `TorrentCard` compares a fixed field list incl. `pin_*` + file `pinned/completed/downloaded` (`web/src/components/TorrentCard/index.jsx:647-666`, `:136-153`); `TorrentFunctions` re-renders only on pin props (`.../TorrentFunctions/index.jsx:243-250`). A new status field shown in the card/dialog must be added to these comparators.
- Web: derive control state from polled server status + nullable local drafts; don't copy props into state via effects. After a write, `invalidateQueries('torrents', undefined, { cancelRefetch: true })` (`.../TorrentFunctions/index.jsx:69`) — [Torrent details dialog](apps/web/torrent-details-dialog.md).
- Web: feature PRs change `web/src` only; `server/web/pages/template/*` is regenerated by maintainer "update static web" commits — [CI / test gate](concepts/ci-test-gate.md).
- Wiki/specs/code in English.
