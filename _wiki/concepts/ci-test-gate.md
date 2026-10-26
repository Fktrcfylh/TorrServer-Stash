# CI / test gate

Per-module gates for changes in this repo. There is no repo-wide test runner.

## CI reality
- `.github/workflows/ts_build.yml` (manual `workflow_dispatch`, `:4`) cross-compiles `./cmd` with `-tags=nosqlite` (`:66-75`); it runs no `go test` and no web lint. Workflow-level `NODE_OPTIONS: --openssl-legacy-provider` (`:23`).
- The gate below is the local gate used by feature phases (specs/torrent-predownload, specs/nas-predownload).

## Go (`server/`, module `server`, `go 1.25.7` — `server/go.mod:3`)
- Toolchain: Go installed via `brew install go` (go1.27.3, `GOTOOLCHAIN=auto`).
- Gate for touched packages, run from `server/`:
  - `go build ./...`
  - `go vet ./<pkg>/...`
  - `go test ./settings/ ./utils/... ./torr/... ./web/... .` (nas-predownload phases also `./remotedl/`, `./tgbot/...`, `./mcp/...` when touched)
  - race: `go test -race` on touched packages (`./torr/`, `./torr/storage/torrstor/`, `./remotedl/`, `./settings/`, `./web/api/`), repeated (`-count`) for concurrency changes
  - cross-compile at least `CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -tags=nosqlite ./cmd` ("xc arm7"); full target list in `build-all.sh`
  - swagger after API doc changes: `cd server && go run github.com/swaggo/swag/cmd/swag@v1.16.6 init -g web/server.go --parseInternal --parseDepth 5`
- Tests: `_test.go` next to the changed file, success and failure cases; restore every global changed (`settings.BTsets`, `settings.ReadOnly`, `bts` fields, package-var seams); no sleeps as proof of ordering — poll async effects with a deadline; a negative async claim is proven through a synchronous seam or an ordering barrier.
- Logging: `log.TLogln` (`server/log/log.go:107`).

