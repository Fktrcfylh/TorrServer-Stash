package torrstor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"server/settings"
)

const testPieceLength = 16384

// The settings DB is a process-wide singleton that cannot be reopened after
// CloseDB, so it is opened once for the package. The globals are restored
// before the tests run, so tests that rely on their own BTsets are unaffected.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "torrstor-test-")
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

// withDiskSettings switches settings.BTsets to disk mode for one test.
func withDiskSettings(t *testing.T) {
	t.Helper()
	old := settings.BTsets
	settings.BTsets = &settings.BTSets{
		UseDisk:          true,
		TorrentsSavePath: t.TempDir(),
		ConnectionsLimit: 25,
		ReaderReadAHead:  95,
	}
	t.Cleanup(func() { settings.BTsets = old })
}

// newTestCache opens a torrent of numPieces full pieces on a real offline
// client whose storage is a torrstor.Storage with the given capacity.
func newTestCache(t *testing.T, numPieces int, capacity int64) (*Cache, *torrent.Torrent) {
	t.Helper()
	withDiskSettings(t)

	info := metainfo.Info{
		Name:        "cache",
		PieceLength: testPieceLength,
		Length:      int64(numPieces) * testPieceLength,
		Pieces:      make([]byte, 20*numPieces),
	}
	buf, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	stor := NewStorage(capacity)
	cfg.DefaultStorage = stor
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
	t.Cleanup(client.Close)

	hash := metainfo.HashBytes(buf)
	tor, _, err := client.AddTorrentSpec(&torrent.TorrentSpec{InfoBytes: buf, InfoHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	c := stor.GetCache(hash)
	if c == nil {
		t.Fatal("storage has no cache for the torrent")
	}
	c.SetTorrent(tor)
	return c, tor
}

// fillPiece writes a full piece and sets its access time, lower is older.
func fillPiece(t *testing.T, c *Cache, id int, accessed int64) {
	t.Helper()
	p := c.pieces[id]
	if _, err := p.WriteAt(make([]byte, testPieceLength), 0); err != nil {
		t.Fatal(err)
	}
	p.Accessed = accessed
}

func pieceFileExists(t *testing.T, c *Cache, id int) bool {
	t.Helper()
	_, err := os.Stat(c.pieces[id].dPiece.name)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// completePiece marks a filled piece complete in the cache and in anacrolix.
func completePiece(t *testing.T, c *Cache, tor *torrent.Torrent, id int) {
	t.Helper()
	fillPiece(t, c, id, 1)
	c.pieces[id].MarkComplete()
	tor.Piece(id).UpdateCompletion()
	if !tor.PieceState(id).Complete {
		t.Fatalf("piece %d is not complete in anacrolix", id)
	}
}

// assertPriority takes the priority as any: its anacrolix type is unexported.
func assertPriority(t *testing.T, tor *torrent.Torrent, id int, want any) {
	t.Helper()
	if got := any(tor.PieceState(id).Priority); got != want {
		t.Fatalf("piece %d priority = %v, want %v", id, got, want)
	}
}

func TestCleanPiecesEvictsOldest(t *testing.T) {
	c, _ := newTestCache(t, 6, 2*testPieceLength)
	for id := 0; id < 6; id++ {
		fillPiece(t, c, id, int64(id+1))
	}

	c.cleanPieces()

	// (6-2)*L/L+1 = 5 oldest pieces go, the newest stays
	for id := 0; id < 5; id++ {
		if pieceFileExists(t, c, id) || c.pieces[id].Size != 0 {
			t.Fatalf("piece %d not evicted: size %d", id, c.pieces[id].Size)
		}
	}
	if !pieceFileExists(t, c, 5) || c.pieces[5].Size != testPieceLength {
		t.Fatalf("newest piece evicted: size %d", c.pieces[5].Size)
	}
}

func TestRemovePieceReleases(t *testing.T) {
	c, _ := newTestCache(t, 2, 2*testPieceLength)
	fillPiece(t, c, 0, 1)

	c.removePiece(c.pieces[0])

	if pieceFileExists(t, c, 0) || c.pieces[0].Size != 0 {
		t.Fatalf("piece kept: size %d", c.pieces[0].Size)
	}
}

func TestClearPriorityNoReaders(t *testing.T) {
	c, tor := newTestCache(t, 4, 2*testPieceLength)
	tor.Piece(1).SetPriority(torrent.PiecePriorityNormal)
	assertPriority(t, tor, 1, torrent.PiecePriorityNormal)

	c.clearPriority()

	assertPriority(t, tor, 1, torrent.PiecePriorityNone)
}

func TestClearPriorityWithReader(t *testing.T) {
	c, tor := newTestCache(t, 8, 4*testPieceLength)
	r := c.NewReader(tor.Files()[0])
	r.SetReadahead(0)
	tor.Piece(2).SetPriority(torrent.PiecePriorityNow)
	tor.Piece(6).SetPriority(torrent.PiecePriorityHigh)

	c.clearPriority()

	assertPriority(t, tor, 2, torrent.PiecePriorityNow)
	assertPriority(t, tor, 3, torrent.PiecePriorityNone)
	assertPriority(t, tor, 6, torrent.PiecePriorityNone)
}

// closeWithPieceFiles writes two piece files, drops the torrent, which closes
// the cache synchronously, and returns the piece file names and the hash dir.
func closeWithPieceFiles(t *testing.T, c *Cache, tor *torrent.Torrent) ([]string, string) {
	t.Helper()
	fillPiece(t, c, 0, 1)
	fillPiece(t, c, 1, 1)
	files := []string{c.pieces[0].dPiece.name, c.pieces[1].dPiece.name}
	dir := filepath.Join(settings.BTsets.TorrentsSavePath, c.hash.HexString())
	tor.Drop()
	if !c.isClosed.Load() {
		t.Fatal("dropping the torrent did not close the cache")
	}
	return files, dir
}

func assertPathsExist(t *testing.T, paths []string, want bool) {
	t.Helper()
	for _, p := range paths {
		_, err := os.Stat(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if got := err == nil; got != want {
			t.Fatalf("%s exists = %v, want %v", p, got, want)
		}
	}
}

func TestCloseRemoveCacheOnDropRemovesFiles(t *testing.T) {
	c, tor := newTestCache(t, 2, 4*testPieceLength)
	settings.BTsets.RemoveCacheOnDrop = true

	files, dir := closeWithPieceFiles(t, c, tor)
	assertPathsExist(t, append(files, dir), false)
}

func TestCloseWithoutRemoveCacheOnDropKeepsFiles(t *testing.T) {
	c, tor := newTestCache(t, 2, 4*testPieceLength)

	files, dir := closeWithPieceFiles(t, c, tor)
	assertPathsExist(t, append(files, dir), true)
}

func TestGetStateFilled(t *testing.T) {
	c, _ := newTestCache(t, 4, 8*testPieceLength)
	for id := 0; id < 3; id++ {
		fillPiece(t, c, id, 1)
	}

	st := c.GetState()
	if st.Filled != 3*testPieceLength {
		t.Fatalf("state filled = %d, want %d", st.Filled, 3*testPieceLength)
	}
	if c.filled != 3*testPieceLength {
		t.Fatalf("filled = %d, want %d", c.filled, 3*testPieceLength)
	}
}
