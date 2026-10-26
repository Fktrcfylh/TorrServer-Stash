# Streaming

**App:** server
**Status:** stable
**Last reviewed:** 2026-10-26 (commit `e2d242e0`)

## Summary
`Torrent.Stream` serves one torrent file over HTTP with range support via `http.ServeContent`, backed by a `torrstor.Reader` (wrapped in a 1 MiB buffered reader) registered in the torrent's cache. It resolves the file by the web file id (natural-sort position from `Status()`), marks the file viewed, and closes the reader when the response ends. `/stream` and `/play` are the HTTP entry points. The stream path makes no NAS RPC or DNS call ([Remote download](remote-download.md)).

## Entry points
- `server/web/api/route.go:32-36` — `HEAD`/`GET /stream`, `/stream/*fname` → `stream`
- `server/web/api/route.go:38-39` — `HEAD`/`GET /play/:hash/:id` → `play`
- `server/web/api/stream.go:207` — `stream` with `play` query → `tor.Stream`
- `server/web/api/stream.go:327` — `streamNoAuth` → `tor.Stream`
- `server/web/api/play.go:84` — `play` → `tor.Stream`
- Loopback probes of `/play/<hash>/<id>` via `settings.LoopbackBaseURL()` (`server/settings/settings.go:50`): preload ffprobe (`server/torr/preload.go:110`), `/ffp` (`server/web/api/ffprobe.go:61`), tgbot (`server/tgbot/ffp.go:48`), GStreamer (`server/gstreamer/service.go:668-672`)

## Key files
| File | Role |
|---|---|
| `server/torr/stream.go` | `Torrent.Stream`, `activeStreams` counter, `GetActiveStreams` (`:188-190`) |
| `server/torr/stream_buffer.go` | `newBufferedStreamReader` — `bufio` over the reader, `Seek` corrects `SeekCurrent` by buffered bytes (`:13-28`) |
| `server/web/api/stream.go` | `/stream` multi-use endpoint: add/save/preload/stat/m3u/play |
| `server/web/api/play.go` | `/play/:hash/:id` |
| `server/settings/viewed.go` | `SetViewed` (`:29`), `RemViewed` (`:45`), `ListViewed` (`:59`) |
| `server/torr/storage/torrstor/reader.go` | Reader created by `Cache.NewReader`, see [Torrent storage cache](torrent-storage-cache.md) |
| `server/torr/stream_buffer_test.go` | `TestStreamBufferMultipartAndHead` (`:115`) — baseline failure, [CI / test gate](../../concepts/ci-test-gate.md) |

## Flow — `Torrent.Stream` (`server/torr/stream.go:41-185`)
1. `activeStreams++`, deferred `--` (`:43-44`); `streamTimeout = BTsets.TorrentDisconnectTimeout` (`:46`).
2. `GotInfo()`; false → `http.NotFound` + error (`:48-51`).
3. `Status()` (snapshot-only NAS fields) and pick `FileStats` entry with `Id == fileID` (`:53-60`); none → **404 written** + error (`:64-68`).
4. Match `t.Files()` by `Path()`; none → 404 (`:70-82`).
5. `sets.MaxSize` > 0 and file larger → 403 (`:84-89`).
6. `t.NewReader(file)`; nil (torrent closed after the info check) → 503 (`:91-97`). `defer t.CloseReader(reader)` (`:99`) — removes reader from cache, extends expiry.
7. `BTsets.ResponsiveMode` → `reader.SetResponsive()` (`:101-103`).
8. Viewed: existing timecode for `fileID` via `ListViewed`, then `SetViewed{Hash, FileIndex: fileID, TimeCode}` (`:116-128`). `FileIndex` is the web file id.
9. Headers: `Connection: close`, `Server`, `X-Stream-Timeout`, `ETag` (hex of `hash/path`), DLNA, mime, `Accept-Ranges` when `Range` sent (`:131-158`).
10. `http.ServeContent(resp, req, path, time.Unix(t.Timestamp,0), newBufferedStreamReader(reader, 1<<20))` (`:175`); returns nil.

## Readahead
- New `torrstor.Reader` starts with readahead 0 (`server/torr/storage/torrstor/reader.go:37`).
- `progressEvent` → `updateRA` sets 16 MB on every cache reader each second (`server/torr/torrent.go:284`, `:301-302`; `server/torr/storage/torrstor/cache.go:183-193`).
- Idle readers (>60 s since last access, more than one reader) are parked (readahead 0, `isUse=false`) unless a read/seek is in flight (`reader.go:168-174`, `:208-223`).

## Callers (entry-point behaviour only)
- `stream` (`server/web/api/stream.go:66-210`): `GetTorrent` (`:140`); `AddTorrent` if nil or `TorrentInDB` (`:147-148`); `GotInfo` (`:155`); `save` → `SaveTorrentToDB` (`:166`); `preload` → blocking `torr.Preload` (`:185-186`); then `stat` / `m3u` / `play` (`:193-207`).
- `play` (`server/web/api/play.go:28-85`): `GetTorrent` (`:44`); `AddTorrent` if `TorrentInDB` (`:56-57`); `GotInfo` (`:64`); single-file → index 1 (`:71-72`); `Stream` (`:84`).
- `streamNoAuth` (`server/web/api/stream.go:212-331`): 401 when `GetTorrent` finds the hash neither loaded nor in DB (`:261-264`).

## Dependencies
- Overview: [server](_overview.md)
- [Torrent lifecycle](torrent-lifecycle.md) (readers gate `expired()`), [Torrent storage cache](torrent-storage-cache.md), [Torrent API helpers](torrent-api-helpers.md), [Preload](preload.md), [Remote download](remote-download.md) (NAS peer, RPC-free status)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md) (file ids)

## Gotchas / decisions
- **No RPC/DNS on the stream path:** `Status()` inside `Stream` only reads the NAS status snapshot; `GetTorrent` never takes the per-hash NAS lock; the NAS peer is added from cache by `progressEvent`, never in the request ([Remote download](remote-download.md)).
- `SetViewed` runs on every `Stream` call before serving (`:124-128`), including `HEAD`, every range request and ffprobe probes (`server/torr/preload.go:109-115`, `/ffp`).
- Viewed is keyed by web file id, a position in the natural-sorted path list (`server/torr/torrent.go:446-458`), not the anacrolix file index.
- A torrent stays loaded while a stream reader exists: `expired()` requires `cache.Readers() == 0` (`server/torr/torrent.go:312`). After the reader closes, expiry moves to at least `now + TorrentDisconnectTimeout` (`:350`).
- `SetViewed` stores timecode 0 unless `BTsets.TrackTimecode` (`server/settings/viewed.go:31`).
- Every failure after the handler's checks writes a status (404/403/503) — upstream fix: an empty 200 made players see a zero-length file (`server/torr/stream.go:61-63`). Callers discard `Stream`'s error.
- `torrfs` and tgbot upload create readers directly and do not call `Stream` (`server/torrfs/torrfile.go:46`, `server/tgbot/upload/torrfile.go:68`), so they do not mark viewed.
