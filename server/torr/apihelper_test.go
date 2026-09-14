package torr

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"server/settings"
	"server/torr/storage/torrstor"
)

// withOfflineClient replaces bts with a BTServer backed by a real client that
// has no network sources, enough for NewTorrent on a spec with info bytes.
func withOfflineClient(t *testing.T) {
	t.Helper()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	bts.storage = torrstor.NewStorage(64 << 20)
	cfg.DefaultStorage = bts.storage
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisableTCP = true
	cfg.DisableUTP = true
	cfg.NoDefaultPortForwarding = true
	cfg.LocalServiceDiscovery = nil
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bts.client = client
	t.Cleanup(client.Close)
}

// specWithInfo builds a spec whose info is already known, so no peers are needed.
func specWithInfo(t *testing.T, name string) *torrent.TorrentSpec {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: 16384, Length: 1, Pieces: make([]byte, 20)}
	buf, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return &torrent.TorrentSpec{InfoBytes: buf, InfoHash: metainfo.HashBytes(buf)}
}

func TestCopyPin(t *testing.T) {
	// copyPin applies the pin, which reads settings.BTsets
	withTorrDB(t)
	src := &Torrent{PinMode: settings.PinModeAll, PinNext: 5, PinAnchor: 3, PinDownloaded: []int{4}}
	dst := &Torrent{Title: "keep", PinMode: settings.PinModeNext, PinNext: 1, PinAnchor: 9, PinDownloaded: []int{8}}
	copyPin(dst, src)
	assertPin(t, "dst", dst, settings.PinModeAll, 5, 3, []int{4})
	if dst.Title != "keep" {
		t.Fatalf("copyPin changed a non-pin field: %q", dst.Title)
	}
	dst.PinDownloaded[0] = 7
	if src.PinDownloaded[0] != 4 {
		t.Fatal("copyPin shares the downloaded slice with src")
	}

	copyPin(dst, &Torrent{})
	assertPin(t, "cleared", dst, "", 0, 0, nil)
}

func TestAddTorrentTakesPinFromDBWhenUnpinned(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spec := specWithInfo(t, "from-db")
	db := pinnedDBTorrent(spec.InfoHash)
	AddTorrentDB(db)

	tor, err := AddTorrent(spec, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	assertPin(t, "added", tor, settings.PinModeNext, 4, 2, []int{1, 3})

	// a re-add saved to DB keeps the pin
	SaveTorrentToDB(tor)
	assertPin(t, "saved", GetTorrentDB(spec.InfoHash), settings.PinModeNext, 4, 2, []int{1, 3})
}

func TestAddTorrentDBPinWinsOverMemory(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spec := specWithInfo(t, "db-wins")
	AddTorrentDB(pinnedDBTorrent(spec.InfoHash))

	first, err := AddTorrent(spec, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// stale in-memory pin, e.g. from a load that raced with a pin change
	first.PinMode, first.PinNext, first.PinAnchor, first.PinDownloaded = settings.PinModeAll, 9, 5, []int{6}

	again, err := AddTorrent(specWithInfo(t, "db-wins"), "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatal("re-add did not return the in-memory torrent")
	}
	assertPin(t, "re-added", again, settings.PinModeNext, 4, 2, []int{1, 3})
}

// A general save from a stale copy must not revert a pin written meanwhile
// (set racing with pin, or a stale async-load copy saved later).
func TestStaleSaveDoesNotRevertPin(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	stale := GetTorrentDB(hash)

	SetTorrentPin(hash.HexString(), settings.PinModeOff, 0)
	stale.Title = "edited"
	AddTorrentDB(stale)

	db := GetTorrentDB(hash)
	if db.Title != "edited" {
		t.Fatalf("edit not saved: %q", db.Title)
	}
	assertPin(t, "db", db, "", 0, 2, nil)
}

func TestSetTorrentPinConcurrentWithSave(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	loaded := pinnedDBTorrent(hash)
	bts.torrents[hash] = loaded
	AddTorrentDB(loaded)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			AddTorrentDB(loaded)
			copyPin(loaded, GetTorrentDB(hash))
			loaded.Status()
		}
	}()
	for i := 0; i < 50; i++ {
		SetTorrentPin(hash.HexString(), settings.PinModeAll, i)
	}
	<-done
	assertPin(t, "db", GetTorrentDB(hash), settings.PinModeAll, 49, 2, []int{1, 3})
}

