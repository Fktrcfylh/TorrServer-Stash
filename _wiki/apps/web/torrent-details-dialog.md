# Torrent details dialog

**App:** web
**Status:** stable
**Last reviewed:** 2026-09-14 (staged Phases 1–6 diff on `215f6fca`)

## Summary
Full-screen dialog opened from a torrent card. Shows poster/title, speed/peers/size/status/category widgets, torrent actions (playlist, predownload pin block, remove views, drop, copy magnet), a buffer progress bar with a mini cache map, and a table of playable files with per-file preload/play/link buttons, "viewed" markers and a predownload "Disk" column. A second view shows the full piece-level cache map. Predownload UI (specs/torrent-predownload Phase 6) also adds a progress/error badge on the torrent card and a `DefaultPinNext` settings field; server side: [Torrent pin](../server/torrent-pin.md).

## Entry points
- `web/src/components/TorrentCard/index.jsx:396` — "Details" button → `openDetailedInfo` (`:187`).
- `web/src/components/TorrentCard/index.jsx:574-584` — `StyledDialog` renders `<DialogTorrentDetailsContent closeDialog torrent={torrent} />`.
- `web/src/components/App/index.jsx:49-54` — `useQuery('torrents', getTorrents, { refetchInterval: 1000 })`; `getTorrents` = `POST /torrents {action:'list'}` (`web/src/utils/Utils.js:61-68`). Data flows `App` → `TorrentList` (`App/index.jsx:152-158`) → `TorrentCard` per torrent (`web/src/components/TorrentList/index.jsx:43-45`) → dialog.

## Key files
| File | Role |
|---|---|
| `web/src/components/DialogTorrentDetailsContent/index.jsx` | Dialog body, layout, local state; passes pin props |
| `.../customHooks.jsx` | `useUpdateCache`, `useCreateCacheMap`, `useGetSettings` |
| `.../helpers.js` | `isFilePlayable` by extension |
| `.../Table/index.jsx` | Playable file table (wide + short/mobile), `pinState` + Disk column |
| `.../TorrentFunctions/index.jsx` | Torrent action buttons, pin block, off confirm dialog |
| `.../TorrentFunctions/style.js` | `SmallLabel`, `MainSectionButtonGroup`, `PinControls` (`:41-54`) |
| `.../TorrentCache/index.jsx` | Canvas cache map ("snake") |
| `.../TorrentCache/getShortCacheMap.js` | Mini map: non-empty pieces + padding |
| `.../TorrentCache/snakeSettings.js` | Colors/sizes per theme and mini/default |
| `.../DetailedView/index.jsx` | Full cache view + debug checkbox |
| `.../widgets/index.jsx`, `widgets/useGetWidgetColors.jsx` | Stat widgets and colors |
| `.../DialogHeader.jsx` | AppBar with back/close |
| `.../StatisticsField.jsx` | Widget presentational component |
| `web/src/components/TorrentCard/index.jsx`, `TorrentCard/style.js` | Card: dialog host, memo comparator, `PinBadge` (`style.js:271-286`) |
| `web/src/components/Settings/SecondarySettingsComponent.jsx`, `defaultSettings.js` | `DefaultPinNext` field + default |
| `web/src/utils/Hosts.js` | API URL helpers |
| `web/src/i18n.js`, `web/src/locales/*/translation.json` | Translations |

