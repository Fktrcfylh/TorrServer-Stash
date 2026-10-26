# TorrServer backend

**Type:** core
**Root:** `/server`
**Entry point:** `server/cmd/main.go:64` (`main`) → `server/server.go:23` (`Start`)

## Purpose
Go HTTP server that loads torrents via anacrolix/torrent and streams their files to players over HTTP (`/stream`, `/play`), with a REST API (`POST /torrents`, `/cache`, `/settings`, …) consumed by the web UI, DLNA, tgbot, MCP and torrfs. Torrent pieces are held in a custom storage (`torrstor`) in RAM or on disk. Fork addition: flagged torrents are predownloaded by a Transmission or qBittorrent client on the LAN (NAS) that then serves pieces as a LAN peer.

## Features
- [Torrent lifecycle](torrent-lifecycle.md) — `Torrent` struct, states, watch/expiry/unload, `Status()` and file ids
- [Torrent API helpers](torrent-api-helpers.md) — Get/Load/Add/Rem/Drop/SetSettings, TorrentDB copies, `POST /torrents` actions
- [Remote download](remote-download.md) — NAS predownload: `remotedl` Transmission + qBittorrent clients, `BTSets.RemoteDownloader`, per-torrent `remote_download` flag (`action:"remote"`, `rem` removes on NAS first), NAS peer cache, `remote_*` status cache
- [Streaming](streaming.md) — `Torrent.Stream`, readers, `SetViewed`
- [Preload](preload.md) — pre-playback head/tail buffering
- [Torrent storage cache](torrent-storage-cache.md) — `torrstor` pieces (mem/disk), eviction, priorities, readers, `Close`/`cleanCache`

Not ingested yet: `dlna`, `tgbot`, `mcp`, `torrfs`, `torznab`, `rutor`, `ffprobe`, `gstreamer`, `web/` (gin API beyond `torrents.go`/`settings.go`/`cache.go`/`stream.go` entry points), `settings` (beyond `torrent.go` and the remote downloader part of `btsets.go`), `utils` (only `CompareStrings` cited).

## Tech
- Go 1.25.7, module `server` (`server/go.mod:3`); local toolchain go1.27.3 (brew, `GOTOOLCHAIN=auto`)
- gin (`server/go.mod:23`), anacrolix/torrent via replace `github.com/tsynik/torrent v1.2.34` (`server/go.mod:6`; source in the module cache)
- DB: BBolt / JSON via `server/settings`
- Logging: `log.TLogln` (`server/log/log.go:107`)

## How to run
`cd server && go run ./cmd -p 8090 -d <data dir>` (flags `server/cmd/main.go:28-56`). Test gate: [CI / test gate](../../concepts/ci-test-gate.md). NAS setup: `README.md:5-73`.

## Notes
- Global `bts *BTServer` is the single torrent registry (`server/torr/apihelper.go:19`), set once by `InitApiHelper` (`:25-30`).
- Status JSON shared with web: [Torrent status JSON](../../concepts/torrent-status-json.md).
- Startup order: `server.Start` → `go cleanCache()` (`server/server.go:78`, body `:92-133`) → `web.Start` (`:89`) → `BTS.Connect()` (`server/web/server.go:77`). `web.Stop` ends with `BTS.Disconnect()` (`:264`). See [Torrent storage cache](torrent-storage-cache.md).
- NAS RPC: writes run in the request; status refresh only on web list/get; the peer address (session-get + DNS) is resolved in a background goroutine, TTL 10 min / retry 1 min, while a flagged torrent is loaded or ticked ([Remote download](remote-download.md)).
