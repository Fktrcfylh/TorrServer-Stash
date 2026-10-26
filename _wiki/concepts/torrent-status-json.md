# Torrent status JSON

**Last reviewed:** 2026-10-26 (commit `e2d242e0`)

## Summary
`state.TorrentStatus` is the single JSON shape for a torrent across REST (`/torrents` list/get/add/remote, `/stream?stat` `server/web/api/stream.go:190`), `/cache` via `CacheState()` (`server/web/api/cache.go:55`), tgbot (`server/tgbot/status.go:368`) and MCP (`server/mcp/helpers.go:64`). It is built on demand by `Torrent.Status()`; nothing stores it. Unloaded torrents produce a reduced status from their DB copy. The web UI polls `action:list` every 1000 ms. The disk-pin fields (`pin_*`, file `pinned/completed/downloaded`) are gone since `763a4204`.

## Key files
| File | Role |
|---|---|
| `server/torr/state/state.go` | `TorrentStat` enum (`:3-31`), `TorrentStatus` (`:33-81`), `TorrentFileStat` (`:83-87`) |
| `server/torr/torrent.go` | `Status()` fills it (`:394-484`); `CacheState()` embeds it (`:486-493`) |
| `server/torr/remotestatus.go` | `setRemoteStatus` fills `remote_state/progress/error` from the snapshot (`:158-172`) |
| `server/torr/dbwrapper.go` | DB copies with `Stat=TorrentInDB` (`:76`, `:100`); `tsFiles` stores the file layout in `Data` (`:13-17`, `:24-34`) |
| `server/web/api/torrents.go` | `list` → `[]Status()` (`:225-242`); `get`, `add`, `remote` return `Status()` (`:192`, `:177`, `:296`) |
| `server/torr/storage/state/state.go` | `CacheState.Torrent *state.TorrentStatus` (`:7-16`) |
| `web/src/torrentStates.js` | `GETTING_INFO..IN_DB = 1..5` (`:1`); no constant for `TorrentAdded=0` |

## `stat` values (`server/torr/state/state.go:24-31`)
`0` added, `1` getting info, `2` preload, `3` working, `4` closed, `5` in db. `stat_string` = `TorrentStat.String()` (`:5-22`).