## Flow
1. Props: `closeDialog`, `torrent` (one item of `/torrents list`). Destructures `poster, hash, title, category, name, stat, download_speed, upload_speed, torrent_size, file_stats, pin_mode, pin_next, pin_progress, pin_error` (`index.jsx:50-65`). Field shapes: [Torrent status JSON](../../concepts/torrent-status-json.md).
2. `useUpdateCache(hash)`: `setInterval` every **100 ms** → `POST /cache {action:'get', hash}`; error → `{}`; guarded by mounted ref (`customHooks.jsx:5-35`).
3. `useGetSettings(cache)`: `POST /settings {action:'get'}` in an effect keyed on `cache`, i.e. after every cache update (`customHooks.jsx:63-69`). Used for `PreloadCache` → buffer size = `Capacity * PreloadCache%`, min 32 MB (`index.jsx:108-110`), and `UseDisk`/`DefaultPinNext` → `TorrentFunctions` (`index.jsx:208-209`). No new request for pin UI.
4. Viewed list: once per `hash`, `POST /viewed {action:'list', hash}` → sorted `file_index` array (`index.jsx:98-106`).
5. `playableFileList = file_stats.filter(isFilePlayable(path))` on every `file_stats` change (`index.jsx:86-88`); `isFilePlayable` = extension in hard-coded video/audio list (`helpers.js:1-81`). Per-file `pinned`/`completed`/`downloaded` ride along.
6. Seasons parsed with `parse-torrent-title` from playable paths; first season selected (`index.jsx:72-84`); season buttons when >1 season (`index.jsx:241-260`).
7. Loader until cache non-empty and `stat` is not `GETTING_INFO`/`IN_DB` (`index.jsx:90-96`; states `web/src/torrentStates.js:1`).
8. Layout (`index.jsx:136-274`): `DialogHeader` (title switches for detailed view, back button) → either `DetailedView` or grid of `MainSection` (poster, title, widgets `:186-193`, `TorrentFunctions` `:197-210`), `CacheSection` (buffer `LoadingProgress` `:218-223`, mini `TorrentCache` `:226`, "detailed view" button `:227-235`), `TorrentFilesSection` (`Table` `:262-268`).

### Table (`Table/index.jsx`)
- Props: `playableFileList, viewedFileList, selectedSeason, seasonAmount, hash` (`:41`).
- Columns: Viewed, Name, Season (only if one season), Episode, Resolution (each only if parsed from any path), Size, Disk (only if `showPinColumn`), Actions (`:91-101`).
- Row per file filtered by `selectedSeason` (`:118`); `isViewed = viewedFileList.includes(id)` → row class `viewed-file-row` + indicator cell (`:108`, `:119-120`); styles `Table/style.js:10,64`.
- File `id` usage: preload `fetch(streamHost()?link=hash&index=id&preload)` (`:45`); play link `streamHost()/<basename>?link=hash&index=id&play` (`:46-47`); GStreamer HLS URL (`:48-56`); player key `${id}:gst|stream` (`:51`).
- Actions: Preload, Infuse/SenPlayer/VLC/IINA links gated by `localStorage` flags (`:75-84`, `:129-159`), `VideoPlayer` or Open link, Copy link (`:160-189`).
- Mobile `ShortTableWrapper` duplicates the same data as cards (`:199-320`).
- Empty list → plain string `'No playable files in this torrent'` (not translated) (`:86-87`).
- **Predownload state:**
  - `pinState(file)` (`:33-38`): `downloaded` → `{kind:'downloaded'}`; else `pinned` → `{kind:'downloading', percent: floor(100 × (completed||0) / length)}` clamped to 100, `length 0` → 0; else `null`.
  - `showPinColumn = playableFileList.some(pinned || downloaded)` (`:63`) — unpinned torrents render exactly as before.
  - `renderPinState(file, emptyValue)` (`:64-70`): downloaded → `OfflinePin` icon (`fontSize='small'`, `titleAccess` `Pin.Downloaded`); downloading → `N%`; null → `emptyValue`.
  - Desktop: header `Pin.Column` after Size (`:99`), cell `data-label='predownload'`, empty → `null` (`:126`). Mobile: extra `short-table-field` `Pin.Column` after Size, empty → `—` (`:247-252`).
  - A stub (unloaded pinned) row has no `completed` → pinned-not-downloaded files show `0%`.

