package torr

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"server/settings"
)

// dbRow stores a DB row for spec with the given pin mode, without loading it.
func dbRow(t *testing.T, spec *Torrent, mode string) metainfo.Hash {
	t.Helper()
	AddTorrentDB(spec)
	if !settings.SetTorrentPin(spec.Hash(), mode, 0) {
		t.Fatal("DB row was not saved")
	}
	return spec.Hash()
}

// waitResumed polls until the torrent is loaded and its cache is pinned.
func waitResumed(t *testing.T, hash metainfo.Hash) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if bts.GetTorrent(hash) != nil {
			if c := bts.storage.GetCache(hash); c != nil && c.PinPending() {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("torrent %s was not resumed with a pinned cache", hash.HexString())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPinnedToResume(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	all := dbRow(t, &Torrent{TorrentSpec: specWithInfo(t, "all")}, settings.PinModeAll)
	next := dbRow(t, &Torrent{TorrentSpec: specWithInfo(t, "next")}, settings.PinModeNext)
	dbRow(t, &Torrent{TorrentSpec: specWithInfo(t, "unpinned")}, "")
	loaded := loadedTorrent(t, "loaded", settings.PinModeAll)
	pinRow(t, loaded, settings.PinModeAll)

	got := pinnedToResume()
	want := []metainfo.Hash{all, next}
	sortHashes := func(a, b metainfo.Hash) int { return bytes.Compare(a[:], b[:]) }
	slices.SortFunc(got, sortHashes)
	slices.SortFunc(want, sortHashes)
	if !slices.Equal(got, want) {
		t.Fatalf("pinnedToResume = %v, want %v", got, want)
	}

	settings.BTsets.UseDisk = false
	if got := pinnedToResume(); got != nil {
		t.Fatalf("pinnedToResume without disk storage = %v, want nil", got)
	}
	settings.BTsets.UseDisk = true

	bts = nil
	if got := pinnedToResume(); got != nil {
		t.Fatalf("pinnedToResume without a BTServer = %v, want nil", got)
	}
}

func TestResumePinnedLoadsPinned(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	hash := dbRow(t, &Torrent{TorrentSpec: specWithInfo(t, "resume")}, settings.PinModeAll)

	resumePinned()
	waitResumed(t, hash)
	if got := pinnedToResume(); len(got) != 0 {
		t.Fatalf("resumed torrent is still selected: %v", got)
	}
}

func TestResumePinnedSkipsLoaded(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := loadedTorrent(t, "loaded", settings.PinModeAll)
	pinRow(t, tor, settings.PinModeAll)
	expired := tor.expiredTime

	resumePinned()
	if tor.expiredTime != expired {
		t.Fatal("resumePinned extended the expiry of a loaded torrent")
	}
}

func TestStartStopResumePinned(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	t.Cleanup(StopResumePinned)
	first := dbRow(t, &Torrent{TorrentSpec: specWithInfo(t, "first")}, settings.PinModeAll)

	StartResumePinned()
	waitResumed(t, first)
	resumeMu.Lock()
	done := resumeDone
	resumeMu.Unlock()
	if done == nil {
		t.Fatal("loop is not running after Start")
	}

	StartResumePinned()
	resumeMu.Lock()
	again := resumeDone
	resumeMu.Unlock()
	if again != done {
		t.Fatal("second Start started another loop")
	}

	StopResumePinned()
	select {
	case <-done:
	default:
		t.Fatal("Stop returned before the loop exited")
	}
	if resumeStop != nil || resumeDone != nil {
		t.Fatal("Stop did not clear the loop channels")
	}
	StopResumePinned()

	second := dbRow(t, &Torrent{TorrentSpec: specWithInfo(t, "second")}, settings.PinModeAll)
	StartResumePinned()
	waitResumed(t, second)
}

func TestPinnedToResumeSkipsFinished(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	data := episodesData(t)
	finished := dbRow(t, &Torrent{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(1)}, Data: data}, settings.PinModeNext)
	unfinished := dbRow(t, &Torrent{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(2)}, Data: data}, settings.PinModeNext)
	// next N 0 targets only E01
	settings.SetTorrentPinDownloaded(finished, []int{1})
	settings.SetTorrentPinDownloaded(unfinished, []int{2})
	for _, hash := range []metainfo.Hash{finished, unfinished} {
		if err := os.MkdirAll(filepath.Join(settings.BTsets.TorrentsSavePath, hash.HexString()), 0o777); err != nil {
			t.Fatal(err)
		}
	}

	if got := pinnedToResume(); !slices.Equal(got, []metainfo.Hash{unfinished}) {
		t.Fatalf("pinnedToResume = %v, want [%v]", got, unfinished)
	}
}
