package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"server/settings"
)

// The settings DB is a process-wide singleton that cannot be reopened after
// CloseDB, so it is opened once for the package and emptied per test.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "server-test-")
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

func testHash(b byte) metainfo.Hash {
	var h metainfo.Hash
	h[0] = b
	h[19] = b
	return h
}

// cacheDirs holds the dirs created for one cleanCache run.
type cacheDirs struct {
	inDB1   string
	inDB2   string
	notInDB string
	notHash string
}

// withCacheDirs sets a disk BTsets with a temp save path, stores two DB rows
// and creates their dirs plus a non-DB hash dir and a dir
// whose name is not a hash. Globals and the torrents table are restored.
func withCacheDirs(t *testing.T, removeCacheOnDrop bool) cacheDirs {
	t.Helper()
	oldBTsets := settings.BTsets
	settings.BTsets = &settings.BTSets{UseDisk: true, TorrentsSavePath: t.TempDir(), RemoveCacheOnDrop: removeCacheOnDrop}
	t.Cleanup(func() {
		for _, db := range settings.ListTorrent() {
			settings.RemTorrent(db.InfoHash)
		}
		settings.BTsets = oldBTsets
	})

	for _, h := range []metainfo.Hash{testHash(1), testHash(2)} {
		settings.AddTorrent(&settings.TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: h}})
	}

	dirs := cacheDirs{
		inDB1:   testHash(1).HexString(),
		inDB2:   testHash(2).HexString(),
		notInDB: testHash(3).HexString(),
		notHash: "not-a-hash",
	}
	for _, name := range []string{dirs.inDB1, dirs.inDB2, dirs.notInDB, dirs.notHash} {
		dir := filepath.Join(settings.BTsets.TorrentsSavePath, name)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "0"), []byte{1}, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	return dirs
}

func assertCacheDir(t *testing.T, name string, want bool) {
	t.Helper()
	_, err := os.Stat(filepath.Join(settings.BTsets.TorrentsSavePath, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if got := err == nil; got != want {
		t.Fatalf("dir %s exists = %v, want %v", name, got, want)
	}
}

func TestCleanCacheRemoveCacheOnDropRemovesHashDirs(t *testing.T) {
	dirs := withCacheDirs(t, true)

	cleanCache()

	assertCacheDir(t, dirs.inDB1, false)
	assertCacheDir(t, dirs.inDB2, false)
	assertCacheDir(t, dirs.notInDB, false)
	assertCacheDir(t, dirs.notHash, true)
}

func TestCleanCacheKeepsDBTorrents(t *testing.T) {
	dirs := withCacheDirs(t, false)

	cleanCache()

	assertCacheDir(t, dirs.inDB1, true)
	assertCacheDir(t, dirs.inDB2, true)
	assertCacheDir(t, dirs.notInDB, false)
	assertCacheDir(t, dirs.notHash, true)
}

func TestCleanCacheWithoutDiskRemovesNothing(t *testing.T) {
	dirs := withCacheDirs(t, true)
	settings.BTsets.UseDisk = false

	cleanCache()

	for _, name := range []string{dirs.inDB1, dirs.inDB2, dirs.notInDB, dirs.notHash} {
		assertCacheDir(t, name, true)
	}
}