### TorrentFunctions (`TorrentFunctions/index.jsx`)
- Props: `hash, viewedFileList, playableFileList, name, title, setViewedFileList, pinMode, pinNext, pinProgress, pinError, useDisk, defaultPinNext` (`:29-42`).
- Latest viewed file = last id in sorted viewed list (`:112-115`).
- Playlist block (full / from latest) when >1 playable file and views exist (`:125-155`); links `stream/<name>.m3u?link=hash&m3u[&fromlast]` (`:119-120`).
- "Remove views" → `POST /viewed {action:'rem', hash, file_index:-1}` then clears list (`:117-118`, `:218`).
- "Drop torrent" → `POST /torrents {action:'drop', hash}` (`:116`, `:221`).
- Info: download playlist (single file or no views) and copy magnet (`:225-239`).
- UI: MUI `Button` inside styled `MainSectionButtonGroup`, labels `SmallLabel`.

#### Pin block (Phase 6)
- Placement: `SmallLabel` `Pin.Title` (+ ` N%` = `pinProgress || 0` when pinned) after the playlist block, before `TorrentState` (`:156-159`); controls in `PinControls ref={pinControlsRef}` (`:160-199`, flex-wrap row for mobile).
- State (all local, `:48-53`): `draftMode` (`'next'`, used only while unpinned), `draftNext` (`null` = not edited, else typed string), `isPending`, `requestError`, `isOffConfirmOpen`, `pinControlsRef`. No sync effects, no store, no new query.
- Derived (`:45-59`): `isPinned = pinMode ∈ {all,next}`; `defaultNext = String(defaultPinNext > 0 ? defaultPinNext : 3)` (`fallbackPinNext` `:26`); `mode = isPinned ? pinMode : draftMode`; `nextValue = draftNext ?? (isPinned ? String(pinNext||0) : defaultNext)`; `nextValid = /^\d+$/`; `settingsLoaded = useDisk !== undefined`; `diskOff = useDisk === false`.
- `sendPin(newMode, next)` (`:61-76`): clears `requestError`, `isPending=true` → `POST /torrents {action:'pin', hash, pin_mode, pin_next}` → success: `off` resets `draftMode='next'`, then `queryClient.invalidateQueries('torrents', undefined, { cancelRefetch: true })` (`:69`); failure: `requestError = error.response?.data?.error || error.message`; finally `draftNext=null`, `isPending=false`.
- Actions:
  - Checkbox on (`onPinToggle` `:78-84`) → `sendPin(draftMode, nextValid ? Number(nextValue) : Number(defaultNext))`.
  - Checkbox off → opens confirm dialog only; OK (`confirmOff` `:86-89`) → `sendPin('off', pinNext||0)`; Cancel/backdrop → close, no request.
  - Radio (`onModeChange` `:91-94`): pinned → `sendPin(value, nextValid ? Number(nextValue) : pinNext||0)` (no local mode change); unpinned → `draftMode = value`.
  - N field: `onChange` → `draftNext`; `commitNext` on blur / Enter (`:96-105`): return if blur `relatedTarget` is inside `pinControlsRef` (`:98`); return unless pinned, `mode==='next'`, `!isPending`, `draftNext !== null`; invalid or `Number(nextValue) === (pinNext||0)` → `draftNext=null`; else `sendPin('next', Number(nextValue))`.
- Disabled matrix (`:107-110`):
  | Control | Disabled when |
  |---|---|
  | Checkbox | `isPending \|\| !settingsLoaded \|\| (diskOff && !isPinned) \|\| (!isPinned && !nextValid && mode==='next')` |
  | Radios | `isPending \|\| !settingsLoaded \|\| diskOff` |
  | N `TextField` (`type=number`, `min 0`, `step 1`, `error=!nextValid`) | radios disabled `\|\| mode !== 'next'` |
- Helper texts (`:196-198`): `diskOff` → `Pin.DiskRequired`; `pinError === 'no_space'` → error `Pin.NoSpace`; `requestError` → error, verbatim.
- Confirm dialog (`:201-214`): MUI `Dialog` + `DialogTitle` `Pin.OffConfirmTitle` + `DialogContentText` `Pin.OffConfirmText` + `DialogActions` Cancel (outlined) / OK (contained, `autoFocus`) — same shape as the card's delete dialog (`TorrentCard/index.jsx:586-590`).
- Memo comparator (`:243-250`): equal iff `pinMode, pinNext, pinProgress, pinError, useDisk, defaultPinNext` all `===`; other props are read only on a render triggered by those.

