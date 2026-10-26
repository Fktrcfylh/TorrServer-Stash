# Preload

**App:** server
**Status:** stable
**Last reviewed:** 2026-10-26 (commit `e2d242e0`)

## Summary
Before playback, `Torrent.Preload` downloads the start of one file (up to a size derived from `CacheSize * PreloadCache%`) and a fixed-size tail, so players can open the file and probe its container quickly. It runs synchronously in the caller, switches the torrent to `TorrentPreload` for the duration, and optionally fills `BitRate`/`DurationSeconds` via ffprobe. Unrelated to NAS predownload ([Remote download](remote-download.md)).

## Entry points
- `server/web/api/stream.go:185-186` — `/stream?...&preload` (auth path)
- `server/web/api/stream.go:310-311` — same, `streamNoAuth`
- `server/tgbot/preload.go:32`, `:46` — tgbot command/callback
- `server/torr/apihelper.go:358-369` — `torr.Preload(torr, index)`: size calc, then `torr.Preload(index, size)`

## Key files
| File | Role |
|---|---|
| `server/torr/apihelper.go` | Size from settings |
| `server/torr/preload.go` | `Torrent.Preload` (`:20-273`), `findFileIndex` (`:275-293`) |
| `server/settings/btsets.go` | `CacheSize` (`:51`), `PreloadCache` percent (`:53`); defaults 64 MB / 50 (`:228-229`); `PreloadCache` clamped 0..100 (`:185-190`) |
| `server/ffprobe` | `ffprobe.Exists`, `ProbeUrl` used at `server/torr/preload.go:109-115` |

## Flow
1. **Size** (`server/torr/apihelper.go:358-369`): `size = CacheSize/100 * PreloadCache`; `<= 0` → no preload; capped at `CacheSize`.
2. **Guard** (`server/torr/preload.go:20-43`): `size <= 0` return; `PreloadSize = size` (`:24`); `GettingInfo` → `WaitInfo` + 100 ms sleep (`:26-32`); under `muTorrent`, return unless `Stat == Working && Torrent != nil`; take the anacrolix pointer once (survives a concurrent `drop()`), set `Stat = Preload` (`:34-43`).
3. **Deferred exit** (`:45-54`): `Preload` → `Working` under `muTorrent`; clear `BitRate`, `DurationSeconds`.
4. **File** (`:56-63`): `findFileIndex(torrent, index)` matches web id via `Status().FileStats` then path; fallback `torrent.Files()[0]`; `size` capped to file length.
5. **Log/keepalive goroutine** (`:74-107`): every 1 s while `Stat == Preload`: log progress and `AddExpiredTime(timeout)`, timeout = `TorrentDisconnectTimeout` capped 1 min (`:69-72`).
6. **ffprobe** (`:109-115`): if available, probe `settings.LoopbackBaseURL() + /play/<hash>/<index>`; sets `BitRate`, `DurationSeconds`.
7. **Closed check** (`:117-125`).
8. **Ranges** (`:127-154`): `startend = max(PieceLength, 8 MB)`; start range `[0, size - startend)` (negative → `size`, capped to file length); end range `[Length - startend, Length)`, read only if `readerEndStart > readerStartEnd` (`:160`).
9. **End range goroutine** (`:160-215`): anacrolix `file.NewReader()`, responsive, readahead 0, seek, 32 KB chunks; stops on error or `Stat != Preload`.
10. **Start range** (`:217-251`): readahead `4*PieceLength` unless the range is smaller; 32 KB chunks until `offset + 32KB >= readerStartEnd`; cancel if `Stat != Preload`; readahead → 0 near the end.
11. `wg.Wait()` for the tail (`:254`); final log (`:261-272`); defer restores `Working`.

## Interaction with cache / readers
- Preload readers are anacrolix `file.NewReader()` (`:133`, `:174`), not `torrstor` readers: not counted by `cache.Readers()` (`server/torr/storage/torrstor/cache.go:400-407`) and give no reader ranges to eviction.
- With no torrstor readers, `getRemPieces` treats every filled piece as a candidate (`server/torr/storage/torrstor/cache.go:292-297`); eviction happens only when `filled > capacity` (`:252`). Size is capped at `CacheSize`.
- `PreloadedBytes` in status is `cache.GetState().Filled`, updated each second (`server/torr/torrent.go:270-272`), i.e. total cache fill.
- Details: [Torrent storage cache](torrent-storage-cache.md).

## Dependencies
- Overview: [server](_overview.md)
- [Torrent lifecycle](torrent-lifecycle.md) (states), [Streaming](streaming.md) (ffprobe → `/play`), [Torrent API helpers](torrent-api-helpers.md), [Torrent storage cache](torrent-storage-cache.md)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md) (`preload_size`, `preloaded_bytes`, `bit_rate`, `duration_seconds`)

## Gotchas / decisions
- Synchronous: `/stream?preload&play` streams only after preload returns (`server/web/api/stream.go:185-207`).
- Skipped unless `Stat == Working`: a torrent loaded by `LoadTorrent` stays `TorrentAdded`; a second concurrent preload sees `Preload`.
- While `Stat == Preload`, `expired()` is false and `GotInfo()` returns true without state change (`server/torr/torrent.go:194-198`).
- Cancellation is cooperative: anything that changes `Stat` away from `Preload` (e.g. `Close`) stops read loops at the next chunk.
- The ffprobe probe goes through `Stream`, which calls `SetViewed` for the file and creates a torrstor reader.
- `PreloadSize` is set before the state guard (`:24`), so it is updated even when preload is skipped.
- `BitRate`/`DurationSeconds` are cleared in the defer outside `muTorrent` (`:51-53`).
- Comments in `preload.go` are partly Russian (`:51`, `:78`, `:145`, `:149`).

## Open questions
- Fallback `torrent.Files()[0]` (`:58`) is the anacrolix order, not necessarily web id 1. `Status()` now sorts a copy, so the torrent's slice is not reordered (`server/torr/torrent.go:446-448`).
