# Torrent status JSON

**Last reviewed:** 2026-09-14 (commit `215f6fca` + staged pin Phases 1–6)

## Summary
`state.TorrentStatus` is the single JSON shape for a torrent across REST (`/torrents` list/get/add, `/stream?stat` `server/web/api/stream.go:181`), `/cache` via `CacheState()` (`server/web/api/cache.go:55`), tgbot (`server/tgbot/status.go`) and MCP (`server/mcp/helpers.go:64`). It is built on demand by `Torrent.Status()`; nothing stores it. Unloaded torrents produce a reduced status from their DB copy. The web UI polls `action:list` every 1000 ms.

## Key files
| File | Role |
|---|---|
| `server/torr/state/state.go` | `TorrentStat` enum (`:3-31`), `TorrentStatus` (`:33-74`), `PinErrorNoSpace` (`:77`), `TorrentFileStat` (`:79-86`) |
| `server/torr/torrent.go` | `Status()` fills it (`:377-502`); `CacheState()` embeds it (`:504-511`) |
| `server/torr/pin.go` | `pinTargets` (`:83-106`), `pinProgress` (`:110-122`), `fileCompleted` (`:249-257`), `dataFiles` (`:261-274`), no-space set (`:29-53`) |
| `server/torr/dbwrapper.go` | DB copies with `Stat=TorrentInDB` (`:75`, `:102`); `tsFiles` wrapper stores the file layout in `Data` (`:13-17`, `:24-34`) |
| `server/web/api/torrents.go` | `list` → `[]Status()` (`:242-253`); `get`, `add` return `Status()` (`:178`, `:167`) |
| `server/torr/storage/state/state.go` | `CacheState.Torrent *state.TorrentStatus` (`:7-16`) |
| `web/src/torrentStates.js` | `GETTING_INFO..IN_DB = 1..5` (`:1`); no constant for `TorrentAdded=0` |

## `stat` values (`server/torr/state/state.go:24-31`)
`0` added, `1` getting info, `2` preload, `3` working, `4` closed, `5` in db. `stat_string` = `TorrentStat.String()` (`:5-22`).

## TorrentStatus fields (`server/torr/state/state.go:33-74`)
| JSON | Go | Filled from (`server/torr/torrent.go`) | Unloaded (DB copy) |
|---|---|---|---|
| `title`, `category`, `poster` | string | `t.Title/Category/Poster` (`:385-387`) | yes |
| `data,omitempty` | string | `t.Data` (`:388`) | yes |
| `timestamp` | int64 | `t.Timestamp` (`:389`) | yes |
| `name,omitempty` | string | `Torrent.Name()` (`:403`) | no |
| `hash,omitempty` | string | spec hash (`:398-401`), overwritten by `Torrent.InfoHash()` (`:404-405`) | yes (spec) |
| `torrs_hash,omitempty` | string | `torrshash.Pack` when info present (`:456-472`) | no |
| `stat`, `stat_string` | int, string | `t.Stat` (`:383-384`) | `5`, `Torrent in db` |
| `loaded_size,omitempty` | int64 | `Torrent.BytesCompleted()` (`:406`) | no |
| `torrent_size,omitempty` | int64 | `t.Size` (`:390`), replaced by `Torrent.Length()` when info (`:432`) | yes (`Size`) |
| `preloaded_bytes`, `preload_size` (omitempty) | int64 | `t.PreloadedBytes`, `t.PreloadSize` (`:408-409`) | no |
| `download_speed`, `upload_speed` (omitempty) | float64 | `t.DownloadSpeed/UploadSpeed` (`:410-411`) | no |
| `total_peers`, `pending_peers`, `active_peers`, `connected_seeders`, `half_open_peers` (omitempty) | int | `Torrent.Stats()` (`:425-429`) | no |
| `bytes_written`, `bytes_written_data`, `bytes_read`, `bytes_read_data`, `bytes_read_useful_data` (omitempty) | int64 | `Torrent.Stats()` (`:414-418`) | no |
| `chunks_written`, `chunks_read`, `chunks_read_useful`, `chunks_read_wasted` (omitempty) | int64 | `Torrent.Stats()` (`:419-422`) | no |
| `pieces_dirtied_good`, `pieces_dirtied_bad` (omitempty) | int64 | `Torrent.Stats()` (`:423-424`) | no |
| `duration_seconds`, `bit_rate` (omitempty) | float64, string | ffprobe during preload (`server/torr/preload.go:113-114`), copied at `:391-392` | no |
| `pin_mode,omitempty` | string | `t.PinMode` (`:393`); `""` (off) omitted | yes (`server/torr/dbwrapper.go:71`) |
| `pin_next,omitempty` | int | `t.PinNext` (`:394`); stored for `all` too | yes (`server/torr/dbwrapper.go:72`) |
| `pin_progress,omitempty` | int 0..100 | pinned only: `floor(100 × Σ done / Σ length)` over target files with `length > 0` (`server/torr/pin.go:110-122`); loaded with info: done = live `completed` (`:453`); absent with `pin_mode` present = 0 | stub: done = `length` of downloaded targets — lower bound, partial files count 0 (`:488-493`); no TorrServer file list in `Data` → absent |
| `pin_error,omitempty` | string | `"no_space"` (`state.PinErrorNoSpace`) when pinned, `pin_progress < 100`, hash in the in-memory no-space set and `UseDisk` (`:496-499`) | yes (same rule; set survives unload, lost on restart) |
| `file_stats,omitempty` | `[]*TorrentFileStat` | info present (`:434-445`) | pinned rows only: from `Data` TorrServer file list (`:474-487`); unpinned: no |

