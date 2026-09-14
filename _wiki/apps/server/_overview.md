# TorrServer backend

**Type:** core
**Root:** `/server`
**Entry point:** `server/cmd/main.go:63` (`main`) → `server/server.go:23` (`Start`)

## Purpose
Go HTTP server that loads torrents via anacrolix/torrent and streams their files to players over HTTP (`/stream`, `/play`), with a REST API (`POST /torrents`, `/cache`, `/settings`, …) consumed by the web UI, DLNA, tgbot, MCP and torrfs. Torrent pieces are held in a custom storage (`torrstor`) in RAM or on disk.

## Features
- [Torrent lifecycle](torrent-lifecycle.md) — `Torrent` struct, states, watch/expiry/unload, `Status()` (incl. pin fields) and file ids
- [Torrent API helpers](torrent-api-helpers.md) — Get/Load/Add/Rem/Drop/SetSettings, TorrentDB copies, `POST /torrents` actions
- [Torrent pin](torrent-pin.md) — pin model (`all`/`next`), `action:"pin"`, DB-owned pin; pin set (window from anchor) keeps all pieces and downloads wanted ones on disk; episode GET moves the anchor and drops watched episodes; `rem`/`off` (previously pinned) delete data, other paths keep pinned data; unfinished `all`/`next` resumed after `Connect` + every minute; per-file pin progress, persisted downloaded ids, free-space pause (Phases 1–6 complete; web UI: [Torrent details dialog](../web/torrent-details-dialog.md))
- [Streaming](streaming.md) — `Torrent.Stream`, readers, `SetViewed`, pin anchor hook
- [Preload](preload.md) — pre-playback head/tail buffering
- [Torrent storage cache](torrent-storage-cache.md) — `torrstor` pieces (mem/disk), eviction, priorities, readers, pin set + `dropPieces`, `Close`/`cleanCache` keep rules

Not ingested yet: `dlna`, `tgbot`, `mcp`, `torrfs`, `torznab`, `rutor`, `ffprobe`, `web/` (gin API beyond `torrents.go`/`cache.go`/`stream.go` entry points), `settings` (beyond `torrent.go`), `utils` (only `IsVideoFile`/`IsSampleFile` and `FreeSpace` cited, [Torrent pin](torrent-pin.md)).

## Tech
- Go 1.25.7, module `server` (`server/go.mod:1-3`)
- gin (`server/go.mod:24`), anacrolix/torrent via a `tsynik/torrent` replace (source not in local module cache)
- DB: BBolt / JSON via `server/settings`
- Logging: `log.TLogln` (`server/log/log.go:107`)

## How to run
`cd server && go run ./cmd -p 8090 -d <data dir>` (flags `server/cmd/main.go:30-54`). Test gate: [CI / test gate](../../concepts/ci-test-gate.md).

## Notes
- Global `bts *BTServer` is the single torrent registry (`server/torr/apihelper.go:19`).
- Status JSON shared with web: [Torrent status JSON](../../concepts/torrent-status-json.md).
- Startup order: `server.Start` → `go cleanCache()` (`server/server.go:72`, body `:86-137`; with `RemoveCacheOnDrop` keeps pinned DB dirs) → `web.Start` (`:83`) → `BTS.Connect()` (`server/web/server.go:59`) → `torr.StartResumePinned()` (`:64`, loads unfinished `all`/`next` pins now, then every minute, and re-applies loaded pins). `web.Stop` stops the loop before `BTS.Disconnect()` (`:168-169`). See [Torrent storage cache](torrent-storage-cache.md), [Torrent pin](torrent-pin.md).
