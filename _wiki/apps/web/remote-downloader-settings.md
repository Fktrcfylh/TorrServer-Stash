# NAS settings (remote downloader)

**App:** web
**Status:** stable
**Last reviewed:** 2026-10-26 (commit `8fdc472f`, specs/nas-predownload Phases 6, 8)

## Summary
Settings dialog tab "NAS" that edits `BTSets.RemoteDownloader` (type Transmission/qBittorrent, URL, user, password, download dir) and tests the connection with the values currently in the form, without saving. Saved with the rest of the settings (`action:'set'`, restarts the torrent client). Server side: [Remote download](../server/remote-download.md#settings). User docs: `README.md:11-24`.

## Entry points
- `web/src/components/Settings/SettingsDialog.jsx:54-56` — `tabRemote = 5`, `tabGStreamer = 6`, `maxTab` falls back to `tabRemote` without GStreamer
- `web/src/components/Settings/SettingsDialog.jsx:196` — tab label `SettingsDialog.Tabs.Remote` ("NAS")
- `web/src/components/Settings/SettingsDialog.jsx:269-271` — `TabPanel` → `<RemoteDownloaderSettings settings updateSettings />`

## Key files
| File | Role |
|---|---|
| `web/src/components/Settings/RemoteDownloaderSettings.jsx` | Tab body: per-type fields + Test button + result |
| `web/src/components/Settings/SettingsDialog.jsx` | Loads `{action:'get'}` (`:66`), saves `{action:'set', sets}` (`:87`), `updateSettings` merge (`:139`) |
| `web/src/locales/*/translation.json` | `Remote.*` keys + `SettingsDialog.Tabs.Remote` |

## Flow
1. `remoteTypes` (`RemoteDownloaderSettings.jsx:10-27`): per type `value`, `label`, URL `placeholder` and the locale keys whose wording differs — Transmission `http://nas:9091/transmission/rpc`, `Remote.URLHint`/`TestOk`/`LpdOff`; qBittorrent `http://nas:8080`, `Remote.URLHintQbittorrent`/`TestOkQbittorrent`/`LpdOffQbittorrent`. `typeInfo` = entry of the current `Type`, else Transmission (`:38`).
2. Reads `settings.RemoteDownloader` with defaults `Type='transmission'`, other fields `''` (`:35-37`).
3. Every field change → `updateSettings({ RemoteDownloader: { ...RemoteDownloader, [field]: value } })` (`:44-51`); nothing is sent until Save.
4. Fields (`:70-133`): native `Select` from `remoteTypes` (`:80`); URL (`type=url`, per-type placeholder + hint, `:88-99`); Username (`autoComplete=off`); Password (`type=password`, `autoComplete=new-password`); Download dir.
5. Test (`:53-61`): `POST /settings {action:'test_remote', remote_downloader: config}` with the current form values; result or `error.response?.data?.error || error.message` stored with `configKey = JSON.stringify(config)`.
6. Result shown only while the form still equals the tested config (`shownTest`, `:40`): success line `typeInfo.testOk` (version, rpc version, peer port) + `lpd_enabled` → info `Remote.LpdOn` / warning `typeInfo.lpdOff` (`:142-155`); error line (`:156`).

## Dependencies
- Overview: [web](_overview.md)
- Server: [Remote download](../server/remote-download.md) (`test_remote`, `RemoteDownloaderConfig`, both clients)
- Consumer of the saved URL: [Torrent details dialog](torrent-details-dialog.md#nas-block-torrentfunctions) (`remoteConfigured`)

## Gotchas / decisions
- `RemoteDownloader` is **not** in `web/src/components/Settings/defaultSettings.js`, so "reset to default" (`SettingsDialog.jsx:292-295`) keeps the NAS config in the form. Server `def` resets it to `{Type: transmission}` (`server/settings/btsets.go:250`).
- Test never saves; changing any field hides a stale result.
- **qBittorrent: repeated tests with wrong credentials can get the TorrServer IP banned** — every `test_remote` builds a fresh client (`remotedl.New`), so the per-client "never resend rejected credentials" guard does not span tests; qBittorrent bans after 5 failed logins (README `:47-49`).
- qBittorrent reports no RPC version: `rpc_version` is `0` (the qB `TestOk` text omits it).
- `settings get` returns the password in clear (masked only in server logs) — the field is pre-filled.
- Locales: top-level `Remote` object (en `web/src/locales/en/translation.json:213`; 33 leaf keys, 8 under `Remote.State`, 3 `*Qbittorrent` variants) + `SettingsDialog.Tabs.Remote` (en `:343`) in all 7 locales.
