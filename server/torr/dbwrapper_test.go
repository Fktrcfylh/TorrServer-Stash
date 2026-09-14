package torr

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/anacrolix/torrent"
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

// withTorrDB gives a test an empty torrents table, an empty BTServer and an
// empty no-space set, and restores settings.ReadOnly, settings.BTsets and bts
// afterwards.
func withTorrDB(t *testing.T) {
	t.Helper()
	oldReadOnly, oldBTsets, oldBts := settings.ReadOnly, settings.BTsets, bts
	clearTorrentsDB()
	clearPinNoSpace()
	settings.ReadOnly = false
	settings.BTsets = &settings.BTSets{DefaultPinNext: 3, TorrentDisconnectTimeout: 30}
	bts = &BTServer{torrents: map[metainfo.Hash]*Torrent{}}
	t.Cleanup(func() {
		clearTorrentsDB()
		clearPinNoSpace()
		settings.ReadOnly, settings.BTsets, bts = oldReadOnly, oldBTsets, oldBts
	})
}

func clearPinNoSpace() {
	pinNoSpaceMu.Lock()
	clear(pinNoSpace)
	pinNoSpaceMu.Unlock()
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

func pinnedDBTorrent(hash metainfo.Hash) *Torrent {
	return &Torrent{
		TorrentSpec:   &torrent.TorrentSpec{InfoHash: hash},
		Title:         "title",
		Category:      "movie",
		Data:          `{"TorrServer":{"Files":[]}}`,
		Timestamp:     42,
		Size:          100,
		PinMode:       settings.PinModeNext,
		PinNext:       4,
		PinAnchor:     2,
		PinDownloaded: []int{1, 3},
	}
}

func assertPin(t *testing.T, where string, got *Torrent, mode string, next, anchor int, downloaded []int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: torrent is nil", where)
	}
	if got.PinMode != mode || got.PinNext != next || got.PinAnchor != anchor || !slices.Equal(got.PinDownloaded, downloaded) {
		t.Fatalf("%s: pin = %q/%d/%d/%v, want %q/%d/%d/%v", where,
			got.PinMode, got.PinNext, got.PinAnchor, got.PinDownloaded, mode, next, anchor, downloaded)
	}
}

func TestAddTorrentDBPinSurvivesGetAndList(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))

	assertPin(t, "GetTorrentDB", GetTorrentDB(hash), settings.PinModeNext, 4, 2, []int{1, 3})
	assertPin(t, "ListTorrentsDB", ListTorrentsDB()[hash], settings.PinModeNext, 4, 2, []int{1, 3})
}

func TestAddTorrentDBUnpinnedStaysOff(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	tor := pinnedDBTorrent(hash)
	tor.PinMode, tor.PinNext, tor.PinAnchor, tor.PinDownloaded = "", 0, 0, nil
	AddTorrentDB(tor)

	assertPin(t, "GetTorrentDB", GetTorrentDB(hash), "", 0, 0, nil)
	assertPin(t, "ListTorrentsDB", ListTorrentsDB()[hash], "", 0, 0, nil)
	if GetTorrentDB(testHash(2)) != nil {
		t.Fatal("GetTorrentDB returned a torrent for an unknown hash")
	}
}

func TestStatusPinJSON(t *testing.T) {
	unpinned := &Torrent{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(1)}, Title: "x"}
	buf, err := json.Marshal(unpinned.Status())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(buf), "pin_mode") || strings.Contains(string(buf), "pin_next") {
		t.Fatalf("unpinned status JSON exposes pin keys: %s", buf)
	}

	pinned := pinnedDBTorrent(testHash(1))
	st := pinned.Status()
	if st.PinMode != settings.PinModeNext || st.PinNext != 4 {
		t.Fatalf("status pin = %q/%d, want next/4", st.PinMode, st.PinNext)
	}
	buf, err = json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf), `"pin_mode":"next"`) || !strings.Contains(string(buf), `"pin_next":4`) {
		t.Fatalf("pinned status JSON lacks pin keys: %s", buf)
	}
}

// pinStatusKeys are the status JSON keys of the pin progress.
var pinStatusKeys = []string{`"pin_progress"`, `"pin_error"`, `"pinned"`, `"completed"`, `"downloaded"`}

func statusJSON(t *testing.T, tor *Torrent) string {
	t.Helper()
	buf, err := json.Marshal(tor.Status())
	if err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func assertNoPinStatusKeys(t *testing.T, where, js string) {
	t.Helper()
	for _, key := range pinStatusKeys {
		if strings.Contains(js, key) {
			t.Fatalf("%s: status JSON has %s: %s", where, key, js)
		}
	}
}

func TestStatusUnpinnedHasNoPinProgress(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "unpinned-status")
	completePieces(t, tor, 0, 1, 2)
	pinNoSpaceSet(tor.Hash(), true)

	before := statusJSON(t, tor)
	assertNoPinStatusKeys(t, "loaded", before)
	if !strings.Contains(before, `"file_stats"`) {
		t.Fatalf("loaded status JSON has no file stats: %s", before)
	}
	tor.muTorrent.Lock()
	tor.PinMode, tor.PinDownloaded = settings.PinModeNext, []int{1}
	tor.muTorrent.Unlock()
	if pinned := statusJSON(t, tor); pinned == before {
		t.Fatal("pinned mirror did not change the status JSON")
	}
	tor.muTorrent.Lock()
	tor.PinMode, tor.PinDownloaded = "", nil
	tor.muTorrent.Unlock()
	if after := statusJSON(t, tor); after != before {
		t.Fatalf("status JSON changed after clearing the pin:\nbefore %s\nafter  %s", before, after)
	}

	stub := &Torrent{TorrentSpec: &torrent.TorrentSpec{InfoHash: tor.Hash()}, Data: episodesData(t), PinDownloaded: []int{1}}
	assertNoPinStatusKeys(t, "stub", statusJSON(t, stub))
}

