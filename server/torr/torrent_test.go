package torr

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent"

	"server/settings"
	"server/torr/state"
)

// withDiskMode switches the test BTsets (from withTorrDB) to disk storage.
func withDiskMode(t *testing.T) {
	t.Helper()
	settings.BTsets.UseDisk = true
	settings.BTsets.TorrentsSavePath = t.TempDir()
}

// expiredTorrent opens a spec on the offline client and returns a working
// torrent past its expiry with no readers, without the watch loop.
func expiredTorrent(t *testing.T, spec *torrent.TorrentSpec) *Torrent {
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

	tor := expiredTorrent(t, specWithInfo(t, "expired"))
	if !tor.expired() {
		t.Fatal("working torrent past its expiry with no readers did not expire")
	}
	tor.expiredTime = time.Now().Add(time.Minute)
	if tor.expired() {
		t.Fatal("torrent before its expiry expired")
	}
	tor.expiredTime = time.Now().Add(-time.Minute)
	tor.Stat = state.TorrentPreload
	if tor.expired() {
		t.Fatal("preloading torrent expired")
	}
}

// loadedTorrent adds a spec through NewTorrent and waits for its info.
func loadedTorrent(t *testing.T, name string) *Torrent {
	t.Helper()
	tor, err := NewTorrent(specWithInfo(t, name), bts)
	if err != nil {
		t.Fatal(err)
	}
	if !tor.WaitInfo() {
		t.Fatal("WaitInfo failed")
	}
	return tor
}