### TorrentCard badge (`TorrentCard/index.jsx`)
- Reads `pin_mode, pin_progress, pin_error` (`:203-205`); `isPinned`, `isPinNoSpace = pin_error==='no_space' && (pin_progress||0) < 100` (`:207-208`).
- `PinBadge` inside `description-section-name` next to category/Name (`:541-545`): error variant text `Pin.NoSpaceShort`, `title` `Pin.NoSpace`; else `Pin.Badge` (`Disk {{progress}}%`), `title` `Pin.Title`. Style: inline span, `accentCardColor` or `#E57373`, white text, nowrap (`style.js:271-286`). Absent for unpinned → card height unchanged.
- Comparator + `sameFileList`: see Gotchas.

### TorrentCache (`TorrentCache/index.jsx`)
- `useCreateCacheMap(cache)`: one entry per `PiecesCount` with `percentage = Size/Length*100`, `priority`, `isReader` (`i === Reader`), `isReaderRange` (`Start <= i < End`) (`customHooks.jsx:37-61`).
- Mini mode: `getShortCacheMap` keeps only pieces with `percentage > 0`, pads to at least `Capacity/PiecesLength - 1` blocks and to full rows (`TorrentCache/index.jsx:42-45`, `getShortCacheMap.js:1-27`). Mini map therefore loses piece positions.
- Canvas draw: fill gradient for in-progress, `completeColor` at 100 %, else background; stroke reader / complete / range / border (`TorrentCache/index.jsx:65-92`). Colors per theme (`DarkModeContext`) and `mini`/`default` in `snakeSettings.js:4-55`; gradient `snakeSettings.js:57-67`.
- Debug mode (`localStorage.isSnakeDebugMode`) prints priority letter: 2 `H`, 3 `R`, 4 `N`, 5 `A`, 1 blank (`TorrentCache/index.jsx:94-106`).
- Size via `react-measure` (`:130`).

### DetailedView (`DetailedView/index.jsx`)
- Widgets incl. pieces count/length (`:38-46`), full `TorrentCache` (`:72`).
- Debug `FormControlLabel` + `Checkbox` stored in `localStorage` (`:54-68`).

### Widgets
- `StatisticsField({icon,title,value,iconBg,valueBg})` (`StatisticsField.jsx:3-14`); widgets: download/upload speed, peers, pieces count/length, status, size, category (`widgets/index.jsx:19-152`); colors via `DarkModeContext` (`widgets/useGetWidgetColors.jsx:30-35`).

### API URL helpers (`web/src/utils/Hosts.js`)
- Base `REACT_APP_SERVER_HOST` or `window.location` origin (`Hosts.js:1-3`), overridable via `setTorrServerHost` (`:22-24`).
- Used here: `torrentsHost` `/torrents` (`:5`), `viewedHost` `/viewed` (`:6`), `cacheHost` `/cache` (`:7`), `settingsHost` `/settings` (`:9`), `streamHost`/`playlistTorrHost` `/stream` (`:10`, `:13`).
- Calls are `axios.post(xHost(), { action, ...})` (e.g. `TorrentFunctions/index.jsx:65`, `:116`); preload uses `fetch` (`Table/index.jsx:45`).

### Translations
- `web/src/i18n.js:1-29`: i18next + `LanguageDetector`, resources en/ru/ua/zh/bg/fr/ro imported from `locales/<lang>/translation.json`, fallback `en`; loaded by `import 'i18n'` in `web/src/index.jsx:6`.
- Components use `const { t } = useTranslation()`; keys flat (`"Viewed"` `locales/en/translation.json:400`) or nested (`"DetailedCacheView"` `:84`, `"Pin"` `:133-147`, `"SettingsDialog"` `:177`).
- `Pin` object (alphabetical top-level position): `Title, Enable, ModeAll, ModeNext, NextCount, DiskRequired, NoSpace, NoSpaceShort, Badge` (`{{progress}}`), `Column, Downloaded, OffConfirmTitle, OffConfirmText`; plus `SettingsDialog.DefaultPinNext`, `DefaultPinNextHint` (en `:256-257`). en and ru translated (ru `Pin` `locales/ru/translation.json:96-110`); bg/fr/ro/ua/zh carry English text.
- Imports resolve from `src` (`web/jsconfig.json:3`).