func TestStatusPinnedLoaded(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "pinned-status")
	SetTorrentPin(tor.Hash().HexString(), settings.PinModeNext, 1)
	// E01 complete, E02 16 of 24 bytes, E04 complete outside the window
	completePieces(t, tor, 0, 1, 2, 6, 7)
	tor.updatePinDownloaded()

	st := tor.Status()
	type fileWant struct {
		pinned     bool
		completed  int64
		downloaded bool
	}
	want := []fileWant{{true, 32, true}, {true, 16, false}, {false, 0, false}, {false, 32, true}, {false, 0, false}}
	if len(st.FileStats) != len(want) {
		t.Fatalf("file stats = %d, want %d", len(st.FileStats), len(want))
	}
	for i, fs := range st.FileStats {
		if got := (fileWant{fs.Pinned, fs.Completed, fs.Downloaded}); got != want[i] {
			t.Fatalf("file %d = %+v, want %+v", fs.Id, got, want[i])
		}
	}
	if st.PinProgress != 100*48/56 {
		t.Fatalf("pin progress = %d, want %d", st.PinProgress, 100*48/56)
	}
	if st.PinError != "" {
		t.Fatalf("pin error = %q without the no-space state", st.PinError)
	}
}

func TestStatusPinnedStub(t *testing.T) {
	withTorrDB(t)
	stub := &Torrent{
		TorrentSpec:   &torrent.TorrentSpec{InfoHash: testHash(1)},
		Data:          episodesData(t),
		PinMode:       settings.PinModeNext,
		PinNext:       1,
		PinDownloaded: []int{2},
	}
	AddTorrentDB(stub)

	st := GetTorrentDB(testHash(1)).Status()
	if len(st.FileStats) != 5 {
		t.Fatalf("stub file stats = %d, want 5", len(st.FileStats))
	}
	for i, fs := range st.FileStats {
		if fs.Id != i+1 || fs.Length == 0 || fs.Completed != 0 || fs.Pinned != (i < 2) || fs.Downloaded != (i == 1) {
			t.Fatalf("stub file %d = %+v", i+1, fs)
		}
	}
	if st.PinProgress != 100*24/56 {
		t.Fatalf("stub pin progress = %d, want %d", st.PinProgress, 100*24/56)
	}

	client := &Torrent{
		TorrentSpec:   &torrent.TorrentSpec{InfoHash: testHash(2)},
		Data:          `{"poster":"x"}`,
		PinMode:       settings.PinModeAll,
		PinDownloaded: []int{1},
	}
	if st := client.Status(); st.FileStats != nil || st.PinProgress != 0 {
		t.Fatalf("stub with client data: file stats %v, progress %d", st.FileStats, st.PinProgress)
	}
}

func TestStatusPinErrorNoSpace(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	loaded := episodesTorrent(t, "error-status")
	loaded.muTorrent.Lock()
	loaded.PinMode = settings.PinModeAll
	loaded.muTorrent.Unlock()
	stub := &Torrent{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(1)}, Data: episodesData(t), PinMode: settings.PinModeNext}

	for name, tor := range map[string]*Torrent{"loaded": loaded, "stub": stub} {
		settings.BTsets.UseDisk = true
		if st := tor.Status(); st.PinError != "" {
			t.Fatalf("%s: pin error = %q without the no-space state", name, st.PinError)
		}
		pinNoSpaceSet(tor.Hash(), true)
		if st := tor.Status(); st.PinError != state.PinErrorNoSpace {
			t.Fatalf("%s: pin error = %q, want %q", name, st.PinError, state.PinErrorNoSpace)
		}
		settings.BTsets.UseDisk = false
		if st := tor.Status(); st.PinError != "" {
			t.Fatalf("%s: pin error = %q without disk storage", name, st.PinError)
		}
	}

	// a stub whose target files are all downloaded reports no error
	settings.BTsets.UseDisk = true
	stub.PinDownloaded = []int{1}
	if st := stub.Status(); st.PinProgress != 100 || st.PinError != "" {
		t.Fatalf("finished stub: progress %d, pin error %q", st.PinProgress, st.PinError)
	}
}

func TestAddTorrentDBStoresOnlyFileLayout(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "layout")
	SetTorrentPin(tor.Hash().HexString(), settings.PinModeNext, 1)
	completePieces(t, tor, 0, 1)
	tor.updatePinDownloaded()
	if st := tor.Status(); !st.FileStats[0].Downloaded || st.FileStats[0].Completed == 0 {
		t.Fatalf("status has no live pin state: %+v", st.FileStats[0])
	}

	tor.Data = ""
	AddTorrentDB(tor)
	data := GetTorrentDB(tor.Hash()).Data
	for _, key := range []string{`"pinned"`, `"completed"`, `"downloaded"`} {
		if strings.Contains(data, key) {
			t.Fatalf("stored data has %s: %s", key, data)
		}
	}
	if got := dataFiles(data); len(got) != 5 || got[0] != (pinFile{id: 1, path: "layout/E01.mkv", length: 32}) {
		t.Fatalf("stored file layout = %v", got)
	}
}