func TestAddTorrentWithoutDBRowIsUnpinned(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	tor, err := AddTorrent(specWithInfo(t, "no-row"), "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	assertPin(t, "added", tor, "", 0, 0, nil)
}

func TestLoadTorrentCopiesPin(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spec := specWithInfo(t, "load")
	AddTorrentDB(pinnedDBTorrent(spec.InfoHash))
	db := GetTorrentDB(spec.InfoHash)
	db.TorrentSpec = spec

	tor := LoadTorrent(db)
	assertPin(t, "loaded", tor, settings.PinModeNext, 4, 2, []int{1, 3})
}

func TestSetTorrentKeepsPin(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	loaded := pinnedDBTorrent(hash)
	loaded.PinMode = settings.PinModeAll
	bts.torrents[hash] = loaded

	SetTorrent(hash.HexString(), "new title", "", "series", "")

	db := GetTorrentDB(hash)
	if db.Title != "new title" || db.Category != "series" {
		t.Fatalf("edit not saved: %+v", db)
	}
	assertPin(t, "db", db, settings.PinModeNext, 4, 2, []int{1, 3})
	if loaded.Title != "new title" {
		t.Fatalf("in-memory edit not applied: %q", loaded.Title)
	}
	assertPin(t, "memory", loaded, settings.PinModeAll, 4, 2, []int{1, 3})
}

func TestSetTorrentKeepsPinForDBOnlyTorrent(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))

	ret := SetTorrent(hash.HexString(), "new title", "", "", "")
	assertPin(t, "returned", ret, settings.PinModeNext, 4, 2, []int{1, 3})
	db := GetTorrentDB(hash)
	if db.Title != "new title" {
		t.Fatalf("edit not saved: %q", db.Title)
	}
	assertPin(t, "db", db, settings.PinModeNext, 4, 2, []int{1, 3})
}

func TestSetTorrentPinDBOnly(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	db := pinnedDBTorrent(hash)
	db.PinMode, db.PinNext = "", 0
	AddTorrentDB(db)

	ret := SetTorrentPin(hash.HexString(), settings.PinModeNext, 5)
	assertPin(t, "returned", ret, settings.PinModeNext, 5, 2, []int{1, 3})
	if ret.Stat.String() != "Torrent in db" {
		t.Fatalf("returned stat = %v, want DB copy", ret.Stat)
	}
	got := GetTorrentDB(hash)
	assertPin(t, "db", got, settings.PinModeNext, 5, 2, []int{1, 3})
	if got.Title != "title" || got.Data != db.Data {
		t.Fatalf("other DB fields changed: %+v", got)
	}
	if bts.GetTorrent(hash) != nil {
		t.Fatal("SetTorrentPin loaded the torrent")
	}
}

func TestSetTorrentPinLoadedWithoutDBRow(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	loaded := pinnedDBTorrent(hash)
	loaded.PinMode, loaded.PinNext = "", 0
	bts.torrents[hash] = loaded

	ret := SetTorrentPin(hash.HexString(), settings.PinModeAll, 2)
	if ret != loaded {
		t.Fatal("SetTorrentPin did not return the loaded torrent")
	}
	assertPin(t, "memory", loaded, settings.PinModeAll, 2, 2, []int{1, 3})
	assertPin(t, "db", GetTorrentDB(hash), settings.PinModeAll, 2, 2, []int{1, 3})
}

func TestSetTorrentPinLoadedWithDBRow(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	loaded := pinnedDBTorrent(hash)
	loaded.Title = "memory title"
	bts.torrents[hash] = loaded

	ret := SetTorrentPin(hash.HexString(), settings.PinModeAll, 0)
	if ret != loaded {
		t.Fatal("SetTorrentPin did not return the loaded torrent")
	}
	assertPin(t, "memory", loaded, settings.PinModeAll, 0, 2, []int{1, 3})
	db := GetTorrentDB(hash)
	assertPin(t, "db", db, settings.PinModeAll, 0, 2, []int{1, 3})
	if db.Title != "title" {
		t.Fatalf("DB row rewritten from memory: title %q", db.Title)
	}
}

func TestSetTorrentPinOffClearsModeAndNext(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	loaded := pinnedDBTorrent(hash)
	bts.torrents[hash] = loaded

	pinNoSpaceSet(hash, true)

	SetTorrentPin(hash.HexString(), settings.PinModeOff, 7)

	// off also clears the downloaded ids and the no-space state
	assertPin(t, "memory", loaded, "", 0, 2, nil)
	assertPin(t, "db", GetTorrentDB(hash), "", 0, 2, nil)
	if pinNoSpaceHas(hash) {
		t.Fatal("SetTorrentPin(off) kept the no-space state of a loaded torrent")
	}
}

