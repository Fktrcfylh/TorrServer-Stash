# Streaming

**App:** server
**Status:** stable
**Last reviewed:** 2026-09-13 (commit `215f6fca` + staged pin Phases 1–4)

## Summary
`Torrent.Stream` serves one torrent file over HTTP with range support via `http.ServeContent`, backed by a `torrstor.Reader` registered in the torrent's cache. It resolves the file by the web file id (natural-sort position from `Status()`), moves the pin anchor of a pinned torrent (async), marks the file viewed, and closes the reader when the response ends. `/stream` and `/play` are the HTTP entry points.

## Entry points
- `server/web/api/route.go:31-35` — `HEAD`/`GET /stream`, `/stream/*fname` → `stream`
- `server/web/api/route.go:37-38` — `HEAD`/`GET /play/:hash/:id` → `play`
- `server/web/api/stream.go:198` — `stream` with `play` query → `tor.Stream`
- `server/web/api/stream.go:318` — `streamNoAuth` (auth required, no user) → `tor.Stream`
- `server/web/api/play.go:84` — `play` → `tor.Stream`
- `server/torr/preload.go:107-116` — preload ffprobe probes `/play/<hash>/<index>` over loopback (see [Preload](preload.md))
- `server/web/api/ffprobe.go:62`, `server/tgbot/ffp.go:54` — `/ffp` loopback probes of `/play/<hash>/<id>?probe=1`

## Key files
| File | Role |
|---|---|
| `server/torr/stream.go` | `Torrent.Stream`, `activeStreams` counter, `GetActiveStreams` (`:179-181`) |
| `server/torr/pin.go` | `ProbeQuery` (`:204`), `updatePinAnchor` (`:209-226`) — see [Torrent pin](torrent-pin.md) |
| `server/web/api/stream.go` | `/stream` multi-use endpoint: add/save/preload/stat/m3u/play |
| `server/web/api/play.go` | `/play/:hash/:id` |
| `server/settings/viewed.go` | `Viewed` (`:9-13`), `SetViewed` (`:29-43`), `ListViewed` (`:59-90`), `RemViewed` (`:45-57`) |
| `server/torr/storage/torrstor/reader.go` | Reader created by `Cache.NewReader`, see [Torrent storage cache](torrent-storage-cache.md) |

