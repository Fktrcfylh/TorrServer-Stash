# Torrent details dialog

**App:** web
**Status:** stable
**Last reviewed:** 2026-10-26 (commit `e2d242e0`, specs/nas-predownload Phase 6)

## Summary
Full-screen dialog opened from a torrent card. Shows poster/title, speed/peers/size/status/category widgets, torrent actions (playlist, NAS predownload block, remove views, drop, copy magnet), a buffer progress bar with a mini cache map, and a table of playable files with per-file preload/play/link buttons and "viewed" markers. A second view shows the full piece-level cache map. Also covers the torrent card parts tied to it: dialog host, memo comparator, NAS badge and the delete dialog. NAS server side: [Remote download](../server/remote-download.md); NAS settings tab: [NAS settings](remote-downloader-settings.md).

## Entry points
- `web/src/components/TorrentCard/index.jsx:421` — "Details" button → `openDetailedInfo` (`:188`).
- `web/src/components/TorrentCard/index.jsx:603-613` — `StyledDialog` renders `<DialogTorrentDetailsContent closeDialog torrent={torrent} />`.
- `web/src/components/App/index.jsx:49-51` — `useQuery('torrents', getTorrents, { refetchInterval: 1000 })`; `getTorrents` = `POST /torrents {action:'list'}` (`web/src/utils/Utils.js:61`). Data flows `App` → `TorrentList` → `TorrentCard` per torrent → dialog.

## Key files
| File | Role |
|---|---|
| `web/src/components/DialogTorrentDetailsContent/index.jsx` | Dialog body, layout, local state; passes `remote*` props |
| `.../customHooks.jsx` | `useUpdateCache`, `useCreateCacheMap`, `useGetSettings` |
| `.../helpers.js` | `isFilePlayable` by extension |
| `.../Table/index.jsx` | Playable file table (wide + short/mobile) |
| `.../TorrentFunctions/index.jsx` | Torrent action buttons, NAS block, untick confirm dialog |
| `.../TorrentFunctions/style.js` | `SmallLabel`, `MainSectionButtonGroup`, `RemoteControls` (`:41-50`) |
| `.../TorrentCache/index.jsx`, `getShortCacheMap.js`, `snakeSettings.js` | Canvas cache map ("snake") |
| `.../DetailedView/index.jsx` | Full cache view + debug checkbox |
| `.../widgets/index.jsx`, `widgets/useGetWidgetColors.jsx` | Stat widgets and colors |
| `web/src/components/TorrentCard/index.jsx`, `TorrentCard/style.js` | Card: dialog host, delete dialog, memo comparator, `RemoteBadge` (`style.js:271-286`) |
| `web/src/components/RemoveAll.jsx`, `UnsafeButton.jsx` | Sidebar "Remove all" (`wipe`) |
| `web/src/utils/Hosts.js` | API URL helpers |
| `web/src/i18n.js`, `web/src/locales/*/translation.json` | Translations |

## Flow
1. Props: `closeDialog`, `torrent` (one item of `/torrents list`). Destructures `poster, hash, title, category, name, stat, download_speed, upload_speed, torrent_size, file_stats, remote_download, remote_state, remote_progress, remote_error` (`index.jsx:50-65`). Shapes: [Torrent status JSON](../../concepts/torrent-status-json.md).
2. `useUpdateCache(hash)`: `setInterval` every **100 ms** → `POST /cache {action:'get', hash}` (`customHooks.jsx:5-35`).
3. `useGetSettings(cache)`: `POST /settings {action:'get'}` in an effect keyed on `cache`, i.e. after every cache update (`customHooks.jsx:63-70`). Used for `PreloadCache` → buffer size = `Capacity * PreloadCache%`, min 32 MB (`index.jsx:111-113`), and `remoteConfigured = settings ? Boolean(settings.RemoteDownloader?.URL) : undefined` (`:108-109`).
4. Viewed list: once per `hash`, `POST /viewed {action:'list', hash}` → sorted `file_index` array (`index.jsx:98-106`).
5. `playableFileList = file_stats.filter(isFilePlayable(path))` on every `file_stats` change (`index.jsx:86-88`).
6. Seasons parsed with `parse-torrent-title`; first season selected (`index.jsx:72-84`); season buttons when >1 season (`:243-262`).
7. Loader until cache non-empty and `stat` is not `GETTING_INFO`/`IN_DB` (`index.jsx:90-96`; states `web/src/torrentStates.js:1`).
8. Layout: widgets (`:189-196`), `TorrentFunctions` (`:200-212`), `CacheSection` buffer + mini `TorrentCache` + detailed-view button (`:216-238`), `Table` (`:264-270`).

