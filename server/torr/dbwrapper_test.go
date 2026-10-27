package torr

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"server/settings"
	"server/torr/state"
)

// The settings DB is a process-wide singleton that cannot be reopened after
// CloseDB, so it is opened once for the package and emptied per test.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "torr-test-")
	if err != nil {
		panic(err)
	}
	oldPath, oldReadOnly, oldSearchWA, oldBTsets := settings.Path, settings.ReadOnly, settings.SearchWA, settings.BTsets
	settings.Path = dir
	if err := settings.InitSets(false, false); err != nil {
		panic(err)
	}
	settings.Path, settings.ReadOnly, settings.SearchWA, settings.BTsets = oldPath, oldReadOnly, oldSearchWA, oldBTsets
	code := m.Run()
	settings.CloseDB()
	os.RemoveAll(dir)
	os.Exit(code)
}

// withTorrDB gives a test an empty torrents table and an empty BTServer, and
// restores settings.ReadOnly, settings.BTsets and bts afterwards.
func withTorrDB(t *testing.T) {
	t.Helper()
	oldReadOnly, oldBTsets, oldBts := settings.ReadOnly, settings.BTsets, bts
	clearTorrentsDB()
	settings.ReadOnly = false
	settings.BTsets = &settings.BTSets{TorrentDisconnectTimeout: 30}
	bts = &BTServer{torrents: map[metainfo.Hash]*Torrent{}}
	t.Cleanup(func() {
		clearTorrentsDB()
		settings.ReadOnly, settings.BTsets, bts = oldReadOnly, oldBTsets, oldBts
	})
}

func clearTorrentsDB() {
	for _, db := range settings.ListTorrent() {
		settings.RemTorrent(db.InfoHash)
	}
}

func testHash(b byte) metainfo.Hash {
	var h metainfo.Hash
	h[0] = b
	h[19] = b
	return h
}

func dbTorrent(hash metainfo.Hash) *Torrent {
	return &Torrent{
		TorrentSpec: &torrent.TorrentSpec{InfoHash: hash},
		Title:       "title",
		Category:    "movie",
		Data:        `{"TorrServer":{"Files":[]}}`,
		Timestamp:   42,
		Size:        100,
	}
}

func assertDBFields(t *testing.T, where string, got *Torrent) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: torrent is nil", where)
	}
	if got.Title != "title" || got.Category != "movie" || got.Timestamp != 42 || got.Size != 100 ||
		got.Data != `{"TorrServer":{"Files":[]}}` || got.Stat != state.TorrentInDB {
		t.Fatalf("%s: fields = %+v", where, got)
	}
}

func TestAddTorrentDBSurvivesGetAndList(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))

	assertDBFields(t, "get", GetTorrentDB(hash))
	assertDBFields(t, "list", ListTorrentsDB()[hash])
}

func TestGetTorrentDBUnknownHash(t *testing.T) {
	withTorrDB(t)
	if got := GetTorrentDB(testHash(9)); got != nil {
		t.Fatalf("unknown hash returned %+v", got)
	}
}

func statusJSON(t *testing.T, tor *Torrent) string {
	t.Helper()
	buf, err := json.Marshal(tor.Status())
	if err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

// The disk pin was removed: no status, torrent or file key mentions it.
func TestStatusHasNoPinKeys(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := loadedTorrent(t, "status")

	js := statusJSON(t, tor)
	if !strings.Contains(js, `"file_stats":[{"id":1,"path":"status","length":1}]`) {
		t.Fatalf("loaded status JSON has unexpected file stats: %s", js)
	}
	for _, key := range []string{`"pin_`, `"pinned"`, `"completed"`, `"downloaded"`} {
		if strings.Contains(js, key) {
			t.Fatalf("status JSON has %s: %s", key, js)
		}
	}
}

func TestAddTorrentDBStoresFileLayout(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := loadedTorrent(t, "layout")

	tor.Data = ""
	AddTorrentDB(tor)
	data := GetTorrentDB(tor.Hash()).Data
	if data != `{"TorrServer":{"Files":[{"id":1,"path":"layout","length":1}]}}` {
		t.Fatalf("stored data = %s", data)
	}
}

// File ids are the 1-based index after natural path sort; viewed records
// store them, so the order must not change. (utils.CompareStrings strips the
// common prefix including digits, so e1 vs e10 sorts e10 first; not used here.)
func TestStatusFileIdsNaturalOrder(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	info := metainfo.Info{Name: "show", PieceLength: 16384, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{
		{Path: []string{"e10.mkv"}, Length: 1},
		{Path: []string{"e2.mkv"}, Length: 1},
		{Path: []string{"e3.mkv"}, Length: 1},
	}}
	buf, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	tor, err := NewTorrent(&torrent.TorrentSpec{InfoBytes: buf, InfoHash: metainfo.HashBytes(buf)}, bts)
	if err != nil {
		t.Fatal(err)
	}
	if !tor.WaitInfo() {
		t.Fatal("WaitInfo failed")
	}

	want := []string{"show/e2.mkv", "show/e3.mkv", "show/e10.mkv"}
	fs := tor.Status().FileStats
	if len(fs) != len(want) {
		t.Fatalf("file stats = %d, want %d", len(fs), len(want))
	}
	for i, f := range fs {
		if f.Id != i+1 || f.Path != want[i] {
			t.Errorf("file %d = %d %q, want %d %q", i, f.Id, f.Path, i+1, want[i])
		}
	}
}
