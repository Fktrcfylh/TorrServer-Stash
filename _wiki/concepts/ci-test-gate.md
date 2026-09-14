# CI / test gate

Per-module gates for changes in this repo. There is no repo-wide test runner.

## CI reality
- `.github/workflows/ts_build.yml` (manual `workflow_dispatch`) cross-compiles `./cmd` with `-tags=nosqlite` (`.github/workflows/ts_build.yml:66`); it runs no `go test` and no web lint. Workflow-level `NODE_OPTIONS: --openssl-legacy-provider` (`:23`), Node 20 (`:249-252`).
- The gate below is the local gate used by feature phases (specs/torrent-predownload).

## Go (`server/`, module `server`, Go 1.25.7 — `server/go.mod:3`)
- Gate for touched packages, run from `server/`:
  - `go build ./...`
  - `go vet ./<pkg>/...`
  - `go test ./<pkg>/...`
  - race-sensitive packages `torr`, `torr/storage/torrstor`: also `go test -race ./torr/ ./torr/storage/torrstor/`
  - `cleanCache` changes: root package `server` — `go vet .`, `go test .` (from `server/`)
  - `web/server.go` changes: `go test ./web/...` as compile check (baseline `web/api/utils` failure stays)
  - Phase 3 gate (specs/torrent-predownload): `go build ./...`; `go vet ./torr/... .`; `go test ./settings/ ./torr/... . ./web/...`; `go test -race ./torr/ ./torr/storage/torrstor/`
  - Phase 4 gate: `go build ./...`; `go vet ./torr/... ./utils/... ./mcp/... ./tgbot/... .`; `go test ./settings/ ./utils/... ./mcp/... ./torr/... ./web/... ./tgbot/... .`; `go test -race ./torr/ ./torr/storage/torrstor/`
  - Phase 5 gate (all OK): `go build ./...`; `go vet ./torr/... ./utils/... .` (`go vet ./settings/` keeps its baseline failure); `go test ./settings/ ./utils/... ./torr/... ./web/... .`; `go test -race ./torr/ ./torr/storage/torrstor/`; cross-compile `CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> [GOARM|GOMIPS] go build -tags=nosqlite ./cmd` for every `build-all.sh` target (linux amd64/arm64/arm 5,7/386/mips/mipsle/mips64/mips64le/riscv64, windows amd64/386, darwin amd64/arm64, freebsd amd64/arm7); `GOOS=android GOARCH=arm64` and `GOOS=ios GOARCH=arm64` `go build ./utils/` OK (ios `go vet` needs `CGO_ENABLED=1`). Needed because `utils.FreeSpace` has per-OS build files (`server/utils/freespace_{unix,windows,other}.go`).