### Settings pattern (entry points only)
- `SettingsDialog.jsx`: loads `POST /settings {action:'get'}` (`web/src/components/Settings/SettingsDialog.jsx:63-67`), saves `{action:'set', sets}` on Save (`:79-85`).
- `inputForm` handler: maps `event.target.id` to settings key; `type==='checkbox'` → boolean, with inverted keys `Disable*` (`SettingsDialog.jsx:96-119`); `updateSettings` merges props (`:137`).
- Toggles are MUI `FormControlLabel` + **`Switch`** (not Checkbox) with `id=<BTSets key>`, `onChange={inputForm}`, plus `FormHelperText` hint, e.g. `SecondarySettingsComponent.jsx:315-321`; `UseDisk` chosen via RAM/Disk buttons calling `updateSettings({UseDisk})` in `PrimarySettingsComponent.jsx:110-166`; `RemoveCacheOnDrop` Switch at `:132-140`.
- Numeric fields: outlined `TextField` `id=<key>`, `type='number'`, `onChange={inputForm}`, `fullWidth`, followed by `<br />` — `TorrentDisconnectTimeout` (`SecondarySettingsComponent.jsx:220-232`), `DefaultPinNext` right after it (`:234-245`, `helperText` `DefaultPinNextHint`, `inputProps={{ min: 1 }}`).
- `defaultSettings.js:1-51` — defaults used by "reset to default" (`SettingsDialog.jsx:284-287`); includes `UseDisk: false` (`:5`), `TorrentsSavePath: ''`, `RemoveCacheOnDrop: false`, `DefaultPinNext: 3` (`:26`).

## Dependencies
- Overview: [web](_overview.md)
- Concepts: [Torrent status JSON](../../concepts/torrent-status-json.md) (incl. Phase 6 UI contract), [CI / test gate](../../concepts/ci-test-gate.md)
- Server: [Torrent pin](../server/torrent-pin.md) (`action:"pin"`, `DefaultPinNext`), [Torrent storage cache](../server/torrent-storage-cache.md) (`/cache` payload), [Torrent API helpers](../server/torrent-api-helpers.md), [Streaming](../server/streaming.md), [Preload](../server/preload.md), [Torrent lifecycle](../server/torrent-lifecycle.md)

