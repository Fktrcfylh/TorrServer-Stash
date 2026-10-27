package torr

import (
	"os"
	"path/filepath"
	"testing"

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

// loadedWithPieceFile loads a torrent with a DB row and writes its piece
// file, returning the hash dir.
func loadedWithPieceFile(t *testing.T, name string) (*Torrent, string) {
	t.Helper()
	tor := loadedTorrent(t, name)
	AddTorrentDB(tor)
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
	AddTorrentDB(dbTorrent(hash))
	dir := hashDir(t, hash)

	RemTorrent(hash.HexString())
	if dirExists(t, dir) {
		t.Fatal("RemTorrent kept the dir of a DB torrent")
	}
	if GetTorrentDB(hash) != nil {
		t.Fatal("RemTorrent kept the DB row")
	}
}

func TestRemTorrentNotLoadedWithoutDiskKeepsDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))
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

	RemTorrent(tor.Hash().HexString())
	if dirExists(t, dir) {
		t.Fatal("RemTorrent kept the dir of a loaded torrent")
	}
	if GetTorrentDB(tor.Hash()) != nil {
		t.Fatal("RemTorrent kept the DB row")
	}
}

func TestRemTorrentReadOnlyKeepsRowAndDir(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))
	dir := hashDir(t, hash)
	settings.ReadOnly = true

	RemTorrent(hash.HexString())
	settings.ReadOnly = false
	if !dirExists(t, dir) {
		t.Fatal("read-only RemTorrent removed the dir")
	}
	if GetTorrentDB(hash) == nil {
		t.Fatal("read-only RemTorrent removed the DB row")
	}
}
