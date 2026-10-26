package settings

import (
	"encoding/json"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// withTestDB opens a fresh config DB in a temp dir and restores the globals.
func withTestDB(t *testing.T) {
	t.Helper()
	oldPath, oldReadOnly, oldSearchWA, oldBTsets := Path, ReadOnly, SearchWA, BTsets
	Path = t.TempDir()
	BTsets = nil
	if err := InitSets(false, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		CloseDB()
		tdb = nil
		globalBboltDB = nil
		Path, ReadOnly, SearchWA, BTsets = oldPath, oldReadOnly, oldSearchWA, oldBTsets
	})
}

func testHash(b byte) metainfo.Hash {
	var h metainfo.Hash
	h[0] = b
	h[19] = b
	return h
}

func findRow(t *testing.T, hash metainfo.Hash) *TorrentDB {
	t.Helper()
	for _, db := range ListTorrent() {
		if db.InfoHash == hash {
			return db
		}
	}
	return nil
}

// legacyPinRow is a stored row written while the disk pin existed.
func legacyPinRow(hash metainfo.Hash) string {
	return `{"InfoHash":"` + hash.HexString() + `","title":"old","data":"x","timestamp":5,` +
		`"pin_mode":"next","pin_next":3,"pin_anchor":2,"pin_downloaded":[1,2]}`
}

// Rows stored by the removed disk pin keep loading; the pin keys are ignored.
func TestLegacyPinRowLoads(t *testing.T) {
	withTestDB(t)
	hash := testHash(1)
	tdb.Set("Torrents", hash.HexString(), []byte(legacyPinRow(hash)))

	got := findRow(t, hash)
	if got == nil {
		t.Fatal("legacy pin row not listed")
	}
	if got.Title != "old" || got.Data != "x" || got.Timestamp != 5 {
		t.Fatalf("legacy row fields lost: %+v", got)
	}
}

// A save of a legacy row rewrites it without the pin keys.
func TestAddTorrentDropsLegacyPinKeys(t *testing.T) {
	withTestDB(t)
	hash := testHash(1)
	tdb.Set("Torrents", hash.HexString(), []byte(legacyPinRow(hash)))

	row := findRow(t, hash)
	row.Title = "edited"
	AddTorrent(row)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(tdb.Get("Torrents", hash.HexString()), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["title"]) != `"edited"` {
		t.Fatalf("edit not saved: %s", raw["title"])
	}
	for _, key := range []string{"pin_mode", "pin_next", "pin_anchor", "pin_downloaded"} {
		if _, ok := raw[key]; ok {
			t.Errorf("saved row still has %s", key)
		}
	}
}

// A corrupt row is skipped, the valid rows are still listed.
func TestListTorrentSkipsCorruptRow(t *testing.T) {
	withTestDB(t)
	tdb.Set("Torrents", testHash(2).HexString(), []byte(`{"title":`))
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(1)}, Title: "ok"})

	list := ListTorrent()
	if len(list) != 1 || list[0].Title != "ok" {
		t.Fatalf("list = %+v, want only the valid row", list)
	}
}

// A stale snapshot of other rows must never be written back, so a save
// touches only its own row (checked via a raw key only a rewrite would drop).
func TestAddTorrentWritesOnlyItsRow(t *testing.T) {
	withTestDB(t)
	other := testHash(2)
	raw := `{"InfoHash":"` + other.HexString() + `","title":"other","pin_mode":"next","extra":1}`
	tdb.Set("Torrents", other.HexString(), []byte(raw))

	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(1)}, Title: "saved"})

	if got := string(tdb.Get("Torrents", other.HexString())); got != raw {
		t.Fatalf("other row rewritten:\n got %s\nwant %s", got, raw)
	}
	if findRow(t, testHash(1)) == nil {
		t.Fatal("saved row missing")
	}
}

func TestSetTorrentRemoteDownloadWritesOnlyFlag(t *testing.T) {
	withTestDB(t)
	hash := testHash(1)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}, Title: "t", Timestamp: 7})

	if !SetTorrentRemoteDownload(hash, true) {
		t.Fatal("setter reported no row")
	}
	got := findRow(t, hash)
	if got == nil || !got.RemoteDownload || got.Title != "t" || got.Timestamp != 7 {
		t.Fatalf("row after set on: %+v", got)
	}
	if !SetTorrentRemoteDownload(hash, false) {
		t.Fatal("setter reported no row")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(tdb.Get("Torrents", hash.HexString()), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["remote_download"]; ok {
		t.Fatalf("off flag still stored: %s", tdb.Get("Torrents", hash.HexString()))
	}
}

func TestSetTorrentRemoteDownloadWithoutRow(t *testing.T) {
	withTestDB(t)
	hash := testHash(2)
	if SetTorrentRemoteDownload(hash, true) {
		t.Fatal("setter reported success without a row")
	}
	if findRow(t, hash) != nil {
		t.Fatal("setter created a row")
	}
}

// A general save never changes the stored flag; a new row takes the passed one.
func TestAddTorrentKeepsStoredRemoteFlag(t *testing.T) {
	withTestDB(t)
	hash := testHash(3)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}, Title: "a"})
	SetTorrentRemoteDownload(hash, true)

	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}, Title: "b"})
	got := findRow(t, hash)
	if got == nil || !got.RemoteDownload || got.Title != "b" {
		t.Fatalf("save cleared the flag: %+v", got)
	}

	SetTorrentRemoteDownload(hash, false)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}, Title: "c", RemoteDownload: true})
	if got := findRow(t, hash); got.RemoteDownload {
		t.Fatal("save set the flag on an existing row")
	}

	fresh := testHash(4)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: fresh}, RemoteDownload: true})
	if got := findRow(t, fresh); got == nil || !got.RemoteDownload {
		t.Fatalf("new row lost the passed flag: %+v", got)
	}
}
