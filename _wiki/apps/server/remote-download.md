# Remote download (NAS predownload)

**App:** server
**Status:** stable
**Last reviewed:** 2026-10-26 (commit `8fdc472f`, specs/nas-predownload Phases 1–8)

## Summary
A per-torrent flag hands the whole torrent to a Transmission or qBittorrent client on the LAN (NAS). Tick → synchronous add; untick or `rem` → synchronous remove with data. While a flagged torrent plays, TorrServer gets pieces from the NAS as a LAN peer (in practice through the client's LPD). The status JSON carries the NAS state/progress from a cache refreshed only by web list/get. Replaces the removed disk pin (`specs/torrent-predownload`, deleted in `763a4204`). User docs (setup, per-client requirements, expectations): `README.md:5-73` — not repeated here. Web UI: [NAS settings](../web/remote-downloader-settings.md), [Torrent details dialog](../web/torrent-details-dialog.md#nas-block-torrentfunctions).

## Entry points
- `server/web/api/torrents.go:96-99` — `POST /torrents {action:"remote", hash, remote_download}` → `remoteTorrent` (`:282-297`) → `torr.SetTorrentRemote`
- `server/web/api/torrents.go:212-215`, `:260` — `rem` / `wipe` → `torr.RemTorrent` (returns error)
- `server/web/api/settings.go:58-60` — `POST /settings {action:"test_remote", remote_downloader?}` → `testRemote` (`:72-93`)
- `server/web/api/torrents.go:189-191`, `:238-240` — `get` / `list` call `torr.RefreshRemoteStatus()` when a returned torrent is flagged
- `server/torr/torrent.go:281-283` — `progressEvent` re-adds the NAS peer for flagged torrents
- tgbot `server/tgbot/delete.go:21`, `:47`, `server/tgbot/callbacks_torrent.go:18-22`, `server/tgbot/remove.go:32-34`; MCP `server/mcp/tools.go:438-440` — surface the `RemTorrent` error

## Key files
| File | Role |
|---|---|
| `server/remotedl/downloader.go` | `Downloader` interface, types, `New` factory, `Shared` cache, `ErrDisabled`/`ErrUnauthorized` |
| `server/remotedl/transmission.go` | Legacy Transmission RPC client |
| `server/remotedl/qbittorrent.go` | qBittorrent Web API v2 client (Phase 8) |
| `server/torr/remote.go` | `SetTorrentRemote`, `removeRemote`, `remoteAddRequest`, lock stripes, sentinel errors |
| `server/torr/remotepeer.go` | NAS peer address cache + `AddPeers` |
| `server/torr/remotestatus.go` | Remote status snapshot for `Status()` |
| `server/settings/btsets.go` | `RemoteDownloaderConfig` (`:38-44`), `BTSets.RemoteDownloader` (`:86`), `String()` mask (`:129-136`) |
| `server/settings/torrent.go` | `TorrentDB.RemoteDownload` (`:25-27`), `SetTorrentRemoteDownload` (`:54-69`) |
| `server/web/api/torrents.go`, `settings.go` | REST actions + error → HTTP mapping |
| Tests | `server/remotedl/{downloader,transmission,torrents,qbittorrent}_test.go`, `server/torr/{remote,remotepeer,remotestatus}_test.go`, `server/web/api/{torrents,settings}_test.go`, `server/settings/{btsets,torrent}_test.go` — fixtures in [CI / test gate](../../concepts/ci-test-gate.md#remote-download-fixtures) |

## Settings
- `RemoteDownloaderConfig{Type, URL, Username, Password, DownloadDir}` (`server/settings/btsets.go:38-44`), no json tags → PascalCase keys in `settings get/set`. Swagger shows them camelCase (`server/docs/swagger.yaml:344-360`); runtime JSON is PascalCase.
- Field comments cover both types (`:39-42`): Transmission RPC URL or qBittorrent Web UI URL; user/password = Transmission Basic auth or qB Web UI login.
- Empty URL = feature off. `DefaultRemoteDownloaderType = "transmission"` (`:47`); empty `Type` → transmission in `SetBTSets` (`:192-194`), `SetDefaultConfig` (`:250`), `loadBTSets` (`:280-282`).
- `(*BTSets).String()` masks `Password` as `***` (`:129-136`); it is what `log.Println("Client config:", settings.BTsets)` prints (`server/torr/btserver.go:159`). `settings get` still returns the password (mission: masking there out of scope).
- `sets.BTsets` is swapped by `SetBTSets` unsynchronised and read unlocked everywhere (pre-existing convention); remote code reads `sets.BTsets.RemoteDownloader` live.
- TorrServer's own LPD (`BTsets.EnableLPD`, default on `server/settings/btsets.go:239`, wired at `server/torr/btserver.go:91-97`) must stay on for the NAS to find it.

## remotedl package
- `Downloader` (`server/remotedl/downloader.go:70-83`): `Session(ctx) SessionInfo{version,rpc_version,peer_port,lpd_enabled}` (`:24-29`; `rpc_version` 0 for qBittorrent); `Add(ctx, AddRequest{Metainfo|Magnet, DownloadDir}) AddResult{Hash,Name,Duplicate}` (`:33-45`); `Remove(ctx, hash)`; `Status(ctx, hashes) map[lowercase hash]RemoteStatus{State,Progress,Error}` (`:63-67`). States `downloading/seeding/queued/stopped/checking/error/missing` (`:48-57`).
- `New(cfg)` (`:110-139`): trimmed empty URL → `(nil, ErrDisabled)`; parse error strips the raw URL (may carry credentials); userinfo in URL rejected (`:125-127`); scheme must be http(s) with host (`:128-130`); `Type` `transmission` / `qbittorrent`, else rejected (`:131-138`). Used directly only by `test_remote` — a fresh client per test, so qB's rejected-credentials guard does not span tests (repeated wrong-credential tests can get the IP banned, README `:47-49`).
- `Shared(cfg)` (`:94-106`): one cached client while `cfg` is equal (keeps the Transmission session id / qB cookie), rebuilt on any field change; `New` errors not cached. Used by `SetTorrentRemote`, `removeRemote`, peer resolve and status refresh.

### Transmission client
`server/remotedl/transmission.go`:
- Legacy dialect: envelope `{method, arguments}`, kebab-case keys, rpc-version 15+ (`:36-37`, `:61-76`). Live NAS: Transmission 2.82, RPC 15.
- `call` (`:281-338`): resends while each 409 carries a **new** `X-Transmission-Session-Id`, at most `maxAttempts=3` HTTP requests (`:29`, `:292-313`); stops on 409 without header or with the id just sent. Id cache updated by `replaceSessionID(sent, next)` CAS under `mu` (`:269-275`) — a concurrent caller's newer id is kept (Phase 2 R1 fix of a real re-handshake race).
- 401 → `ErrUnauthorized`, never retried (`:316-317`); other non-200 (403 rpc-whitelist, 421, …) → `HTTP <n>: <≤200-byte body excerpt>` (`:318-319`, `:26`, `:371-377`); `result != "success"` → error (`:326-331`).
- `post` (`:342-368`): `requestTimeout` 10 s per HTTP request (package var `:34`), Basic auth when user or password set (`:354-356`), body read capped at 8 MiB (`:24`, `:363`).
- `Session` (`:79-94`): `lpd-enabled=false` → one warning log per client instance (`lpdWarned atomic.Bool` `:49`, `:90-92`).
- `Add` (`:114-134`): exactly one of metainfo (base64) / magnet (`filename`) else error; `torrent-duplicate` → `Duplicate: true` success (`:129-130`).
- `Remove` (`:144-150`): refuses anything but 40 hex chars **before** building a request (empty `ids` would remove every torrent); lowercases; always `delete-local-data: true`.
- `Status` (`:187-228`): empty list → no request (no `ids` = all torrents); invalid hash refused; ids lowercased/deduped, all pre-set `missing`; one `torrent-get` with `hashString,status,percentDone,error,errorString` (`:168`). Map `transmissionState` (`:230-245`): 0 stopped, 1/2 checking, 3/5 queued, 4 downloading, 6 seeding, other → `error` + "unknown transmission status". `error != 0` → `Error = errorString`; only `error == 3` (local error, `:182`) forces `State=error` (tracker warnings keep the state). `Progress = round(percentDone×1000)/10`.

### qBittorrent client
`server/remotedl/qbittorrent.go` (Phase 8, Web API v2, 4.1+):
- Factory: `Type == qbittorrentType` (`"qbittorrent"`, unexported const `:26`) → `newQbittorrent` (`server/remotedl/downloader.go:134-135`). URL = Web UI base, optional path prefix (reverse proxy); every call goes to `<base>/api/v2/<path>` (`send` `:308-333`, same `requestTimeout` and 8 MiB cap).
- Auth (`request` `:251-273`, `login` `:279-304`): cookie jar session (`:53-61`), no pre-login. 403 with credentials → one `auth/login` + resend; 403 again → `ErrUnauthorized`; 403 without credentials → `ErrUnauthorized` + "set the user and password, or allow this host without login" hint. Login `Fails.` → `loginRejected` (`:45-48`): this client never sends those credentials again (qB bans the IP after 5 failures for 1 h); login 403 → "IP banned?" error. Logins serialised by `loginMu` (`:43-44`).
- No `Referer`/`Origin` header (`:32-36`): qB's CSRF check passes header-less requests; a Referer from the configured URL would fail behind a Host-rewriting proxy. Per the Phase 8 agent (qB behaviour, not in code): a foreign Referer → 401 (CSRF/host check), 403 = no session; verified live with CSRF protection on.
- `Session` (`:70-92`): `app/version` + `app/preferences` (`listen_port` → `PeerPort`, `lsd` → `LPDEnabled`); `RPCVersion` stays 0; `lsd=false` → one warning per client (`lpdWarned` `:50`, `:88-90`).
- `Add` (`:97-148`): exactly one of metainfo/magnet; multipart `torrents` file or `urls` magnet, `savepath` only when `DownloadDir` set; no paused/tags/category/sequential. Hash and name computed locally (`addTarget` `:152-169`; the API reports neither). `Ok.` → added; `Fails.` → `torrents/info` for the hash: present = `Duplicate`, absent = error "invalid torrent or magnet?" (`:132-146`). Other non-200 (e.g. 415) → `HTTP <n>: <excerpt>`.
- `Remove` (`:174-181`): `torrents/delete` `hashes=<40-hex>&deleteFiles=true`; `isInfoHash` refuses empty, `all`, `|`-lists and non-hex (`all` deletes everything). qB answers 200 for an unknown hash.
- `Status` (`:191-228`): empty list → no request; one `torrents/info?hashes=a|b`, ids lowercased/deduped, absent → `missing`; `Progress = round(progress×1000)/10`. `qbittorrentState` (`:232-247`): `uploading/stalledUP/forcedUP` → seeding; `downloading/stalledDL/metaDL/forcedMetaDL/forcedDL/allocating` → downloading; `pausedUP/pausedDL/stoppedUP/stoppedDL` → stopped; `queuedUP/queuedDL` → queued; `checkingUP/checkingDL/checkingResumeData/moving` → checking; else → error with `Error = qBittorrent state "<s>"`.
- Layers above `remotedl` (flag, peer, status cache, REST, web) are client-agnostic and unchanged by Phase 8.

## Per-torrent flag
- Owner: DB row `TorrentDB.RemoteDownload` `remote_download,omitempty` (`server/settings/torrent.go:25-27`). `settings.AddTorrent` copies the **stored** value over the caller's (`:41-44`), so general saves never change it; only `SetTorrentRemoteDownload(hash, on) bool` (`:54-69`, false = no row) writes it; `getTorrentRow` (`:72-82`) reads one row with `mu` held.
- Mirror: `Torrent.RemoteDownload` under `muTorrent` (`server/torr/torrent.go:36-38`) — display only, decisions read the row. Copied in `AddTorrentDB` (`server/torr/dbwrapper.go:55-58`, new rows only), `GetTorrentDB` (`:75`), `ListTorrentsDB` (`:99`), `LoadTorrent` (`server/torr/apihelper.go:41`, before `WaitInfo`), `AddTorrent` (`:91-94`, from DB row), `GetTorrent` async load (`:129`). May lag a concurrent toggle.

## Flow — SetTorrentRemote
`SetTorrentRemote(hashHex, on)` — `server/torr/remote.go:80-162`:
1. `ReadOnly` → `ErrReadOnly` (`:81-84`); bad hex → `ErrTorrentNotFound` (`:85-89`).
2. `lockRemote(hash)` for the whole call (`:91`).
3. No row: not loaded → `ErrTorrentNotFound`; loaded without info → `ErrInfoPending` (`AddTorrentDB` needs size/files, `:100-103`); else `AddTorrentDB` and re-read (`:104-107`).
4. `off` on an unflagged row → no RPC, returns status (`:110-114`) — never touches client torrents not flagged here.
5. `remoteDownloader()` → `Shared(BTsets.RemoteDownloader)`; any config error wrapped in `ErrRemoteConfig` (`:69-75`, `:116-120`). Context `remoteTimeout` 30 s for all requests (`:33`, `:121`).
6. `on`: `remoteAddRequest` + `DownloadDir` (`:125-130`) → `Add`; duplicate is success; client hash ≠ ours → warning log only (`:137-139`). `off`: `Remove(hash)` (`:141-145`).
7. Only after RPC success: `SetTorrentRemoteDownload` (`:148-150`); `on` → `invalidateRemoteStatus()` (`:152-154`); loaded torrent → mirror + `addRemotePeer()` on `on` (`:155-160`). RPC failure → error, flag unchanged.

## Payload — remoteAddRequest
`server/torr/remote.go:197-244`:
- Info bytes from the row spec, else from the loaded torrent's `Metainfo()` (`:198-210`).
- `metainfo.HashBytes(info) != hash` → refuse (would add another torrent, `:219-221`); else bencoded `.torrent` with `AnnounceList` = original trackers, `Announce` = first (`:222-230`).
- No info bytes → magnet with hash, `dn` (spec `DisplayName`, else row title) and original trackers (`:233-243`).
- `originalTrackers` (`:253-274`) drops every tracker in `retrackers()` = `GetDefTrackers()` + `GetTrackerFromFile()` (`:248-250`, package var stubbed in tests), duplicates and empty tiers. A torrent's own tracker that is also in the default list (e.g. opentrackr) is stripped too.

## Flow — RemTorrent
`RemTorrent(hashHex) error` — `server/torr/apihelper.go:181-231`:
1. `ReadOnly` → log, `nil` (`:182-185`).
2. `lockRemote(hash)` held for the whole removal (`:188`).
3. `removeRemote(hash)` (`server/torr/remote.go:173-192`): row missing or unflagged → nil; else `Shared` + `Remove` under 30 s. Error (config or RPC) → returned, **memory, files and row kept** (`server/torr/apihelper.go:189-191`).
4. Then the pre-pin order: not loaded → DB row + `TorrentsSavePath/<hash>` (`:194-203`); loaded → `bts.RemoveTorrent`, wait `closed` ≤ 5 s, remove dir (`:205-226`); DB row last (`:229`). See [Torrent API helpers](torrent-api-helpers.md#flow--remtorrent).

## Locks
- `remoteLocks [64]sync.Mutex`, stripe `hash[0] % 64` (`server/torr/remote.go:41-47`): serialises tick/untick/`rem` of one torrent so client and flag never diverge; fixed size (request hashes never grow it); unrelated torrents share a stripe 1 in 64 and then wait on its RPC.
- Order: per-hash remote lock → `bt.mu` / `muTorrent` / `settings.mu`. `GetTorrent` (stream path) never takes it.
- `remotePeers.mu` and `remoteStatuses.mu` are leaf locks; `setRemoteStatus` runs inside `Status()` under `muTorrent` (`server/torr/torrent.go:479-481`).

## REST mapping
`server/web/api/torrents.go`:
- `torrReqJS.RemoteDownload *bool` (`:33-36`): missing key → 400 `remote_download is required` (`:287-290`); empty hash → 400.
- `remoteErrorStatus` (`:301-314`): `ErrRemoteConfig` 400, `ErrTorrentNotFound` 404, `ErrReadOnly` 403, `ErrInfoPending` 409, anything else (RPC) 502. Used by `remote` and `rem` (`:212-215`).
- Success → `200` + `Status()` (`:296`).
- `wipe` (`:254-280`): continues past failures; any → 502 `"<n> torrent(s) not removed: <first error>"` (`:274-277`).
- `test_remote` (`server/web/api/settings.go:72-93`): body `remote_downloader` else saved config; never saves or restarts; 200 `SessionInfo`, 400 config error (URL not logged), 502 RPC error. Unreachable host ≈ 13 s before 502 (live).

## NAS peer
`server/torr/remotepeer.go`:
- Address = `url.Hostname()` of the RPC URL via `net.DefaultResolver.LookupIP`, IPv4 preferred, + `Session().PeerPort` (`:53-81`). Resolved in the background and cached so adding a peer never waits on RPC/DNS.
- `peerCache{cfg, gen, peer, ok, next, resolving, failing}` (`:37-46`) keyed by the whole `RemoteDownloaderConfig`; config change resets and bumps `gen`, late results of an older `gen` dropped (`:103-106`, `:123-125`).
- `remotePeer()` (`:92-112`) non-blocking: missing/stale → one background `resolve`; serves the stale address while refreshing.
- Package vars (`:23-34`): TTL 10 min, retry 1 min after failure, resolve timeout 15 s, re-add 30 s. Failure logged once until a success; `remote peer: ip:port` logged on change (`:129-137`).
- `addRemotePeer` (`:143-156`): flagged + loaded + cached → `addPeers` (`lt.AddPeers`, seam `:85-87`). Callers: `SetTorrentRemote(on)` after save; `readdRemotePeer` (`:160-173`) from `progressEvent` for flagged torrents only (`server/torr/torrent.go:277-283`), claims `remotePeerAt`, restores it if nothing was added (retries every tick until cached). No hook in `NewTorrent`; `LoadTorrent` sets the mirror before `WaitInfo` so the peer is added while waiting.
- Fork API (`github.com/tsynik/torrent v1.2.34`, `server/go.mod:6`): `Torrent.AddPeers([]Peer)` (`t.go:209`), `Client.OnLPDAnnouncement` (`client.go:100`).

## Status cache
`server/torr/remotestatus.go`:
- `Status()` only reads a snapshot: `setRemoteStatus` (`:158-172`) ← `cachedRemoteStatus` (`:89-103`), for flagged torrents with a hash (`server/torr/torrent.go:479-481`). No RPC on the stream path or any other `Status()` caller.
- `RefreshRemoteStatus()` (`:71-84`) — called only by web `getTorrent` / `listTorrents` when a returned item is flagged (`server/web/api/torrents.go:189-191`, `:238-240`); never blocks; starts one goroutine when the snapshot is older than `remoteStatusTTL` 5 s (also the pause after a failure, `:24`) and none of the current `gen` is running (`running`, `:80-83`).
- `refresh` (`:105-143`): reads flagged rows from `sets.ListTorrent()`, one batch `Status` with `remoteStatusTimeout` 15 s; result dropped if `gen` changed. Failure → every flagged torrent shows the error; logged once, recovery logged.
- `gen` bumped by `syncConfigLocked` on config change (`:61-66`, drops snapshot) and `invalidateRemoteStatus()` (`:147-153`, keeps snapshot until next refresh) after a successful tick.
- No loop: nothing refreshes while no web client lists a flagged torrent; tgbot/MCP see the last snapshot. JSON contract: [Torrent status JSON](../../concepts/torrent-status-json.md#remote-fields).

## Dependencies
- Overview: [server](_overview.md)
- Features: [Torrent API helpers](torrent-api-helpers.md) (`RemTorrent`, `TorrentDB` copies), [Torrent lifecycle](torrent-lifecycle.md) (`progressEvent`, `Status()`), [Streaming](streaming.md) (RPC-free stream path)
- Web: [NAS settings](../web/remote-downloader-settings.md), [Torrent details dialog](../web/torrent-details-dialog.md)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md), [CI / test gate](../../concepts/ci-test-gate.md)

## Gotchas / decisions
- **All client writes synchronous, no reconcile loop** (human decision). API error = nothing changed in TorrServer.
- **Duplicates are ours:** a torrent already in the client before the tick is removed with its data on untick/`rem` (README `:69-71`).
- **Untick and `rem` delete NAS data** (Transmission `delete-local-data`, qB `deleteFiles`, always true).
- **Unflag needs a configured downloader:** URL cleared while rows are flagged → untick/`rem` return 400 (`ErrRemoteConfig`) and keep the torrent; reconfigure first (README `:72-73`).
- **Peer injection is best-effort; LPD is the working channel.** Live (Phase 4): with NAS `lpd-enabled` off, 0 sockets to the NAS during a stream — fork BEP40 queue + `wantConns` filled 25/25 within seconds, 458 pending, NAS never dialed. With NAS LPD on, the NAS dialed TorrServer within 10 s. `AddPeers` kept (helps idle/small swarms). README makes NAS LPD a requirement.
- **LPD reaches unflagged torrents too:** fork `OnLPDAnnouncement` adds the announcer to every non-private torrent, not only announced hashes (`client.go:96-124` in the fork), and the NAS dials TorrServer for any torrent it seeds.
- **Flag off keeps an existing NAS connection**; only re-adds stop.
- **NAS gets a fair share:** one peer among all connected ones; no prefer-local knob (anacrolix issue #894). Live: `rateToPeer` 31–87 KB/s at a 1.5 MB/s stream with ~25 peers.
- **Proxy:** `--proxymode full|peers` routes the LAN dial via the proxy (`server/torr/btserver.go:235-254`); default `tracker` mode does not.
- `torrent.Peer.Source` is left zero (fork `peerSource` is unexported).
- qBittorrent: no hash comparison of the added torrent (hash computed locally from the payload); `Duplicate` relies on `torrents/info` after `Fails.`.
- Transmission 2.82 dropped the `wss://` tracker from the `.torrent` and kept only udp (live Phase 3).
- `ErrInfoPending` only for a loaded torrent **without** a row; a row with info bytes or a magnet fallback never needs info.
- Secrets: `New` never echoes a URL with credentials; `test_remote` logs no URL on config error; `String()` masks the password.

## Live results (2026-10-26; real NAS for 2–7, local qBittorrent for 8; human-approved)
| Phase | Result |
|---|---|
| 2 | `test_remote` 200: `2.82 (14160)`, rpc 15, peer port 51413, lpd false; unconfigured 400 |
| 3 | Tick BBB → NAS torrent in configured dir, trackers only udp; untick → removed with data; `rem` 200 |
| 4 | NAS seeded BBB 276 MB in ~2 min; LPD-on stream: NAS dialed in 10 s (see gotchas) |
| 5 | Unflagged list has no `remote_*` keys; fake client 0→21 %; unreachable → `error`; stream without list → 0 `torrent-get` |
| 7 | UI tick → seeding in 220 s, badge `NAS 100%`, stream 276 MB, NAS `rateToPeer` ≤ 65 KB/s; unreachable URL → 502 + UI error; untick → NAS count back; `rem` of unflagged → no RPC. 30/31 checks PASS, 1 non-must lost (likely poll timing) |
| 8 | qBittorrent 4.5.0 (local): `test_remote` `v4.5.0`, rpc 0, peer port 52255, lpd true; tick refused twice with "info pending" (409) then added (`stalledDL`); progress 2.3→66.7 %; untick removed with data, client count back to its start; log `remote peer: 127.0.0.1:52255` |

## Open questions
- Phase 7 real-run evidence was overwritten by a dry run; the failed non-must check is unidentified.
- Upload slots/choking on the NAS while it is still downloading limit what TorrServer gets (README `:63-65`); not measured beyond the live numbers above.
- `GET`-handler status refresh (`server/web/api/torrents.go:189-191`) has no unit test (async `NewTorrent` races test cleanup under `-race`).