## Flow — `Torrent.Stream` (`server/torr/stream.go:41-176`)
1. `activeStreams++`, deferred `--` (`:43-44`); `streamTimeout = BTsets.TorrentDisconnectTimeout` (`:46`).
2. `GotInfo()`; false → `http.NotFound` + error (`:48-51`).
3. `Status()` and pick `FileStats` entry with `Id == fileID` (`:53-60`); none → error, no HTTP response written (`:61-63`).
4. Match `t.Files()` by `Path()` (`:65-72`); none → error (`:73-75`).
5. `sets.MaxSize` > 0 and file larger → 403 (`:77-82`).
6. Pin anchor: `t.updatePinAnchor(fileID, file.Path(), req.Method, req.URL.Query().Has(ProbeQuery))` (`:83`) — GET of an episode of a pinned torrent, not `?probe=1`, not `Preload`, not ReadOnly → mirror anchor set + `go savePinAnchor`; never blocks on DB/cache/files. Return value ignored. See [Torrent pin](torrent-pin.md#flow--anchor-trigger-and-watched-cleanup).
7. `t.NewReader(file)` → `cache.NewReader` (`server/torr/torrent.go:327-333`); nil (torrent Closed) → error (`:85-88`). `defer t.CloseReader(reader)` (`:90`) — removes reader from cache, extends torrent expiry (`server/torr/torrent.go:335-338`).
8. `BTsets.ResponsiveMode` → `reader.SetResponsive()` (`:92-94`).
9. Viewed: read existing timecode for `fileID` via `ListViewed`, then `SetViewed{Hash, FileIndex: fileID, TimeCode}` (`:107-119`). `FileIndex` is the web file id.
10. Headers: `Connection: close`, `Server`, `X-Stream-Timeout`, `ETag` (hex of `hash/path`), DLNA, mime, `Accept-Ranges` when `Range` sent (`:122-149`).
11. `http.ServeContent(resp, req, path, time.Unix(t.Timestamp,0), reader)` (`:166`); returns nil after it finishes (`:175`).

## Readahead
- New `torrstor.Reader` starts with readahead 0 (`server/torr/storage/torrstor/reader.go:34`).
- `progressEvent` → `updateRA` sets 16 MB on every cache reader each second (`server/torr/torrent.go:270`, `:287-288`; `server/torr/storage/torrstor/cache.go:304-314`).
- Idle readers (>60 s since last access, more than one reader) switch readahead off (`server/torr/storage/torrstor/reader.go:163-169`).

## Callers (entry-point behaviour only)
- `stream` (`server/web/api/stream.go:57-201`): `GetTorrent`; `AddTorrent` if nil or `TorrentInDB` (`:131-144`); `GotInfo` (`:146`); `save` → `SaveTorrentToDB` (`:156-159`); index = 1 for single-file torrent else `index` query (`:162-170`); `preload` → blocking `torr.Preload` (`:176-178`); then `stat` / `m3u` / `play` (`:180-200`).
- `play` (`server/web/api/play.go:28-85`): `GetTorrent` (`:44`); `AddTorrent` if `TorrentInDB` (`:56-62`); `GotInfo` (`:64`); single-file → index 1 (`:71-72`); `Stream` (`:84`).
- `streamNoAuth` (`server/web/api/stream.go:203-323`): 401 when `GetTorrent` finds the hash neither loaded nor in DB (`:252-257`).

## Dependencies
- Overview: [server](_overview.md)
- [Torrent lifecycle](torrent-lifecycle.md) (readers gate `expired()`), [Torrent storage cache](torrent-storage-cache.md), [Torrent API helpers](torrent-api-helpers.md), [Preload](preload.md), [Torrent pin](torrent-pin.md) (anchor trigger)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md) (file ids)

## Gotchas / decisions
- `SetViewed` runs on every `Stream` call before serving (`server/torr/stream.go:115-119`), including `HEAD` requests (routes `server/web/api/route.go:31`, `:37`), every HTTP range request from a player, and ffprobe probes (`server/torr/preload.go:108-112`, `/ffp`). The pin anchor hook (`:83`) is narrower: GET only, `?probe=1` and `Stat == Preload` excluded; same file id again is a no-op, so range requests do not re-save.
- Loopback ffprobe callers must append `?` + `torr.ProbeQuery` + `=1` (`server/web/api/ffprobe.go:62`, `server/tgbot/ffp.go:54`); otherwise probing a later episode moves the anchor and drops earlier episodes. Third-party scanners GETting `/play` still move it (known limitation).
- Viewed and the pin anchor are keyed by web file id, a position in the natural-sorted path list (`sortedFiles`, `server/torr/torrent.go:434-441`), not the anacrolix file index.
- A torrent stays loaded while a stream reader exists: `expired()` requires `cache.Readers() == 0` (`server/torr/torrent.go:299`). After the reader closes, expiry is moved to at least `now + TorrentDisconnectTimeout` (`server/torr/torrent.go:337`).
- `SetViewed` stores timecode 0 unless `BTsets.TrackTimecode` (`server/settings/viewed.go:30-33`).
- Callers discard `Stream`'s error (`server/web/api/stream.go:198`, `server/web/api/play.go:84`); file-not-found paths write no status (`server/torr/stream.go:61-63`, `server/torr/stream.go:73-75`). No unit test drives `Stream`; the anchor call site is covered by live smoke ([Torrent pin](torrent-pin.md#live-smoke-phase-4-2026-09-13)).
- `torrfs` and tgbot upload create readers directly and do not call `Stream` (`server/torrfs/torrfile.go:46`, `server/tgbot/upload/torrfile.go:68`), so they do not mark viewed.

## Open questions
- Response status when `Stream` returns an error before writing (file id not found): gin default not verified.