## Gotchas / decisions
- **`TorrentCard` comparator gates the dialog:** the dialog is rendered inside `TorrentCard`, whose `memo` comparator compares only `hash,title,name,poster,category,stat,torrent_size,download_speed,data,pin_mode,pin_next,pin_progress,pin_error` and, via `sameFileList`, file `id/path/length/pinned/completed/downloaded` (`TorrentCard/index.jsx:647-666`, `sameFileList` `:136-153`). Any other new torrent/file field will not reach the card or the dialog unless one of these also changes — extend the comparator. Unpinned torrents never carry the pin keys → no extra renders.
- **`TorrentFunctions` re-renders only on pin inputs:** comparator on `pinMode, pinNext, pinProgress, pinError, useDisk, defaultPinNext` (`TorrentFunctions/index.jsx:243-250`). Other props (`viewedFileList`, `playableFileList`, `name`, `title`) stay frozen at mount except when a pin input change forces a render (pre-existing frozen behaviour kept by decision).
- **Derive UI from server status, don't sync drafts (after 5a, findings 1, 2, 7):** the first version copied `pin_mode`/`pin_next` into drafts via effects; a failed request left the rejected mode on screen and a focused, untouched N re-sent a stale value. Now `mode`/`nextValue` are derived and `draftNext` is nullable, cleared after every request (success or failure).
- **`cancelRefetch: true` on invalidate (after 5e Codex, C1):** react-query 3.39.3 `invalidateQueries` reuses a list fetch that started before the POST (`query.js:252-264` in react-query), so cleared drafts could show the pre-write N and a following radio click re-sent it (`TorrentFunctions/index.jsx:69`).
- **N blur into another pin control does not commit (after 5a, finding 3):** otherwise `isPending` disabled the radio before the click landed; the radio/checkbox request carries the typed N (`:98`).
- **Confirm only on uncheck:** `off` deletes `TorrentsSavePath/<hash>`; mode/N changes keep data outside the new window → no confirm. On a loaded torrent the dir disappears only after unload (dialog keeps `/cache` polling → torrent stays loaded until the disconnect timeout after close); confirm text "will be deleted" holds.
- **`UseDisk=false`:** unpinned → every control disabled + `Pin.DiskRequired`; pinned → checkbox stays enabled so the user can still unpin (server accepts `off` without disk), radios/N disabled + hint.
- **Settings still loading (`useDisk === undefined`) → controls disabled, no hint** (avoids flashing the disk hint).
- **Radio / N on a pinned torrent apply immediately** (one POST); N commits on blur/Enter, not per keystroke — no request per digit and the 1 s poll cannot overwrite a half-typed value.
- **`DefaultPinNext` input `min 1`:** server normalises `<= 0` to 3 ([Torrent pin](../server/torrent-pin.md)), so 0 is not representable in settings; per-torrent N accepts 0.
- **Disk column hidden when no playable file is pinned/downloaded** — unpinned torrents look exactly as before.
- **Server error text shown verbatim** (English JSON `{error}` from the API); translating server errors out of scope.
- **ru `Pin.Title` = "Фоновая загрузка"** (after 5a, finding 5): "Предзагрузка" is already the ru Preload button/section name.
- **Known limitation (5e C2, downgraded):** pin controls sit behind the dialog loader (cache + metadata, `index.jsx:90-96`); a pinned torrent whose metadata never arrives cannot be unpinned from the dialog (card Delete and API still work).
- **Known limitation (5e C3, downgraded):** per-file indicators only for playable files — subtitles/other files pinned by `all` show no per-file state; card badge and dialog percent cover the whole target.
- **Known, not fixed:** setState after unmount when the dialog closes mid-request (React 17 dev warning only); disabled outlined N field is visually subtle in the light theme (MUI default).
- `Table` re-renders only on deep prop change (`lodash/isEqual`, `Table/index.jsx:324`); `playableFileList` is rebuilt from `file_stats`, so file-level pin fields propagate once the card passes them.
- `TorrentCache` re-renders only when `cache.Pieces` or `cache.Readers` change (`TorrentCache/index.jsx:144-147`).
- `/cache` is polled every 100 ms while the dialog is mounted (`customHooks.jsx:20-28`); server `getCache` calls `torr.GetTorrent`, which loads a DB-only torrent and extends its expiry (`server/web/api/cache.go:52`, `server/torr/apihelper.go:107-138`). An open dialog keeps the torrent loaded.
- `/settings get` fires on every cache update (`customHooks.jsx:66-67`), ~10 req/s; pin UI reuses it.
- Cache map `isReaderRange` uses `i < End` (`customHooks.jsx:52`) while server `inRanges` is inclusive (`server/torr/storage/torrstor/ranges.go:16`).
- `/cache` only lists pieces with `Size > 0` (`server/torr/storage/torrstor/cache.go:323`); the map shows missing pieces as 0 %.
- Viewed list is fetched once per hash (`index.jsx:98-106`); playback elsewhere does not refresh it until the dialog remounts.
- Dialog is mounted inside `StyledDialog`; whether content stays mounted while closed depends on MUI Dialog defaults (see Open questions).

## Open questions
- Does MUI v4 `Dialog` unmount children when `open=false` (affects whether `/cache` polling stops when the dialog closes)? Not verified in repo code.
- Anacrolix priority numeric values behind debug letters (`TorrentCache/index.jsx:96-100`) not defined in repo.