- Tests: `_test.go` next to the changed file, success and failure cases; restore every global changed (`settings.BTsets`, `settings.ReadOnly`, `bts` fields, `torr.freeSpace`); no sleeps as proof of ordering — poll async effects with a deadline. Race tests: `server/torr/btserver_race_test.go`, `server/torr/storage/torrstor/race_test.go`, `TestSetPinConcurrent` (`server/torr/storage/torrstor/cache_test.go:704`), `TestPinConcurrentStatusDownloadedApply` (`server/torr/pin_test.go:945`). A negative async claim is proven through a synchronous seam (e.g. `pinnedToResume`, `updatePinAnchor`'s bool return, direct `dropPieces`/`savePinAnchor`/`updatePinDownloaded`/`applyPin`/`resumePinned` calls) or an ordering barrier, not a sleep. Blocked-lock orderings: `runBlockedDrop` (`server/torr/storage/torrstor/cache_test.go:611`) waits until the goroutine stack shows `dropPieces` in `Mutex.Lock`.
- Shared fixtures: `torr`, `web/api`, `torr/storage/torrstor` and root `server` tests open one settings DB per package in `TestMain` (`server/torr/dbwrapper_test.go:19`, `server/web/api/torrents_test.go:21`, `server/torr/storage/torrstor/cache_test.go:25`, `server/server_test.go:16`) — the settings DB singleton cannot be reopened after `CloseDB`; tests empty the torrents table per test (`withTorrDB` `server/torr/dbwrapper_test.go:39`, which also empties the in-memory no-space set before and after, `clearPinNoSpace` `:54`). `settings` tests open a fresh DB per test (`withTestDB` `server/settings/torrent_test.go:13`). `torr` tests needing `NewTorrent` use an offline anacrolix client (no DHT/trackers/TCP/uTP) and specs with info bytes (`withOfflineClient` `server/torr/apihelper_test.go:19`, `specWithInfo` `:40`); DB rows for resume tests carry a `specWithInfo` spec so the async load needs no peers (`dbRow` `server/torr/resume_test.go:18`). `server/torr/torrent_test.go` reuses them with `withDiskMode` (`:17`), which also installs a `freeSpace` double with 1 PiB for the save path. **`freeSpace` double:** `withFreeSpace(t, map[path]bytes)` (`server/torr/torrent_test.go:33`) replaces the `torr.freeSpace` package var with a map-backed func (unknown path → `0, false`), records called paths (`calledWith` `:54`), `set` (`:48`) changes a value, restored in cleanup. Multi-file layouts in `torr`: `episodesSpec` (`server/torr/pin_test.go:104`, 5 `.mkv` files, piece length 16 bytes, 10 pieces, zero hashes), `episodesTorrent` (`:121`, loaded on the offline client with DB row and pre-written piece files, no watch loop) and `episodesData` (`:560`, matching TorrServer `Data` list for stubs); async priority effects polled by `waitWanted` (`:162`). **`completePieces`** (`server/torr/pin_test.go:411`) makes pieces complete for anacrolix too: writes the full piece through the cache, `MarkComplete`, then `Torrent.Piece(i).UpdateCompletion()` so `PieceState(i).Complete` and `File.State()` see it. `pausedTorrent` (`:764`) builds a paused `next` pin through the double. `torrstor` tests (`cache_test.go`, `diskpiece_test.go`) build their own real offline anacrolix client whose `DefaultStorage` is a `torrstor.Storage`, on disk settings: `withDiskSettings` (`server/torr/storage/torrstor/cache_test.go:43`, `TorrentsSavePath=t.TempDir()`, restores `BTsets`), `newPinTestCache` (`:76`) → `newPinSetTestCache` (`:88`, explicit `want`/`drop`) → `newPinFilesTestCache` (`:95`, consecutive files of N pieces), `withTorrentRow` (`:742`), `openWithPieceFiles` (`server/torr/storage/torrstor/diskpiece_test.go:16`); the `torrstor` `TestMain` restores `Path/ReadOnly/SearchWA/BTsets` right after `InitSets`, so `race_test.go` keeps zeroed memory-mode `BTsets`. Root `server` tests: `withCacheDirs` (`server/server_test.go:51`, disk `BTsets` + `t.TempDir()` save path, pinned/unpinned rows, empties the torrents table in cleanup). See [Torrent storage cache](../apps/server/torrent-storage-cache.md#tests).
- **Resume-loop cleanup rule:** a test that calls `torr.StartResumePinned` registers `t.Cleanup(StopResumePinned)` after the fixture cleanups (`withTorrDB`/`withDiskMode`/`withOfflineClient`), so it runs first and no loop goroutine outlives the test or reads restored globals (`server/torr/resume_test.go:106`).
- Logging: `log.TLogln` (`server/log/log.go:107`).

### Baseline failures (pre-existing; must stay exactly these)
- `server/rutor`: `TestParseChannel`, `TestParseArr` — missing fixture `rutor/rutor.ls`.
- `server/web/api/utils`: package fails under `go test` vet step — `link.go:74:39 fmt.Errorf call has arguments but no formatting directives`.
- `server/settings`: `go vet ./settings/` — `settings/viewed.go:88:2: unreachable code` (file untouched by pin Phases 1–5; not in the Phase 4/5 vet gate).
- `server/torr/utils`: `go test -race ./torr/utils/` — `TestTrackersPeriodicRefresh`, `TestTrackersRefreshFallsBackToNextURL`, `TestTrackersRefreshAllFailKeepsCache` (not in the Phase 3/4/5 `-race` gate).

## Web (`web/`, React 17, CRA `react-scripts` 4.0.3 — `web/package.json:18`, `web/package.json:26`)
- Gate: `cd web && export NODE_OPTIONS=--openssl-legacy-provider && yarn install --frozen-lockfile && yarn lint && yarn build`. On Node ≥17 `yarn build` fails without `--openssl-legacy-provider` (webpack 4 in CRA 4); CI sets it at `.github/workflows/ts_build.yml:23`.
- `lint` = `eslint --ext .js,.jsx src --color` (`web/package.json:36`). Baseline after Phase 6: 0 errors / 0 warnings — any new warning is a regression.
- `yarn.lock` must stay unchanged (`--frozen-lockfile`); no new npm dependency in feature phases.
- Locale check: every `web/src/locales/*/translation.json` stays valid JSON with the same new keys as `en` (Phase 6 gate used a node one-liner).
- No web unit tests (torrent-predownload mission decision); behaviour verified by a headless-browser live smoke (below).
- **Embedded pages not regenerated:** `server/web/pages/template/*` (built `web/build` embedded in Go) is not rebuilt or staged in feature PRs; maintainer publishes separate "update static web" commits (e.g. `309a5917`). Do not stage `web/build`.

## Live smoke
- `cd server && go run ./cmd -p <port> -d <scratch data dir>` — flags `-p` port, `-d` DB/config dir (`server/cmd/main.go:30`, `server/cmd/main.go:36`). Never point `-d` at a real config. Docker is not used.
- Web changes (Phase 6 pattern): serve `yarn build` output from a scratchpad node proxy (static `web/build`, other paths proxied to the Go server) and drive it with a scratchpad Playwright/puppeteer project; screenshots in the scratchpad. Tracked embedded pages untouched.

## Related
- [Wiki index](../index.md), [server overview](../apps/server/_overview.md), [web overview](../apps/web/_overview.md)