func TestSetTorrentPinUnknownHash(t *testing.T) {
	withTorrDB(t)
	if ret := SetTorrentPin(testHash(9).HexString(), settings.PinModeAll, 1); ret != nil {
		t.Fatalf("SetTorrentPin returned %+v for an unknown hash", ret)
	}
	if n := len(settings.ListTorrent()); n != 0 {
		t.Fatalf("DB has %d rows, want 0", n)
	}
}

func TestSetTorrentPinReadOnly(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	loaded := pinnedDBTorrent(hash)
	bts.torrents[hash] = loaded
	settings.ReadOnly = true

	if ret := SetTorrentPin(hash.HexString(), settings.PinModeAll, 1); ret != nil {
		t.Fatalf("SetTorrentPin returned %+v in read-only mode", ret)
	}
	assertPin(t, "memory", loaded, settings.PinModeNext, 4, 2, []int{1, 3})
	assertPin(t, "db", GetTorrentDB(hash), settings.PinModeNext, 4, 2, []int{1, 3})
}

// First pin of a loaded torrent without a DB row, racing an ordinary save
// that creates the row: the requested pin must end up in DB.
func TestSetTorrentPinFirstPinRacingSave(t *testing.T) {
	withTorrDB(t)
	for i := 0; i < 100; i++ {
		var hash metainfo.Hash
		hash[0], hash[1], hash[19] = 7, byte(i), 7
		loaded := pinnedDBTorrent(hash)
		loaded.PinMode, loaded.PinNext = "", 0
		bts.torrents[hash] = loaded

		done := make(chan struct{})
		go func() {
			defer close(done)
			AddTorrentDB(loaded)
		}()
		SetTorrentPin(hash.HexString(), settings.PinModeAll, 6)
		<-done

		if db := GetTorrentDB(hash); db == nil || db.PinMode != settings.PinModeAll || db.PinNext != 6 {
			t.Fatalf("iteration %d: DB pin = %+v, want all/6", i, db)
		}
	}
}

// hashDir creates the disk cache dir of hash with one file and returns its path.
func hashDir(t *testing.T, hash metainfo.Hash) string {
	t.Helper()
	dir := filepath.Join(settings.BTsets.TorrentsSavePath, hash.HexString())
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0"), []byte{1}, 0o666); err != nil {
		t.Fatal(err)
	}
	return dir
}

func dirExists(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Stat(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// waitDirGone polls an asynchronous removal with a deadline.
func waitDirGone(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for dirExists(t, dir) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still exists", dir)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pinRow stores a DB row for tor with the given pin mode.
func pinRow(t *testing.T, tor *Torrent, mode string) {
	t.Helper()
	AddTorrentDB(tor)
	if !settings.SetTorrentPin(tor.Hash(), mode, 0) {
		t.Fatal("DB row was not saved")
	}
}

// loadedWithPieceFile loads a torrent with a pinned DB row and writes its piece
// file, returning the hash dir.
func loadedWithPieceFile(t *testing.T, name string) (*Torrent, string) {
	t.Helper()
	tor := loadedTorrent(t, name, settings.PinModeAll)
	pinRow(t, tor, settings.PinModeAll)
	if _, err := tor.GetCache().Piece(tor.Info().Piece(0)).WriteAt([]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(settings.BTsets.TorrentsSavePath, tor.Hash().HexString())
	if !dirExists(t, filepath.Join(dir, "0")) {
		t.Fatal("piece file was not written")
	}
	return tor, dir
}

func TestRemTorrentNotLoadedRemovesDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	dir := hashDir(t, hash)
	pinNoSpaceSet(hash, true)

	RemTorrent(hash.HexString())
	if dirExists(t, dir) {
		t.Fatal("RemTorrent kept the dir of a DB torrent")
	}
	if GetTorrentDB(hash) != nil {
		t.Fatal("RemTorrent kept the DB row")
	}
	if pinNoSpaceHas(hash) {
		t.Fatal("RemTorrent kept the no-space state of a DB torrent")
	}
}

func TestRemTorrentNotLoadedWithoutDiskKeepsDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	dir := hashDir(t, hash)
	settings.BTsets.UseDisk = false

	RemTorrent(hash.HexString())
	if !dirExists(t, dir) {
		t.Fatal("RemTorrent removed a dir without disk storage")
	}
	if GetTorrentDB(hash) != nil {
		t.Fatal("RemTorrent kept the DB row")
	}
}

func TestRemTorrentLoadedRemovesDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor, dir := loadedWithPieceFile(t, "rem-loaded")
	pinNoSpaceSet(tor.Hash(), true)

	RemTorrent(tor.Hash().HexString())
	if dirExists(t, dir) {
		t.Fatal("RemTorrent kept the dir of a loaded torrent")
	}
	if GetTorrentDB(tor.Hash()) != nil {
		t.Fatal("RemTorrent kept the DB row")
	}
	if pinNoSpaceHas(tor.Hash()) {
		t.Fatal("RemTorrent kept the no-space state of a loaded torrent")
	}
}

func TestSetTorrentPinOffDBOnlyRemovesDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	dir := hashDir(t, hash)

	pinNoSpaceSet(hash, true)

	if ret := SetTorrentPin(hash.HexString(), settings.PinModeOff, 0); ret == nil {
		t.Fatal("SetTorrentPin(off) returned nil for a DB torrent")
	}
	if dirExists(t, dir) {
		t.Fatal("SetTorrentPin(off) kept the dir of a DB torrent")
	}
	assertPin(t, "db", GetTorrentDB(hash), "", 0, 2, nil)
	if pinNoSpaceHas(hash) {
		t.Fatal("SetTorrentPin(off) kept the no-space state of a DB torrent")
	}
}

