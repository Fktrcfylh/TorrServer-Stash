# Preload

**App:** server
**Status:** stable
**Last reviewed:** 2026-09-13 (commit `215f6fca`)

## Summary
Before playback, `Torrent.Preload` downloads the start of one file (up to a size derived from `CacheSize * PreloadCache%`) and a fixed-size tail, so players can open the file and probe its container quickly. It runs synchronously in the caller, switches the torrent to `TorrentPreload` for the duration, and optionally fills `BitRate`/`DurationSeconds` via ffprobe.

## Entry points
- `server/web/api/stream.go:176-178` — `/stream?...&preload` (auth path)
- `server/web/api/stream.go:301-303` — same, `streamNoAuth`
- `server/tgbot/preload.go:32`, `:46` — tgbot command/callback
- `server/torr/apihelper.go:403-414` — `torr.Preload(torr, index)`: size calc, then `torr.Preload(index, size)`

## Key files
| File | Role |
|---|---|
| `server/torr/apihelper.go` | Size from settings |
| `server/torr/preload.go` | `Torrent.Preload`, `findFileIndex` (`:275-293`) |
| `server/settings/btsets.go` | `CacheSize` (`:38`), `PreloadCache` percent (`:40`); defaults 64 MB / 50 (`:204-205`); `PreloadCache` clamped 0..100 (`:165-170`) |
| `server/ffprobe` | `ffprobe.Exists`, `ProbeUrl` used at `server/torr/preload.go:107-116` |

## Flow
1. **Size** (`server/torr/apihelper.go:403-414`): `size = CacheSize/100 * PreloadCache`; `<= 0` → no preload; capped at `CacheSize`.
2. **Guard** (`server/torr/preload.go:21-41`): `size <= 0` return; `PreloadSize = size` (`:24`); if `GettingInfo` → `WaitInfo` + 100 ms sleep (`:26-32`); under `muTorrent`, return unless `Stat == TorrentWorking` (`:34-38`); set `Stat = TorrentPreload` (`:40`).
3. **Deferred exit** (`:43-52`): under `muTorrent`, `Preload` → `Working`; clear `BitRate`, `DurationSeconds`.
4. **File** (`:54-61`): `findFileIndex(index)` matches web id via `Status().FileStats` then path (`:275-293`); fallback `t.Files()[0]`; `size` capped to file length.
5. **Log/keepalive goroutine** (`:73-105`): every 1 s while `Stat == Preload`: log progress and `AddExpiredTime(timeout)`, timeout = `TorrentDisconnectTimeout` capped 1 min (`:67-70`, `:100`).
6. **ffprobe** (`:107-116`): if available, probe `http(s)://127.0.0.1:<port>/play/<hash>/<index>`; sets `BitRate`, `DurationSeconds`.
7. **Closed check** (`:118-126`).
8. **Ranges** (`:128-155`):
   - `startend = max(PieceLength, 8 MB)` (`:129-132`).
   - Start range `[0, readerStartEnd)`, `readerStartEnd = size - startend`; if negative → `size`; capped to file length (`:143-152`).
   - End range `[Length - startend, Length)` (`:154-155`), read only if `readerEndStart > readerStartEnd` (`:161`).
9. **End range goroutine** (`:161-216`): anacrolix `file.NewReader()`, responsive, readahead 0, seek, read 32 KB chunks; stops on error or `Stat != Preload`.
10. **Start range** (`:219-252`): anacrolix reader (`:134`), responsive; readahead `4*PieceLength` unless range smaller (`:219-224`); read 32 KB chunks until `offset + 32KB >= readerStartEnd`; cancel if `Stat != Preload` (`:229-237`); readahead → 0 near end (`:248-251`).
11. `wg.Wait()` for tail (`:255`); final log (`:263-272`); defer restores `Working`.

## Interaction with cache / readers
- Preload readers are anacrolix `file.NewReader()` (`:134`, `:175`), not `torrstor` readers: not in `cache.readers`, so not counted by `cache.Readers()` (`server/torr/storage/torrstor/cache.go:520-527`) and give no reader ranges to eviction.
- With no torrstor readers, `getRemPieces` treats every filled unpinned piece as removable (`server/torr/storage/torrstor/cache.go:417-422`; `isIdInFileBE` protects nothing when `ranges` is empty, `:483-493`); eviction happens only when `filled > capacity` (`:373`). Size is capped at `CacheSize` (`server/torr/apihelper.go:410-412`). A pinned torrent evicts nothing (`isKept`).
- `PreloadedBytes` shown in status is `cache.GetState().Filled` updated each second (`server/torr/torrent.go:260-262`), i.e. total cache fill, not preload-specific.
- Details of eviction and priorities: [Torrent storage cache](torrent-storage-cache.md).

## Dependencies
- Overview: [server](_overview.md)
- [Torrent lifecycle](torrent-lifecycle.md) (states), [Streaming](streaming.md) (ffprobe → `/play`), [Torrent API helpers](torrent-api-helpers.md), [Torrent storage cache](torrent-storage-cache.md)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md) (`preload_size`, `preloaded_bytes`, `bit_rate`, `duration_seconds`)

## Gotchas / decisions
- Synchronous: `/stream?preload&play` streams only after preload returns (`server/web/api/stream.go:176-200`).
- Preload is skipped unless `Stat == Working` (`:35-38`): a torrent loaded by `LoadTorrent` stays `TorrentAdded` (`server/torr/apihelper.go:33`), a second concurrent preload sees `Preload`.
- While `Stat == Preload`, `expired()` is false (`server/torr/torrent.go:299`) and `GotInfo()` returns true without state change (`server/torr/torrent.go:198-200`).
- Cancellation is cooperative: anything that changes `Stat` away from `Preload` (e.g. `Close` sets `Closed`, `server/torr/torrent.go:363`) stops read loops at the next chunk.
- ffprobe probe goes through `Stream`, which calls `SetViewed` for the file (`server/torr/stream.go:115-119`) and creates a torrstor reader. It does not move the pin anchor: `updatePinAnchor` ignores `Stat == TorrentPreload` (`server/torr/pin.go:210`) — [Torrent pin](torrent-pin.md).
- `PreloadSize` is set before the state guard (`:24`), so it is updated even when preload is skipped.
- Comments in `preload.go` are partly Russian (`:49`, `:76`, `:146`, `:150`).

## Open questions
- Fallback `t.Files()[0]` (`server/torr/preload.go:56`): is it web id 1? `Status()` and `applyPin` sort the slice returned by `t.Files()` in place (`sortedFiles`, `server/torr/pin.go:195-200`, called at `server/torr/torrent.go:159`, `:434`); whether that reorders anacrolix's internal slice depends on library source not available locally.
- `t.Files()[0]` on a torrent without info would panic; guarded only indirectly by `Stat == Working` (`:35`) before `t.Info() == nil` check (`:63`).
