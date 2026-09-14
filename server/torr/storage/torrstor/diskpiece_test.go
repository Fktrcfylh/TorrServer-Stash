package torrstor

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/anacrolix/torrent/metainfo"

	"server/settings"
)

// openWithPieceFiles creates piece files of the given sizes, then opens a disk
// cache over three pieces of 16 KiB, the last one 8 KiB long.
func openWithPieceFiles(t *testing.T, sizes map[int]int) *Cache {
	t.Helper()
	withDiskSettings(t)
	info := &metainfo.Info{
		Name:        "short-last",
		PieceLength: pinTestPieceLength,
		Length:      2*pinTestPieceLength + pinTestPieceLength/2,
		Pieces:      make([]byte, 20*3),
	}
	var hash metainfo.Hash
	hash[0] = 9

	dir := filepath.Join(settings.BTsets.TorrentsSavePath, hash.HexString())
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	for id, size := range sizes {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(id)), make([]byte, size), 0o666); err != nil {
			t.Fatal(err)
		}
	}

	stor := NewStorage(1 << 20)
	stor.OpenTorrent(info, hash)
	t.Cleanup(func() { stor.CloseHash(hash) })
	return stor.GetCache(hash)
}

func assertComplete(t *testing.T, c *Cache, id int, size int64, complete bool) {
	t.Helper()
	p := c.pieces[id]
	if p.Size != size || p.Complete != complete {
		t.Fatalf("piece %d: size %d complete %v, want size %d complete %v", id, p.Size, p.Complete, size, complete)
	}
}

func TestNewDiskPieceFullFilesComplete(t *testing.T) {
	c := openWithPieceFiles(t, map[int]int{0: pinTestPieceLength, 2: pinTestPieceLength / 2})

	assertComplete(t, c, 0, pinTestPieceLength, true)
	assertComplete(t, c, 1, 0, false)
	// the last piece is complete at its real length, not the piece length
	assertComplete(t, c, 2, pinTestPieceLength/2, true)
}

func TestNewDiskPieceShortFilesIncomplete(t *testing.T) {
	c := openWithPieceFiles(t, map[int]int{0: pinTestPieceLength - 1, 2: pinTestPieceLength/2 - 1})

	assertComplete(t, c, 0, pinTestPieceLength-1, false)
	assertComplete(t, c, 2, pinTestPieceLength/2-1, false)
}
