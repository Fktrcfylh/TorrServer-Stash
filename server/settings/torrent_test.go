package settings

import (
	"encoding/json"
	"slices"
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

func TestTorrentDBPinJSONRoundTrip(t *testing.T) {
	in := &TorrentDB{
		TorrentSpec:   &torrent.TorrentSpec{InfoHash: testHash(1)},
		Title:         "t",
		PinMode:       PinModeNext,
		PinNext:       4,
		PinAnchor:     2,
		PinDownloaded: []int{1, 3},
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out TorrentDB
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	if out.PinMode != PinModeNext || out.PinNext != 4 || out.PinAnchor != 2 || !slices.Equal(out.PinDownloaded, []int{1, 3}) {
		t.Fatalf("round trip lost pin: %s -> %+v", buf, out)
	}
}

func TestTorrentDBWithoutPinKeysDecodesOff(t *testing.T) {
	var out TorrentDB
	if err := json.Unmarshal([]byte(`{"title":"old","data":"x","timestamp":5}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.PinMode != "" || out.PinNext != 0 || out.PinAnchor != 0 || out.PinDownloaded != nil {
		t.Fatalf("old row decoded with pin: %+v", out)
	}
	buf, err := json.Marshal(&TorrentDB{Title: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != `{"title":"old"}` {
		t.Fatalf("unpinned row JSON changed: %s", buf)
	}
}

func TestSetTorrentPinUpdatesOnlyModeAndNext(t *testing.T) {
	withTestDB(t)
	hash := testHash(1)
	other := testHash(2)
	AddTorrent(&TorrentDB{
		TorrentSpec:   &torrent.TorrentSpec{InfoHash: hash},
		Title:         "title",
		Category:      "movie",
		Data:          "data",
		Timestamp:     10,
		Size:          20,
		PinMode:       PinModeAll,
		PinNext:       1,
		PinAnchor:     7,
		PinDownloaded: []int{2, 5},
	})
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: other}, Title: "other"})

	if !SetTorrentPin(hash, PinModeNext, 6) {
		t.Fatal("SetTorrentPin returned false for an existing row")
	}
	got := findRow(t, hash)
	if got == nil {
		t.Fatal("row disappeared")
	}
	if got.PinMode != PinModeNext || got.PinNext != 6 {
		t.Fatalf("pin not updated: mode=%q next=%d", got.PinMode, got.PinNext)
	}
	if got.Title != "title" || got.Category != "movie" || got.Data != "data" || got.Timestamp != 10 || got.Size != 20 ||
		got.PinAnchor != 7 || !slices.Equal(got.PinDownloaded, []int{2, 5}) {
		t.Fatalf("other fields changed: %+v", got)
	}
	if o := findRow(t, other); o == nil || o.Title != "other" || o.PinMode != "" || o.PinNext != 0 {
		t.Fatalf("unrelated row changed: %+v", o)
	}
}

func TestSetTorrentPinAnchorUpdatesOnlyAnchor(t *testing.T) {
	withTestDB(t)
	hash := testHash(1)
	other := testHash(2)
	AddTorrent(&TorrentDB{
		TorrentSpec:   &torrent.TorrentSpec{InfoHash: hash},
		Title:         "title",
		Category:      "movie",
		Data:          "data",
		Timestamp:     10,
		Size:          20,
		PinMode:       PinModeNext,
		PinNext:       2,
		PinAnchor:     1,
		PinDownloaded: []int{2, 5},
	})
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: other}, Title: "other"})

	if !SetTorrentPinAnchor(hash, 4) {
		t.Fatal("SetTorrentPinAnchor returned false for an existing row")
	}
	got := findRow(t, hash)
	if got == nil {
		t.Fatal("row disappeared")
	}
	if got.PinAnchor != 4 {
		t.Fatalf("anchor = %d, want 4", got.PinAnchor)
	}
	if got.Title != "title" || got.Category != "movie" || got.Data != "data" || got.Timestamp != 10 || got.Size != 20 ||
		got.PinMode != PinModeNext || got.PinNext != 2 || !slices.Equal(got.PinDownloaded, []int{2, 5}) {
		t.Fatalf("other fields changed: %+v", got)
	}
	if o := findRow(t, other); o == nil || o.Title != "other" || o.PinAnchor != 0 {
		t.Fatalf("unrelated row changed: %+v", o)
	}
}

func TestSetTorrentPinAnchorMissingRow(t *testing.T) {
	withTestDB(t)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(1)}, Title: "kept"})
	before, _ := json.Marshal(ListTorrent())

	if SetTorrentPinAnchor(testHash(9), 3) {
		t.Fatal("SetTorrentPinAnchor returned true for a missing row")
	}
	after, _ := json.Marshal(ListTorrent())
	if string(before) != string(after) {
		t.Fatalf("DB changed:\nbefore %s\nafter  %s", before, after)
	}
	if findRow(t, testHash(9)) != nil {
		t.Fatal("row created for a missing hash")
	}
}

func TestSetTorrentPinMissingRow(t *testing.T) {
	withTestDB(t)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(1)}, Title: "kept"})
	before, _ := json.Marshal(ListTorrent())

	if SetTorrentPin(testHash(9), PinModeAll, 2) {
		t.Fatal("SetTorrentPin returned true for a missing row")
	}
	after, _ := json.Marshal(ListTorrent())
	if string(before) != string(after) {
		t.Fatalf("DB changed:\nbefore %s\nafter  %s", before, after)
	}
}

func TestAddTorrentKeepsStoredPin(t *testing.T) {
	withTestDB(t)
	hash := testHash(1)
	AddTorrent(&TorrentDB{
		TorrentSpec:   &torrent.TorrentSpec{InfoHash: hash},
		Title:         "new",
		PinMode:       PinModeNext,
		PinNext:       2,
		PinAnchor:     4,
		PinDownloaded: []int{4, 5},
	})
	if got := findRow(t, hash); got == nil || got.PinMode != PinModeNext || got.PinNext != 2 {
		t.Fatalf("new row did not take the given pin: %+v", got)
	}

	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}, Title: "edited", PinMode: PinModeAll, PinNext: 9})
	got := findRow(t, hash)
	if got.Title != "edited" {
		t.Fatalf("edit not saved: %q", got.Title)
	}
	if got.PinMode != PinModeNext || got.PinNext != 2 || got.PinAnchor != 4 || !slices.Equal(got.PinDownloaded, []int{4, 5}) {
		t.Fatalf("general save changed the stored pin: %+v", got)
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

func TestIsTorrentPinned(t *testing.T) {
	withTestDB(t)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(1)}})
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(2)}})
	SetTorrentPin(testHash(2), PinModeAll, 0)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(3)}})
	SetTorrentPin(testHash(3), PinModeNext, 2)
	before, _ := json.Marshal(ListTorrent())

	if IsTorrentPinned(testHash(9)) {
		t.Fatal("missing row reported as pinned")
	}
	if IsTorrentPinned(testHash(1)) {
		t.Fatal("unpinned row reported as pinned")
	}
	if !IsTorrentPinned(testHash(2)) {
		t.Fatal("row pinned with all not reported as pinned")
	}
	if !IsTorrentPinned(testHash(3)) {
		t.Fatal("row pinned with next not reported as pinned")
	}
	after, _ := json.Marshal(ListTorrent())
	if string(before) != string(after) {
		t.Fatalf("DB changed:\nbefore %s\nafter  %s", before, after)
	}
}

func TestSetTorrentPinDownloadedUpdatesOnlyDownloaded(t *testing.T) {
	withTestDB(t)
	hash := testHash(1)
	AddTorrent(&TorrentDB{
		TorrentSpec:   &torrent.TorrentSpec{InfoHash: hash},
		Title:         "title",
		Data:          "data",
		PinMode:       PinModeNext,
		PinNext:       2,
		PinAnchor:     3,
		PinDownloaded: []int{1},
	})

	if !SetTorrentPinDownloaded(hash, []int{3, 4}) {
		t.Fatal("SetTorrentPinDownloaded returned false for an existing row")
	}
	got := findRow(t, hash)
	if !slices.Equal(got.PinDownloaded, []int{3, 4}) {
		t.Fatalf("downloaded = %v, want [3 4]", got.PinDownloaded)
	}
	if got.Title != "title" || got.Data != "data" || got.PinMode != PinModeNext || got.PinNext != 2 || got.PinAnchor != 3 {
		t.Fatalf("other fields changed: %+v", got)
	}

	if !SetTorrentPinDownloaded(hash, nil) {
		t.Fatal("SetTorrentPinDownloaded(nil) returned false for an existing row")
	}
	if got := findRow(t, hash); got.PinDownloaded != nil || got.PinMode != PinModeNext {
		t.Fatalf("clear downloaded: %+v", got)
	}
}

func TestSetTorrentPinDownloadedUnpinnedRow(t *testing.T) {
	withTestDB(t)
	hash := testHash(1)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}, Title: "unpinned", PinAnchor: 2})

	if SetTorrentPinDownloaded(hash, []int{1}) {
		t.Fatal("SetTorrentPinDownloaded returned true for an unpinned row")
	}
	if got := findRow(t, hash); got.PinDownloaded != nil {
		t.Fatalf("unpinned row downloaded = %v", got.PinDownloaded)
	}

	// pin off racing a downloaded save: the save after off stores nothing
	SetTorrentPin(hash, PinModeNext, 1)
	SetTorrentPinDownloaded(hash, []int{1})
	SetTorrentPin(hash, "", 0)
	if SetTorrentPinDownloaded(hash, []int{1}) {
		t.Fatal("SetTorrentPinDownloaded returned true after pin off")
	}
	if got := findRow(t, hash); got.PinDownloaded != nil || got.PinMode != "" {
		t.Fatalf("row after off and late save: %+v", got)
	}
}

func TestSetTorrentPinDownloadedMissingRow(t *testing.T) {
	withTestDB(t)
	AddTorrent(&TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(1)}, Title: "kept"})
	before, _ := json.Marshal(ListTorrent())

	if SetTorrentPinDownloaded(testHash(9), []int{1}) {
		t.Fatal("SetTorrentPinDownloaded returned true for a missing row")
	}
	after, _ := json.Marshal(ListTorrent())
	if string(before) != string(after) {
		t.Fatalf("DB changed:\nbefore %s\nafter  %s", before, after)
	}
	if findRow(t, testHash(9)) != nil {
		t.Fatal("row created for a missing hash")
	}
}

func TestSetTorrentPinOffClearsDownloaded(t *testing.T) {
	withTestDB(t)
	hash := testHash(1)
	AddTorrent(&TorrentDB{
		TorrentSpec:   &torrent.TorrentSpec{InfoHash: hash},
		PinMode:       PinModeAll,
		PinAnchor:     3,
		PinDownloaded: []int{1, 2},
	})

	if !SetTorrentPin(hash, PinModeNext, 1) {
		t.Fatal("SetTorrentPin(next) returned false")
	}
	if got := findRow(t, hash); !slices.Equal(got.PinDownloaded, []int{1, 2}) || got.PinAnchor != 3 {
		t.Fatalf("next changed downloaded or anchor: %+v", got)
	}

	if !SetTorrentPin(hash, "", 0) {
		t.Fatal("SetTorrentPin(off) returned false")
	}
	got := findRow(t, hash)
	if got.PinDownloaded != nil || got.PinMode != "" {
		t.Fatalf("off kept downloaded: %+v", got)
	}
	if got.PinAnchor != 3 {
		t.Fatalf("off changed the anchor to %d", got.PinAnchor)
	}
}