func TestSetTorrentPinOffUnknownHashKeepsDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(9)
	dir := hashDir(t, hash)

	if ret := SetTorrentPin(hash.HexString(), settings.PinModeOff, 0); ret != nil {
		t.Fatalf("SetTorrentPin(off) returned %+v for an unknown hash", ret)
	}
	if !dirExists(t, dir) {
		t.Fatal("SetTorrentPin(off) removed the dir of an unknown hash")
	}
}

func TestSetTorrentPinOnKeepsDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	dir := hashDir(t, hash)

	for _, mode := range []string{settings.PinModeAll, settings.PinModeNext} {
		SetTorrentPin(hash.HexString(), mode, 1)
		if !dirExists(t, dir) {
			t.Fatalf("SetTorrentPin(%s) removed the dir", mode)
		}
	}
}

func TestSetTorrentPinOffWithoutDiskKeepsDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(1)
	AddTorrentDB(pinnedDBTorrent(hash))
	dir := hashDir(t, hash)
	settings.BTsets.UseDisk = false

	SetTorrentPin(hash.HexString(), settings.PinModeOff, 0)
	if !dirExists(t, dir) {
		t.Fatal("SetTorrentPin(off) removed a dir without disk storage")
	}
}

func TestSetTorrentPinOffLoadedRemovesDirAfterClose(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor, dir := loadedWithPieceFile(t, "off-loaded")
	hash := tor.Hash()

	if ret := SetTorrentPin(hash.HexString(), settings.PinModeOff, 0); ret != tor {
		t.Fatal("SetTorrentPin(off) did not return the loaded torrent")
	}
	if !dirExists(t, filepath.Join(dir, "0")) {
		t.Fatal("SetTorrentPin(off) removed the dir of a loaded torrent")
	}

	if !bts.RemoveTorrent(hash) {
		t.Fatal("RemoveTorrent failed")
	}
	waitDirGone(t, dir)
}

// A settings reload drops torrents without removing them from the map.
func TestSetTorrentPinOffLoadedRemovesDirAfterDropAll(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor, dir := loadedWithPieceFile(t, "off-drop-all")

	SetTorrentPin(tor.Hash().HexString(), settings.PinModeOff, 0)
	dropAllTorrent()
	waitDirGone(t, dir)
}