## TorrentStatus fields (`server/torr/state/state.go:33-81`)
| JSON | Go | Filled from (`server/torr/torrent.go`) | Unloaded (DB copy) |
|---|---|---|---|
| `title`, `category`, `poster` | string | `t.Title/Category/Poster` (`:402-404`) | yes |
| `data,omitempty` | string | `t.Data` (`:405`) | yes |
| `timestamp` | int64 | `t.Timestamp` (`:406`) | yes |
| `name,omitempty` | string | `Torrent.Name()` (`:416`) | no |
| `hash,omitempty` | string | spec hash (`:412-414`), overwritten by `Torrent.InfoHash()` (`:417`) | yes (spec) |
| `torrs_hash,omitempty` | string | `torrshash.Pack` when info present (`:460-476`) | no |
| `stat`, `stat_string` | int, string | `t.Stat` (`:400-401`) | `5`, `Torrent in db` |
| `loaded_size,omitempty` | int64 | `Torrent.BytesCompleted()` (`:418`) | no |
| `torrent_size,omitempty` | int64 | `t.Size` (`:407`), replaced by `Torrent.Length()` when info (`:444`) | yes (`Size`) |
| `preloaded_bytes`, `preload_size` (omitempty) | int64 | `t.PreloadedBytes`, `t.PreloadSize` (`:420-421`) | no |
| `download_speed`, `upload_speed` (omitempty) | float64 | `t.DownloadSpeed/UploadSpeed` (`:422-423`) | no |
| peers, bytes, chunks, pieces counters (omitempty) | int / int64 | `Torrent.Stats()` (`:425-441`) | no |
| `duration_seconds`, `bit_rate` (omitempty) | float64, string | ffprobe during preload (`server/torr/preload.go:112-113`), copied at `:408-409` | no |
| `remote_download,omitempty` | bool | mirror `t.RemoteDownload` (`:410`) | yes (row flag, `server/torr/dbwrapper.go:75`, `:99`) |
| `remote_state`, `remote_progress`, `remote_error` (omitempty) | string, `*float64`, string | NAS snapshot, flagged only (`:479-481`) — see [Remote fields](#remote-fields) | yes (same snapshot) |
| `file_stats,omitempty` | `[]*TorrentFileStat` | info present (`:443-458`) | no |

"Unloaded" = `Torrent == nil` branch skipped (`server/torr/torrent.go:415`); DB copy fields per `server/torr/dbwrapper.go:67-76`.

## TorrentFileStat (`server/torr/state/state.go:83-87`)
| JSON | Value |
|---|---|
| `id,omitempty` | `i + 1` after sorting a **copy** of `t.Files()` by `utils2.CompareStrings(path)` (`server/torr/torrent.go:446-454`) |
| `path,omitempty` | `File.Path()` (`:455`) |
| `length,omitempty` | `File.Length()` (`:456`) |

## Remote fields
Contract for flagged torrents (loaded and DB stubs); producer [Remote download](../apps/server/remote-download.md#status-cache):
- `remote_download: true` only while the DB row is flagged; absent otherwise. Unflagged JSON has **no** `remote_*` keys.
- Before the first refresh (or while the downloader is unconfigured): only `remote_download: true` → UI shows "Waiting for status".
- `remote_state`: `downloading | seeding | queued | stopped | checking | error | missing` (`server/remotedl/downloader.go:48-57`).
- `remote_progress`: percent 0..100, one decimal, pointer so `0` is present whenever the client reported the torrent; absent for `missing` and RPC errors (`server/torr/remotestatus.go:167-171`).
- `remote_error`: client `errorString` (also for tracker warnings that keep the state), or on RPC failure `remote_state:"error"` + `remote_error` = the client error (`transmission torrent-get: …` / `qbittorrent torrents/info: …`) for every flagged torrent (`:163-166`). qBittorrent sets `remote_error` only for unmapped states (`qBittorrent state "<s>"`).
- Staleness: snapshot ≤ 5 s old plus one refresh while a web client lists; tgbot/MCP `Status()` see the last snapshot and never trigger a refresh. The mirror `remote_download` may lag a concurrent toggle by one poll.

## Producers
- Loaded torrent: `Torrent.Status()` under `muTorrent` (`server/torr/torrent.go:395-396`).
- Unloaded torrent: `ListTorrent` merges DB copies (`server/torr/apihelper.go:233-257`); `Status()` on them yields only the "yes" rows above.
- Persisted file list: `AddTorrentDB` serializes `Status().FileStats` into `Data` as `{"TorrServer":{"Files":[...]}}` when `Data` is empty (`server/torr/dbwrapper.go:24-31`).

## Web consumers
- Poll: `useQuery('torrents', getTorrents, { refetchInterval: 1000 })` (`web/src/components/App/index.jsx:49-51`); `getTorrents` posts `{action:'list'}` (`web/src/utils/Utils.js:61`).
- `TorrentCard` reads `title, name, category, poster, torrent_size, download_speed, hash, stat, data, file_stats, remote_*` (`web/src/components/TorrentCard/index.jsx:196-211`); unloaded torrents without `file_stats` fall back to `data` → `TorrServer.Files` (`:75-81`); resolving players calls `action:'get'` up to 60 times at 1 s (`:63-71`).
- Details dialog reads `poster, hash, title, category, name, stat, download_speed, upload_speed, torrent_size, file_stats, remote_*` (`web/src/components/DialogTorrentDetailsContent/index.jsx:50-65`).
- `remote_*` consumers: card `RemoteBadge` and delete flow; dialog NAS block — [Torrent details dialog](../apps/web/torrent-details-dialog.md).

## Related
- [Torrent lifecycle](../apps/server/torrent-lifecycle.md), [Remote download](../apps/server/remote-download.md), [Torrent API helpers](../apps/server/torrent-api-helpers.md), [Streaming](../apps/server/streaming.md), [Preload](../apps/server/preload.md)

## Gotchas / decisions
- `omitempty` hides `0`/`false`/`""`; that is why `remote_progress` is a pointer.
- `TorrentCard` is `memo`ized with a field-list comparator (`web/src/components/TorrentCard/index.jsx:673-692`) incl. the four `remote_*` keys; `sameFileList` compares `id`, `path`, `length` (`:141-151`). A new status/file field shown in the card or dialog must be added there.
- File ids are recomputed per call and depend only on paths; ids stored in `Data` at save time follow the same sort. DB stubs have no `file_stats`.
- `action:'get'` on an unloaded torrent starts loading it (`server/torr/apihelper.go:116-132`).
- Status is computed for every torrent every second per open web client (`server/web/api/torrents.go:233-237`); `Status()` sorts files and packs `torrs_hash` each time; `remote_*` adds one map lookup under the leaf `remoteStatuses.mu`, no I/O. `TestStatusHasNoPinKeys` (`server/torr/dbwrapper_test.go:112`) and `TestListHasNoPinKeys` (`server/web/api/torrents_test.go:157`) guard the pin removal.

## Open questions
- `TorrentAdded` (`0`) has no web constant (`web/src/torrentStates.js:1`); how the card renders it not checked.
