package torr

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"

	"server/settings"
	"server/torr/state"
)

// withDiskMode switches the test BTsets (from withTorrDB) to disk storage with
// plenty of free space in the save path.
func withDiskMode(t *testing.T) {
	t.Helper()
	settings.BTsets.UseDisk = true
	settings.BTsets.TorrentsSavePath = t.TempDir()
	withFreeSpace(t, map[string]uint64{settings.BTsets.TorrentsSavePath: 1 << 50})
}

// freeSpaceDouble replaces freeSpace: it returns the free space of a known
// path, 0, false for other paths, and records the paths it is called with.
type freeSpaceDouble struct {
	mu    sync.Mutex
	free  map[string]uint64
	paths []string
}

// withFreeSpace installs a freeSpace double and restores freeSpace afterwards.
func withFreeSpace(t *testing.T, free map[string]uint64) *freeSpaceDouble {
	t.Helper()
	d := &freeSpaceDouble{free: free}
	old := freeSpace
	freeSpace = func(path string) (uint64, bool) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.paths = append(d.paths, path)
		v, ok := d.free[path]
		return v, ok
	}
	t.Cleanup(func() { freeSpace = old })
	return d
}

func (d *freeSpaceDouble) set(path string, free uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.free[path] = free
}

func (d *freeSpaceDouble) calledWith(path string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.paths, path)
}

// expiredTorrent opens a spec on the offline client and returns a working
// torrent past its expiry with no readers, without the watch loop.
func expiredTorrent(t *testing.T, spec *torrent.TorrentSpec, mode string) *Torrent {
	t.Helper()
	goTorrent, _, err := bts.client.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	cache := bts.storage.GetCache(spec.InfoHash)
	if cache == nil {
		t.Fatal("storage has no cache for the torrent")
	}
	return &Torrent{
		TorrentSpec: spec,
		Torrent:     goTorrent,
		Stat:        state.TorrentWorking,
		PinMode:     mode,
		cache:       cache,
		expiredTime: time.Now().Add(-time.Minute),
	}
}

func TestExpired(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)

	if (&Torrent{Stat: state.TorrentWorking}).expired() {
		t.Fatal("torrent without cache expired")
	}

	unpinned := expiredTorrent(t, specWithInfo(t, "unpinned"), "")
	unpinned.applyPin()
	if !unpinned.expired() {
		t.Fatal("unpinned torrent did not expire")
	}

	// next with N 0 wants only the pieces 0 and 1 of the first episode
	next := expiredTorrent(t, episodesSpec(t, "next"), settings.PinModeNext)
	next.applyPin()
	if next.expired() {
		t.Fatal("torrent pinned with next and an incomplete window expired")
	}
	next.cache.Piece(next.Torrent.Info().Piece(0)).MarkComplete()
	if next.expired() {
		t.Fatal("torrent pinned with next and one incomplete window piece expired")
	}
	next.cache.Piece(next.Torrent.Info().Piece(1)).MarkComplete()
	if !next.expired() {
		t.Fatal("torrent pinned with next and a complete window did not expire")
	}

	pinned := expiredTorrent(t, specWithInfo(t, "all"), settings.PinModeAll)
	pinned.applyPin()
	if pinned.expired() {
		t.Fatal("torrent pinned with all and incomplete pieces expired")
	}
	for i := 0; i < pinned.Torrent.NumPieces(); i++ {
		pinned.cache.Piece(pinned.Torrent.Info().Piece(i)).MarkComplete()
	}
	if !pinned.expired() {
		t.Fatal("torrent pinned with all and every piece complete did not expire")
	}

	settings.BTsets.UseDisk = false
	memory := expiredTorrent(t, specWithInfo(t, "all-memory"), settings.PinModeAll)
	memory.applyPin()
	if !memory.expired() {
		t.Fatal("torrent pinned with all without disk storage did not expire")
	}
}

// loadedTorrent adds a spec through NewTorrent and waits for its info.
func loadedTorrent(t *testing.T, name, mode string) *Torrent {
	t.Helper()
	tor, err := NewTorrent(specWithInfo(t, name), bts)
	if err != nil {
		t.Fatal(err)
	}
	tor.PinMode = mode
	if !tor.WaitInfo() {
		t.Fatal("WaitInfo failed")
	}
	return tor
}

func TestWaitInfoAppliesPin(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)

	if !loadedTorrent(t, "wait-all", settings.PinModeAll).GetCache().PinPending() {
		t.Fatal("WaitInfo did not pin the cache of a torrent pinned with all")
	}
	if loadedTorrent(t, "wait-off", "").GetCache().PinPending() {
		t.Fatal("WaitInfo pinned the cache of an unpinned torrent")
	}
}

func TestSetTorrentPinAppliesPinToLoaded(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := loadedTorrent(t, "set-pin", "")
	cache := tor.GetCache()

	SetTorrentPin(tor.Hash().HexString(), settings.PinModeAll, 0)
	if !cache.PinPending() {
		t.Fatal("SetTorrentPin(all) did not pin the loaded cache")
	}

	SetTorrentPin(tor.Hash().HexString(), settings.PinModeOff, 0)
	if cache.PinPending() {
		t.Fatal("SetTorrentPin(off) did not unpin the loaded cache")
	}
}

func TestCopyPinAppliesPinToLoaded(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := loadedTorrent(t, "copy-pin", "")

	// a torrent without episodes pinned with next is pinned like all
	copyPin(tor, &Torrent{PinMode: settings.PinModeNext, PinNext: 2})
	if !tor.GetCache().PinPending() {
		t.Fatal("copyPin of mode next did not pin the loaded cache")
	}

	copyPin(tor, &Torrent{})
	if tor.GetCache().PinPending() {
		t.Fatal("copyPin of an empty pin did not unpin the loaded cache")
	}

	copyPin(tor, &Torrent{PinMode: settings.PinModeAll})
	if !tor.GetCache().PinPending() {
		t.Fatal("copyPin of mode all did not pin the loaded cache")
	}
}