func TestRemoveTorrentDirAfterCloseRechecks(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor, dir := loadedWithPieceFile(t, "after-close")
	hash := tor.Hash()
	settings.SetTorrentPin(hash, "", 0)
	if !bts.RemoveTorrent(hash) {
		t.Fatal("RemoveTorrent failed")
	}
	<-tor.closed
	tor.PinMode = ""

	// pinned again on the loaded torrent: the mirror is checked, not only the DB
	tor.PinMode = settings.PinModeAll
	removeTorrentDirAfterClose(tor, hash, dir)
	if !dirExists(t, dir) {
		t.Fatal("removed the dir of a torrent whose mirror is pinned again")
	}
	tor.PinMode = ""

	settings.SetTorrentPin(hash, settings.PinModeNext, 1)
	removeTorrentDirAfterClose(tor, hash, dir)
	if !dirExists(t, dir) {
		t.Fatal("removed the dir of a torrent pinned again")
	}
	settings.SetTorrentPin(hash, "", 0)

	// another torrent under the same hash: loaded again
	bts.mu.Lock()
	bts.torrents[hash] = &Torrent{}
	bts.mu.Unlock()
	removeTorrentDirAfterClose(tor, hash, dir)
	if !dirExists(t, dir) {
		t.Fatal("removed the dir of a torrent loaded again")
	}

	// the closed torrent itself left in the map, as drop() and client.Close do
	bts.mu.Lock()
	bts.torrents[hash] = tor
	bts.mu.Unlock()

	removeTorrentDirAfterClose(tor, hash, dir)
	if dirExists(t, dir) {
		t.Fatal("kept the dir of a closed unpinned torrent")
	}
}

// C1: pinning a loaded torrent without metadata and without a DB row.
func TestSetTorrentPinLoadedWithoutInfo(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	hash := testHash(5)
	tor, err := NewTorrent(&torrent.TorrentSpec{InfoHash: hash}, bts)
	if err != nil {
		t.Fatal(err)
	}
	if tor.Torrent.Info() != nil {
		t.Fatal("torrent without info bytes has info")
	}

	if ret := SetTorrentPin(hash.HexString(), settings.PinModeAll, 0); ret != tor {
		t.Fatal("SetTorrentPin did not return the loaded torrent")
	}
	db := GetTorrentDB(hash)
	if db == nil || db.PinMode != settings.PinModeAll || db.Size != 0 {
		t.Fatalf("DB row = %+v, want mode all and size 0", db)
	}
}

// C2: off removes nothing when the torrent was not pinned before.
func TestSetTorrentPinOffUnpinnedDBOnlyKeepsDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(1)
	tor := pinnedDBTorrent(hash)
	tor.PinMode, tor.PinNext = "", 0
	AddTorrentDB(tor)
	dir := hashDir(t, hash)

	if ret := SetTorrentPin(hash.HexString(), settings.PinModeOff, 0); ret == nil {
		t.Fatal("SetTorrentPin(off) returned nil for a DB torrent")
	}
	if !dirExists(t, dir) {
		t.Fatal("SetTorrentPin(off) removed the dir of an unpinned DB torrent")
	}
}

func TestSetTorrentPinOffUnpinnedLoadedKeepsDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := loadedTorrent(t, "off-unpinned", "")
	pinRow(t, tor, "")
	if _, err := tor.GetCache().Piece(tor.Info().Piece(0)).WriteAt([]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(settings.BTsets.TorrentsSavePath, tor.Hash().HexString())

	if ret := SetTorrentPin(tor.Hash().HexString(), settings.PinModeOff, 0); ret != tor {
		t.Fatal("SetTorrentPin(off) did not return the loaded torrent")
	}
	// ordering barrier: a pinned torrent switched off and closed after it has
	// its dir removed by the same deferred path, so a delete scheduled for the
	// unpinned torrent would have run by then
	control, controlDir := loadedWithPieceFile(t, "off-unpinned-control")
	SetTorrentPin(control.Hash().HexString(), settings.PinModeOff, 0)
	if !bts.RemoveTorrent(tor.Hash()) {
		t.Fatal("RemoveTorrent failed")
	}
	<-tor.closed
	if !bts.RemoveTorrent(control.Hash()) {
		t.Fatal("RemoveTorrent of the control torrent failed")
	}
	waitDirGone(t, controlDir)
	if !dirExists(t, filepath.Join(dir, "0")) {
		t.Fatal("the dir of an unpinned loaded torrent was removed")
	}
}

// A loaded torrent pinned only in memory was pinned before off.
func TestSetTorrentPinOffMirrorPinnedRemovesDirAfterClose(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor, dir := loadedWithPieceFile(t, "off-mirror")
	if !settings.SetTorrentPin(tor.Hash(), "", 0) {
		t.Fatal("DB row was not saved")
	}

	SetTorrentPin(tor.Hash().HexString(), settings.PinModeOff, 0)
	if !bts.RemoveTorrent(tor.Hash()) {
		t.Fatal("RemoveTorrent failed")
	}
	waitDirGone(t, dir)
}