"Unloaded" = `Torrent == nil` branch skipped (`server/torr/torrent.go:402`); DB copy fields per `server/torr/dbwrapper.go:64-75`.

## TorrentFileStat (`server/torr/state/state.go:79-86`)
| JSON | Go | Value |
|---|---|---|
| `id,omitempty` | int | `i + 1` after `sortedFiles(t.Files())` (natural sort by `utils2.CompareStrings(path)`, `server/torr/pin.go:195-200`) (`server/torr/torrent.go:434-438`); same ids used by the pin window, anchor and `pin_downloaded`; stub: `Id` from `Data` (`server/torr/pin.go:261-274`) |
| `path,omitempty` | string | `File.Path()` (`server/torr/torrent.go:439`); stub: `Data` |
| `length,omitempty` | int64 | `File.Length()` (`server/torr/torrent.go:440`); stub: `Data` |
| `pinned,omitempty` | bool | pinned only: file is a target of the pin (`pinTargets`, `server/torr/pin.go:83-106`) — `next` window episodes K..K+N, or for `all`/no episodes every file except episodes before K (`:449`, stub `:484`) |
| `completed,omitempty` | int64 | loaded pinned with info only: bytes of the file inside complete pieces via anacrolix `File.State()` (`fileCompleted`, `server/torr/pin.go:249-257`) (`:450`); never on stubs |
| `downloaded,omitempty` | bool | pinned only: id in persisted `pin_downloaded` = file fully complete on disk; any file of the pinned torrent, not only targets (loaded: mirror `:451`; stub: row `:485`) |