### Shared fixtures
- One settings DB per package in `TestMain` (singleton cannot be reopened after `CloseDB`): `server/torr/dbwrapper_test.go:19`, `server/web/api/torrents_test.go:26`, `server/torr/storage/torrstor/cache_test.go:20`, `server/server_test.go:16`. `settings` tests open a fresh DB per test (`withTestDB` `server/settings/torrent_test.go:12`; `testHash` `:28`, `findRow` `:35`, `legacyPinRow` `:46`).
- `torr`: `withTorrDB` (`server/torr/dbwrapper_test.go:38`, empties the torrents table), `testHash` (`:57`), `dbTorrent` (`:64`); `withOfflineClient` (`server/torr/apihelper_test.go:18`, offline anacrolix client: no DHT/trackers/TCP/uTP), `specWithInfo` (`:39`); `withDiskMode` (`server/torr/torrent_test.go:14`), `expiredTorrent` (`:22`), `loadedTorrent` (`:66`).
- `torrstor`: `withDiskSettings`, `newTestCache`, `testPieceLength` — [Torrent storage cache](../apps/server/torrent-storage-cache.md#tests).
- Root `server`: `withCacheDirs` (`server/server_test.go:51`).
- `web/api`: `withTorrentsAPI` (`server/web/api/torrents_test.go:93`), `addDBTorrent` (`:114`), `postTorrents` (`:126`).

### Remote download fixtures
- `remotedl`: `fakeTransmission` (`server/remotedl/transmission_test.go:25`: 409 session-id handshake, optional Basic auth, scripted `reply`, request/conflict counters, `rotate` id), `newFake` (`:76`), `naSession` (`:21`, recorded NAS session-get reply); `testHashHex` (`server/remotedl/torrents_test.go:15`), `lastRequest` (`:23`), `replyWith` (`:41`), `hashN` (`:185`); qBittorrent: `fakeQB` (`server/remotedl/qbittorrent_test.go:26`, served under a `/qb` path prefix, records an error when a request carries `Referer`/`Origin` `:50-51`), `newQBFake(t, f, user, pass)` (`:161`), `testTorrent` (`:176`).
- `torr`: `withRemote` (`server/torr/remote_test.go:69` — httptest fake + `retrackers` stub + peer/status seams stubbed so their RPCs stay out of recorded calls), `fakeRemote` (`:36`), `flaggedRow` (`:92`), `rowFlag` (`:100`).
- `torr` peer: `peerSpy` (`server/torr/remotepeer_test.go:28`), `lockedPeerSeams` (`:61`), `captureLog`/`syncBuffer` (`:98`, `:106`), `countingResolver` (`:130`), `waitPeer` (`:157`), `cachedPeer` (`:334`), `nasPeer` (`:25`).
- `torr` status: `statusSpy` (`server/torr/remotestatus_test.go:24`, `hold(t, url)` `:55`), `lockedStatusSeams` (`:85`), `resetStatusCacheLocked` (`:101`), `waitRefreshed` (`:114`), consts `nasURL`/`otherURL` (`:126-127`, `.invalid` hosts), `withStatusSpy` (`:131`), `statusFields` (`:139`), `remoteKeys` (`:152`), `stubStatus` (`:162`), `listed` (`:172`).
- `web/api`: `fakeRPC` (`server/web/api/torrents_test.go:183`, counts `gets`, scripted `torrents`/`fail`), `withFakeRPC` (`:226`), `rowFlag` (`:235`), `listItems` (`:357`), `remoteFields` (`:376`), `waitList` (`:387`), `flagRow` (`:402`), `hangingRPC` (`:455`); settings: `fakeRemote` (`server/web/api/settings_test.go:19`), `withSavedRemote` (`:36`), `postSettings` (`:43`), `errorBody` (`:55`); `TestTestRemoteQbittorrent` (`:111`).
- Seams are package vars swapped under their cache mutex (`resolveRemotePeer`, `addPeers`, `fetchRemoteStatus`, `retrackers`, `requestTimeout`, `remote*` timings) — [Remote download](../apps/server/remote-download.md).

### Test gotchas
- **httptest TIME_WAIT:** stress tests use one server + a pooled `http.Transport{MaxIdleConnsPerHost: 64}` (`server/remotedl/transmission_test.go:459`); a server per round exhausted macOS ephemeral ports and then every httptest package failed.
- **Blocking fakes must read `r.Body` first** (e.g. `server/torr/remote_test.go:536`, `server/web/api/torrents_test.go:461`) or `httptest.Server.Close` hangs; release blocking handlers before the test can fail.
- **No real watch loop in `-race` tests:** upstream races (`t.cache` `WaitInfo` vs `expired()`, `Cache.GetState` vs `AdjustRA`) fire; call `progressEvent` / `readdRemotePeer` once directly.
- **web/api tests must not POST `action:get` for a DB torrent:** the async `NewTorrent` races `withTorrentsAPI` cleanup under `-race` → the get-handler refresh trigger is untested.

### Baseline failures (pre-existing; must stay exactly these, verified 2026-10-26 on go1.27.3)
- `server/torr`: `TestStreamBufferMultipartAndHead` always fails — `stream_buffer_test.go:145: multipart: boundary is empty` (fails on pristine upstream too).
- `server/torr`: `go test -race -run TestGotInfoKeepsWorkingStateUnderConcurrentRequests` (`server/torr/gotinfo_race_test.go:75`) flaky (orchestrator: 2/3 failures on pristine upstream).
- `server/settings`: `go vet ./settings/` — `settings/viewed.go:88:2: unreachable code`.
- `server/rutor`: `TestParseChannel`, `TestParseArr` **skip** without the runtime-downloaded `rutor/rutor.ls` (`server/rutor/mem_test.go:21-23`); no longer failures.
- Gone on the branch: `web/api/utils` vet failure (`link.go` fixed upstream; `go vet ./web/api/utils/` clean) and the `-race ./torr/utils/` tracker-refresh failures (pass).

## Web (`web/`, React 17, CRA `react-scripts` 4.0.3 — `web/package.json:18`, `:26`)
- Gate: `cd web && export NODE_OPTIONS=--openssl-legacy-provider && yarn install --frozen-lockfile && yarn lint && yarn build`.
- `lint` = `eslint --ext .js,.jsx src --color` (`web/package.json:36`). Baseline 0 errors / 0 warnings.
- `yarn.lock` must stay unchanged; no new npm dependency in feature phases.
- Locale check: every `web/src/locales/*/translation.json` valid JSON with the same new keys as `en` (node one-liner).
- No web unit tests (mission decision); behaviour verified by a headless-browser live smoke.
- **Embedded pages not regenerated:** `server/web/pages/template/*` is not rebuilt or staged in feature PRs; maintainer publishes "update static web" commits. Do not stage `web/build`.

## Live smoke
- `cd server && go run ./cmd -p <port> -d <scratch data dir>` (`-p` `server/cmd/main.go:29`, `-d` `:35`). Never point `-d` at a real config. Docker is not used.
- **Web smoke recipe** (nas-predownload Phase 6): scratch Go server from the branch + node proxy serving `web/build` statically and proxying the API + python fake Transmission; `playwright-core@1.55.0` with a cached `chrome-headless-shell`, installed in the scratchpad only. Gotchas: sidebar is collapsed at 1400 px width (settings icon reached by mouse position); controlled MUI checkboxes need `.click()`; close the details dialog with `button[aria-label=close]`; the NAS checkbox follows the poll — poll `isChecked`.
- **NAS live script recipe** (Phase 7): shell driver starts the scratch server + proxy + headless UI flow; the UI script may only **read** the NAS (allow-list guard: `session-get`, `torrent-get`); every NAS write goes through TorrServer tick/untick; refuses to run when the test torrent is already on the NAS; `DRY=1` runs the same flow against a python fake into a separate output dir; a rollback script removes only the test hash with data if `torrent-get` finds it. Real NAS runs only after explicit human approval. Unreachable URL timings: `test_remote` ~13 s, untick 502 `context deadline exceeded`.
- Scratchpad paths are session-local; rebuild the scripts from this recipe.

## Related
- [Wiki index](../index.md), [server overview](../apps/server/_overview.md), [web overview](../apps/web/_overview.md)