### Table (`Table/index.jsx`)
- Props: `playableFileList, viewedFileList, selectedSeason, seasonAmount, hash` (`:33`).
- Columns: Viewed, Name, Season (one season only), Episode, Resolution (each only if parsed from any path), Size, Actions (`:78-84`).
- Row per file filtered by `selectedSeason`; viewed → row class `viewed-file-row` + indicator cell (`:91`, `:101-103`).
- File `id` usage: preload `fetch(streamHost()?link=hash&index=id&preload)` (`:38`); play link `streamHost()/<basename>?link=hash&index=id&play` (`:39-40`); GStreamer HLS player key `${id}:gst|stream` (`:41-49`).
- Player links gated by `localStorage` flags (`:60-69`); mobile `ShortTableWrapper` duplicates the data (`:181-295`).
- Empty list → plain untranslated string (`:71-72`). Re-renders only on deep prop change (`lodash/isEqual`, `:299`).

### NAS block (`TorrentFunctions`)
- Props add `remoteDownload, remoteState, remoteProgress, remoteError, remoteConfigured` (`TorrentFunctions/index.jsx:32-36`).
- Placement: after the playlist block, before `TorrentState` (`:111-133`): `SmallLabel` `Remote.Title`, `Checkbox` `Remote.Enable`, state line + `LinearProgress`.
- State: `checked = Boolean(remoteDownload)` from polled status (`:43`); local only `isRemotePending`, `remoteRequestError`, `isRemoteOffConfirmOpen` (`:40-42`).
- Disabled when pending, settings unknown (`remoteConfigured === undefined`) or unflagged and not configured (`:45`). A flagged torrent stays untickable without config (server then answers 400 — [Remote download](../server/remote-download.md#gotchas--decisions)).
- `sendRemote(on)` (`:47-59`): `POST /torrents {action:'remote', hash, remote_download}` → `invalidateQueries('torrents', undefined, { cancelRefetch: true })`; error 409 → `Remote.InfoPending`, else server `error` text or `error.message`.
- Tick → request; untick → MUI confirm `Dialog` (`Remote.OffConfirmTitle/Text`, Cancel / OK) → `sendRemote(false)` (`:60-65`, `:135-148`).
- Flagged: `Remote.State.<remote_state || 'unknown'>` + ` N%` and determinate bar when `remote_progress` is a number (`:119-129`). Helper texts: `Remote.NotConfigured` when `remoteConfigured === false`; `remote_error` (flagged only); request error (`:130-132`).
- `TorrentFunctions` is a plain `memo` (default shallow compare, `:24`); the playlist block now follows `viewedFileList`/`playableFileList` changes (the old pin-only comparator froze them).

### TorrentCard NAS badge, delete, comparator (`TorrentCard/index.jsx`)
- Reads `remote_download, remote_state, remote_progress, remote_error` (`:207-210`).
- `RemoteBadge` when flagged (`:567-573`): text `Remote.Badge` (`NAS {{progress}}%`) when progress is a number, else `Remote.BadgeShort`; `title` = state label (+ `: error`). Color via transient `$color` from `remoteBadgeColors` (`:139`: error/missing red `#E57373`, seeding green `#4CAF50`, else `accentCardColor`), style `style.js:271-286`.
- Delete (`:216-235`): unflagged → fire `rem` and close (unchanged); flagged → await `rem`, keep the dialog with the server error, buttons disabled while pending (dialog `:614-631`).
- Memo comparator (`:673-692`): `hash,title,name,poster,category,stat,torrent_size,download_speed,data,remote_download,remote_state,remote_progress,remote_error` + `sameFileList` (`:141-151`, file `id/path/length`).

### RemoveAll (`web/src/components/RemoveAll.jsx`)
- `wipe` via `fetch`; resolves to null or the server error text (`:13-30`); dialog awaits it, shows the error (`:42-52`, `:66-70`); Cancel disabled while pending.

### TorrentCache / DetailedView / widgets
- `useCreateCacheMap(cache)`: one entry per `PiecesCount` with `percentage`, `priority`, `isReader`, `isReaderRange` (`Start <= i < End`) (`customHooks.jsx:37-61`).
- Mini mode keeps only pieces with `percentage > 0`, padded (`TorrentCache/index.jsx:42-46`, `getShortCacheMap.js`) — loses piece positions. Canvas draw `:60-92`; debug letters when `localStorage.isSnakeDebugMode` (`:94`). Re-renders only when `cache.Pieces` or `cache.Readers` change (memo at file end).
- `DetailedView`: full `TorrentCache` (`DetailedView/index.jsx:72`), debug `Checkbox` stored in `localStorage` (`:56-62`).
- Widgets `widgets/index.jsx:19-132`; colors from `DarkModeContext` (`widgets/useGetWidgetColors.jsx:31`).

### API URL helpers (`web/src/utils/Hosts.js`)
- Base `REACT_APP_SERVER_HOST` or `window.location` origin (`:1-3`), overridable via `setTorrServerHost` (`:24-26`). Used here: `torrentsHost` (`:5`), `viewedHost` (`:6`), `cacheHost` (`:7`), `settingsHost` (`:9`), `streamHost`/`playlistTorrHost` (`:10`, `:13`).

### Translations
- `web/src/i18n.js`: i18next + `LanguageDetector`, en/ru/ua/zh/bg/fr/ro, fallback `en`. Keys flat (`"Viewed"` en `:429`) or nested (`"DetailedCacheView"` `:46`, `"Remote"` `:213`, `"SettingsDialog"` `:261`). `Remote` keys: [NAS settings](remote-downloader-settings.md#gotchas--decisions).

## Dependencies
- Overview: [web](_overview.md)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md), [CI / test gate](../../concepts/ci-test-gate.md)
- Server: [Remote download](../server/remote-download.md), [Torrent storage cache](../server/torrent-storage-cache.md) (`/cache` payload), [Torrent API helpers](../server/torrent-api-helpers.md), [Streaming](../server/streaming.md), [Preload](../server/preload.md), [Torrent lifecycle](../server/torrent-lifecycle.md)
- Sibling: [NAS settings](remote-downloader-settings.md)

## Gotchas / decisions
- **`TorrentCard` comparator gates the dialog:** the dialog lives inside `TorrentCard`; a status/file field not in the comparator (`TorrentCard/index.jsx:673-692`) or `sameFileList` never reaches the card or the dialog — extend it for new fields.
- **Checkbox follows the poll:** it turns checked only after `torrent-add` returns (~3 s on the NAS) and the next list; UI tests must poll `isChecked`, not assert right after the click.
- **Untick confirm only:** untick deletes the NAS data; tick has no confirm.
- **Delete of a flagged torrent can fail** (NAS unreachable) → error in the card dialog, torrent kept; unflagged delete keeps the old fire-and-close flow.
- **`UnsafeButton` copies `disabled` into state at mount** (`UnsafeButton.jsx:6`): later prop changes are ignored, so `RemoveAll` guards repeated clicks with `isPending` instead (`RemoveAll.jsx:43-44`).
- **Server error text shown verbatim** (English `{error}`); only 409 is translated (`Remote.InfoPending`).
- `/cache` is polled every 100 ms while the dialog is mounted; server `getCache` calls `torr.GetTorrent`, which loads a DB-only torrent and extends its expiry (`server/web/api/cache.go:52`). An open dialog keeps the torrent loaded.
- `/settings get` fires on every cache update (`customHooks.jsx:65-67`), ~10 req/s; `remoteConfigured` reuses it, no new request.
- Cache map `isReaderRange` uses `i < End` (`customHooks.jsx:52`) while server `inRanges` is inclusive (`server/torr/storage/torrstor/ranges.go:14-21`).
- Viewed list is fetched once per hash (`index.jsx:98-106`); playback elsewhere does not refresh it until remount.
- setState after unmount when the dialog closes mid NAS request (React 17 dev warning only; the card delete guards with `isMounted`).

## Open questions
- Does MUI v4 `Dialog` unmount children when `open=false` (affects whether `/cache` polling stops)? Not verified.
- Anacrolix priority numeric values behind the debug letters are not defined in repo.