## Phase 6 UI contract
Exact semantics the web UI (Phase 6) relies on (consumers: [Web consumers](#web-consumers)):
- Pin fields exist only when `pin_mode` is `all`/`next`; an unpinned torrent's JSON is unchanged (none of `pin_progress`/`pin_error`/`pinned`/`completed`/`downloaded`).
- `pin_progress`: int 0..100, floor, bytes over target files. Loaded = live `completed` bytes; stub = lengths of `downloaded` targets (lower bound). Absent with `pin_mode` present = 0.
- `pin_error`: only `"no_space"`, only while `pin_progress < 100` and `UseDisk`; background download paused (streaming still works). Clears within one resume tick (≤ 1 min) after space frees up.
- `pinned`: target of the pin, not piece overlap (a subtitle sharing a boundary piece with a window episode is not pinned under `next`).
- `completed`: loaded pinned torrents only; bytes inside complete pieces (partial pieces count 0).
- `downloaded`: file fully on disk; can be true for non-target files (e.g. an episode outside the `next` window that was fully streamed).
- Stub `file_stats` (unloaded pinned row) come from the `Data` TorrServer list: `id`/`path`/`length`/`pinned`/`downloaded`, no `completed`. `Data` without that list (client-owned data, pin saved before metadata) → no `file_stats`, no `pin_progress`.
- Stub vs loaded: the same torrent alternates (resume loop loads unfinished/paused pins each minute, timeout unloads finished/paused ones); the UI must accept both shapes.

## Producers
- Loaded torrent: `Torrent.Status()` under `muTorrent` (`server/torr/torrent.go:378-379`).
- Unloaded torrent: `ListTorrent` merges DB copies (`server/torr/apihelper.go:313-317`); `Status()` on them yields only the "yes" rows above.
- Persisted file list: `AddTorrentDB` serializes `Status().FileStats` into `Data` as `{"TorrServer":{"Files":[...]}}` when `Data` is empty, copying only `id`/`path`/`length` per file (`server/torr/dbwrapper.go:24-34`); JSON tags match `TorrentFileStat`.

## Web consumers
- Poll: `useQuery('torrents', getTorrents, { refetchInterval: 1000 })` (`web/src/components/App/index.jsx:49-54`, interval `:51`); `getTorrents` posts `{action:'list'}` (`web/src/utils/Utils.js:61-68`, `:63`).
- `TorrentCard` reads `title, name, category, poster, torrent_size, download_speed, hash, stat, data, file_stats, pin_mode, pin_progress, pin_error` (`web/src/components/TorrentCard/index.jsx:192-206`); for unloaded torrents without `file_stats` falls back to `data` → `TorrServer.Files` (`:72-79`, `:242-246`); resolving players calls `action:'get'` up to 60 times at 1 s (`:60-68`, `:365`). Pin badge (`pin_mode` all/next; `no_space` + `pin_progress < 100` → error variant) at `:207-208`, `:541-545`.
- Details dialog reads `poster, hash, title, category, name, stat, download_speed, upload_speed, torrent_size, file_stats, pin_mode, pin_next, pin_progress, pin_error` (`web/src/components/DialogTorrentDetailsContent/index.jsx:50-65`). See [Torrent details dialog](../apps/web/torrent-details-dialog.md).
- Pin field consumers (Phase 6):
  | Field | Component |
  |---|---|
  | `pin_mode`, `pin_next` | `TorrentFunctions` (checkbox/radio/N, derived — `.../TorrentFunctions/index.jsx:45-57`); `TorrentCard` badge (`pin_mode` only) |
  | `pin_progress` | `TorrentFunctions` label `N%` (`:156-159`); `TorrentCard` badge |
  | `pin_error` | `TorrentFunctions` `Pin.NoSpace` (`:197`); `TorrentCard` error badge |
  | file `pinned`, `completed`, `downloaded` | `Table` `pinState` / Disk column (`.../Table/index.jsx:33-38`, `:63`) — playable files only |
- Settings `UseDisk`, `DefaultPinNext` reach `TorrentFunctions` from `/settings get`, not from status JSON.

## Related
- [Torrent lifecycle](../apps/server/torrent-lifecycle.md), [Torrent pin](../apps/server/torrent-pin.md), [Torrent API helpers](../apps/server/torrent-api-helpers.md), [Streaming](../apps/server/streaming.md), [Preload](../apps/server/preload.md)

## Gotchas / decisions
- Pin: absent `pin_mode` = off, absent `pin_next` with a mode = N 0; `pin_anchor` and the raw `pin_downloaded` list are DB-only, exposed only through per-file `downloaded`. See [Torrent pin](../apps/server/torrent-pin.md).
- Adding fields: `omitempty` hides `0`/`false`/`""` — `completed: 0`, `downloaded: false`, `pinned: false` and `pin_progress: 0` are absent from JSON.
- `TorrentCard` is `memo`ized with a custom comparator on 13 fields (incl. `pin_mode`, `pin_next`, `pin_progress`, `pin_error`) + `sameFileList` (`web/src/components/TorrentCard/index.jsx:647-666`); `sameFileList` compares `id`, `path`, `length`, `pinned`, `completed`, `downloaded` (`:136-153`). Any other new status/file field will not re-render the card (or the dialog inside it) unless added there. `TorrentFunctions` has its own comparator on pin props only (`.../TorrentFunctions/index.jsx:243-250`).
- File ids are recomputed per call and depend only on paths; ids stored in `Data` at save time follow the same sort (`server/torr/dbwrapper.go:27`).
- DB copies of **unpinned** torrents have no `file_stats`; pinned stubs build them from `Data` + row (Phase 5). Clients reading `data` → `TorrServer.Files` directly still see only `id`/`path`/`length` (live pin state is stripped on save).
- `action:'get'` on an unloaded torrent starts loading it (`server/torr/apihelper.go:119-135`).
- Status is computed for every torrent every second per open web client (`server/web/api/torrents.go:249-251`); `Status()` sorts files and packs `torrs_hash` each time (`server/torr/torrent.go:434-472`); pinned torrents add a target pass and one `File.State()` per file; no DB write in `Status()`.

## Open questions
- `TorrentAdded` (`0`) has no web constant (`web/src/torrentStates.js:1`); how the card renders it not checked.
