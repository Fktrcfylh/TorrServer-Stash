package torrstor

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"server/settings"
)

const pinTestPieceLength = 16384

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

// pinAllSet returns a pin set that wants every piece and drops none.
func pinAllSet(numPieces int) (want, drop []bool) {
	want = make([]bool, numPieces)
	for i := range want {
		want[i] = true
	}
	return want, make([]bool, numPieces)
}

// boolSet returns a slice of n values with the given ids set.
func boolSet(n int, ids ...int) []bool {
	ret := make([]bool, n)
	for _, id := range ids {
		ret[id] = true
	}
	return ret
}

// newPinTestCache opens a torrent of numPieces full pieces on a real offline
// client whose storage is a torrstor.Storage with the given capacity. When
// pinned, every piece is wanted.
func newPinTestCache(t *testing.T, numPieces int, capacity int64, pinned bool) (*Cache, *torrent.Torrent) {
	t.Helper()
	if !pinned {
		return newPinSetTestCache(t, numPieces, capacity, nil, nil)
	}
	want, drop := pinAllSet(numPieces)
	return newPinSetTestCache(t, numPieces, capacity, want, drop)
}

// newPinSetTestCache is newPinTestCache with an explicit pin set. The pin is
// set before the torrent is attached, so no asynchronous priority refresh or
// piece drop runs.
func newPinSetTestCache(t *testing.T, numPieces int, capacity int64, want, drop []bool) (*Cache, *torrent.Torrent) {
	t.Helper()
	return newPinFilesTestCache(t, []int{numPieces}, capacity, want, drop)
}

// newPinFilesTestCache is newPinSetTestCache for a torrent of consecutive files
// of the given numbers of full pieces.
func newPinFilesTestCache(t *testing.T, filePieces []int, capacity int64, want, drop []bool) (*Cache, *torrent.Torrent) {
	t.Helper()
	withDiskSettings(t)

	numPieces := 0
	for _, n := range filePieces {
		numPieces += n
	}
	info := metainfo.Info{
		Name:        "pin",
		PieceLength: pinTestPieceLength,
		Pieces:      make([]byte, 20*numPieces),
	}
	if len(filePieces) == 1 {
		info.Length = int64(numPieces) * pinTestPieceLength
	} else {
		for i, n := range filePieces {
			info.Files = append(info.Files, metainfo.FileInfo{Length: int64(n) * pinTestPieceLength, Path: []string{"f" + string(rune('0'+i))}})
		}
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
	c.SetPin(want, drop)
	c.SetTorrent(tor)
	return c, tor
}

// fillPiece writes a full piece and sets its access time, lower is older.
func fillPiece(t *testing.T, c *Cache, id int, accessed int64) {
	t.Helper()
	p := c.pieces[id]
	if _, err := p.WriteAt(make([]byte, pinTestPieceLength), 0); err != nil {
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

// waitPriority polls an asynchronous priority change with a deadline.
func waitPriority(t *testing.T, tor *torrent.Torrent, ids []int, want any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		done := true
		for _, id := range ids {
			if any(tor.PieceState(id).Priority) != want {
				done = false
				break
			}
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			for _, id := range ids {
				assertPriority(t, tor, id, want)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCleanPiecesEvictsOldestUnpinned(t *testing.T) {
	c, _ := newPinTestCache(t, 6, 2*pinTestPieceLength, false)
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
	if !pieceFileExists(t, c, 5) || c.pieces[5].Size != pinTestPieceLength {
		t.Fatalf("newest piece evicted: size %d", c.pieces[5].Size)
	}
}

func TestCleanPiecesKeepsPinned(t *testing.T) {
	c, _ := newPinTestCache(t, 6, 2*pinTestPieceLength, true)
	for id := 0; id < 6; id++ {
		fillPiece(t, c, id, int64(id+1))
	}

	c.cleanPieces()

	for id := 0; id < 6; id++ {
		if !pieceFileExists(t, c, id) || c.pieces[id].Size != pinTestPieceLength {
			t.Fatalf("pinned piece %d evicted: size %d", id, c.pieces[id].Size)
		}
	}
	if rem := c.getRemPieces(); len(rem) != 0 {
		t.Fatalf("getRemPieces returned %d pinned pieces", len(rem))
	}
	if c.filled != 0 {
		t.Fatalf("filled = %d, want 0 for pinned pieces", c.filled)
	}
}

// GetState reports pinned bytes as filled, the eviction counter is not touched.
func TestGetStateFilledIncludesPinned(t *testing.T) {
	c, _ := newPinTestCache(t, 4, 2*pinTestPieceLength, true)
	for id := 0; id < 3; id++ {
		fillPiece(t, c, id, 1)
	}
	c.filled = 7

	st := c.GetState()
	if st.Filled != 3*pinTestPieceLength {
		t.Fatalf("state filled = %d, want %d", st.Filled, 3*pinTestPieceLength)
	}
	if c.filled != 7 {
		t.Fatalf("GetState changed the eviction counter to %d", c.filled)
	}
}

func TestRemovePiecePinnedIsNoop(t *testing.T) {
	c, _ := newPinTestCache(t, 2, 2*pinTestPieceLength, true)
	fillPiece(t, c, 0, 1)

	c.removePiece(c.pieces[0])

	if !pieceFileExists(t, c, 0) || c.pieces[0].Size != pinTestPieceLength {
		t.Fatalf("pinned piece released: size %d", c.pieces[0].Size)
	}
}

func TestRemovePieceUnpinnedReleases(t *testing.T) {
	c, _ := newPinTestCache(t, 2, 2*pinTestPieceLength, false)
	fillPiece(t, c, 0, 1)

	c.removePiece(c.pieces[0])

	if pieceFileExists(t, c, 0) || c.pieces[0].Size != 0 {
		t.Fatalf("unpinned piece kept: size %d", c.pieces[0].Size)
	}
}

func TestClearPriorityUnpinnedNoReaders(t *testing.T) {
	c, tor := newPinTestCache(t, 4, 2*pinTestPieceLength, false)
	tor.Piece(1).SetPriority(torrent.PiecePriorityNormal)
	assertPriority(t, tor, 1, torrent.PiecePriorityNormal)

	c.clearPriority()

	assertPriority(t, tor, 1, torrent.PiecePriorityNone)
}

func TestClearPriorityPinnedNoReaders(t *testing.T) {
	c, tor := newPinTestCache(t, 4, 2*pinTestPieceLength, true)
	tor.Piece(1).SetPriority(torrent.PiecePriorityHigh)
	completePiece(t, c, tor, 2)

	c.clearPriority()

	for _, id := range []int{0, 1, 3} {
		assertPriority(t, tor, id, torrent.PiecePriorityNormal)
	}
	// anacrolix reports None for a complete piece whatever its own priority,
	// so make it incomplete again to see whether clearPriority set one
	c.pieces[2].MarkNotComplete()
	tor.Piece(2).UpdateCompletion()
	assertPriority(t, tor, 2, torrent.PiecePriorityNone)
}

func TestClearPriorityPinnedWithReader(t *testing.T) {
	// capacity 4 pieces, 95% read ahead: the reader range at offset 0 is pieces 0..3
	c, tor := newPinTestCache(t, 8, 4*pinTestPieceLength, true)
	r := c.NewReader(tor.Files()[0])
	// anacrolix gives only the reader position (piece 0) Now
	r.SetReadahead(0)
	if rng := r.getPiecesRange(); rng.Start != 0 || rng.End != 3 {
		t.Fatalf("reader range = %d..%d, want 0..3", rng.Start, rng.End)
	}
	tor.Piece(2).SetPriority(torrent.PiecePriorityNow)
	tor.Piece(1).SetPriority(torrent.PiecePriorityHigh)
	tor.Piece(6).SetPriority(torrent.PiecePriorityHigh)
	assertPriority(t, tor, 3, torrent.PiecePriorityNone)

	c.clearPriority()

	assertPriority(t, tor, 2, torrent.PiecePriorityNow)
	assertPriority(t, tor, 1, torrent.PiecePriorityHigh)
	assertPriority(t, tor, 3, torrent.PiecePriorityNormal)
	// outside the range a pinned piece is set to Normal, also from above
	assertPriority(t, tor, 6, torrent.PiecePriorityNormal)
	assertPriority(t, tor, 7, torrent.PiecePriorityNormal)
}

func TestClearPriorityUnpinnedWithReader(t *testing.T) {
	c, tor := newPinTestCache(t, 8, 4*pinTestPieceLength, false)
	r := c.NewReader(tor.Files()[0])
	r.SetReadahead(0)
	tor.Piece(2).SetPriority(torrent.PiecePriorityNow)
	tor.Piece(6).SetPriority(torrent.PiecePriorityHigh)

	c.clearPriority()

	assertPriority(t, tor, 2, torrent.PiecePriorityNow)
	assertPriority(t, tor, 3, torrent.PiecePriorityNone)
	assertPriority(t, tor, 6, torrent.PiecePriorityNone)
}

func TestSetPinRearmsPriorities(t *testing.T) {
	c, tor := newPinTestCache(t, 4, 2*pinTestPieceLength, false)
	ids := []int{0, 1, 2, 3}

	c.SetPin(pinAllSet(4))
	waitPriority(t, tor, ids, torrent.PiecePriorityNormal)
	c.SetPin(pinAllSet(4))
	if c.pin.Load() == nil {
		t.Fatal("repeated SetPin of every piece cleared the pin")
	}

	c.SetPin(nil, nil)
	waitPriority(t, tor, ids, torrent.PiecePriorityNone)
	c.SetPin(nil, nil)
	if c.pin.Load() != nil {
		t.Fatal("repeated SetPin(nil, nil) set the pin")
	}
}

// An equal set is not stored again, so nothing is re-armed or dropped.
func TestSetPinUnchangedKeepsSet(t *testing.T) {
	c, _ := newPinSetTestCache(t, 4, 2*pinTestPieceLength, boolSet(4, 2, 3), boolSet(4, 0))
	cur := c.pin.Load()

	c.SetPin(boolSet(4, 2, 3), boolSet(4, 0))
	if c.pin.Load() != cur {
		t.Fatal("SetPin stored an equal set again")
	}

	c.SetPin(boolSet(4, 2, 3), boolSet(4, 1))
	if got := c.pin.Load(); got == cur || !slices.Equal(got.drop, boolSet(4, 1)) {
		t.Fatal("SetPin did not store a set with a changed drop")
	}
	cur = c.pin.Load()
	c.SetPin(boolSet(4, 3), boolSet(4, 1))
	if got := c.pin.Load(); got == cur || !slices.Equal(got.want, boolSet(4, 3)) {
		t.Fatal("SetPin did not store a set with a changed want")
	}
}

func TestSetPinWithoutTorrent(t *testing.T) {
	var nilCache *Cache
	nilCache.SetPin(pinAllSet(1))

	withDiskSettings(t)
	stor := NewStorage(1 << 20)
	var hash metainfo.Hash
	hash[0] = 1
	stor.OpenTorrent(testInfo(), hash)
	c := stor.GetCache(hash)
	t.Cleanup(func() { stor.CloseHash(hash) })

	c.SetPin(pinAllSet(c.pieceCount))
	if c.pin.Load() == nil || !c.PinPending() {
		t.Fatal("pin not set on a cache without a torrent")
	}
}

// SetPin must not return while cleanPieces is releasing pieces it selected
// before the pin was set.
func TestSetPinWaitsForRunningEviction(t *testing.T) {
	c, _ := newPinTestCache(t, 2, 2*pinTestPieceLength, false)
	c.muRemove.Lock() // a cleanPieces pass is in progress

	done := make(chan struct{})
	go func() {
		c.SetPin(pinAllSet(2))
		close(done)
	}()
	select {
	case <-done:
		c.muRemove.Unlock()
		t.Fatal("SetPin returned during a running eviction")
	case <-time.After(100 * time.Millisecond):
	}
	if c.pin.Load() != nil {
		c.muRemove.Unlock()
		t.Fatal("pin set during a running eviction")
	}

	c.muRemove.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SetPin did not return after the eviction finished")
	}
	if c.pin.Load() == nil {
		t.Fatal("pin not set")
	}
}

func TestPinPending(t *testing.T) {
	var nilCache *Cache
	if nilCache.PinPending() {
		t.Fatal("nil cache reports a pending pin")
	}

	unpinned, _ := newPinTestCache(t, 3, 2*pinTestPieceLength, false)
	if unpinned.PinPending() {
		t.Fatal("unpinned cache reports a pending pin")
	}

	c, _ := newPinTestCache(t, 3, 2*pinTestPieceLength, true)
	if !c.PinPending() {
		t.Fatal("pinned cache with incomplete pieces reports no pending pin")
	}
	c.pieces[0].MarkComplete()
	c.pieces[1].MarkComplete()
	if !c.PinPending() {
		t.Fatal("pinned cache with one incomplete piece reports no pending pin")
	}
	c.pieces[2].MarkComplete()
	if c.PinPending() {
		t.Fatal("pinned cache with all pieces complete reports a pending pin")
	}

	c.Close()
	if c.PinPending() {
		t.Fatal("closed cache reports a pending pin")
	}
}

// Only wanted pieces are pending, incomplete pieces outside want are not.
func TestPinPendingCountsOnlyWanted(t *testing.T) {
	c, _ := newPinSetTestCache(t, 4, 2*pinTestPieceLength, boolSet(4, 0, 1), boolSet(4))
	if !c.PinPending() {
		t.Fatal("incomplete wanted pieces report no pending pin")
	}
	c.pieces[0].MarkComplete()
	if !c.PinPending() {
		t.Fatal("one incomplete wanted piece reports no pending pin")
	}
	c.pieces[1].MarkComplete()
	if c.PinPending() {
		t.Fatal("complete wanted pieces with incomplete not wanted pieces report a pending pin")
	}
	c.pieces[0].MarkNotComplete()
	if !c.PinPending() {
		t.Fatal("incomplete wanted piece reports no pending pin")
	}
}

// Every piece of a pinned cache is kept, wanted or not.
func TestCleanPiecesKeepsNotWantedOfPinned(t *testing.T) {
	c, _ := newPinSetTestCache(t, 6, 2*pinTestPieceLength, boolSet(6, 0), boolSet(6, 1))
	for id := 0; id < 6; id++ {
		fillPiece(t, c, id, int64(id+1))
	}

	c.cleanPieces()

	for id := 0; id < 6; id++ {
		if !pieceFileExists(t, c, id) || c.pieces[id].Size != pinTestPieceLength {
			t.Fatalf("piece %d of a pinned cache evicted: size %d", id, c.pieces[id].Size)
		}
	}
	for id := 0; id < 6; id++ {
		c.removePiece(c.pieces[id])
		if !pieceFileExists(t, c, id) || c.pieces[id].Size != pinTestPieceLength {
			t.Fatalf("removePiece released piece %d of a pinned cache", id)
		}
	}
}

func TestClearPriorityNotWantedOfPinned(t *testing.T) {
	c, tor := newPinSetTestCache(t, 4, 2*pinTestPieceLength, boolSet(4, 0, 1), boolSet(4))
	tor.Piece(2).SetPriority(torrent.PiecePriorityHigh)
	tor.Piece(3).SetPriority(torrent.PiecePriorityNormal)

	c.clearPriority()

	assertPriority(t, tor, 0, torrent.PiecePriorityNormal)
	assertPriority(t, tor, 1, torrent.PiecePriorityNormal)
	assertPriority(t, tor, 2, torrent.PiecePriorityNone)
	assertPriority(t, tor, 3, torrent.PiecePriorityNone)
}

// assertReleased checks that a dropped piece has no file, size or completion.
func assertReleased(t *testing.T, c *Cache, tor *torrent.Torrent, id int) {
	t.Helper()
	if pieceFileExists(t, c, id) || c.pieces[id].Size != 0 || c.pieces[id].Complete || tor.PieceState(id).Complete {
		t.Fatalf("piece %d not released: size %d complete %v", id, c.pieces[id].Size, c.pieces[id].Complete)
	}
}

func assertKept(t *testing.T, c *Cache, id int) {
	t.Helper()
	if !pieceFileExists(t, c, id) || c.pieces[id].Size != pinTestPieceLength {
		t.Fatalf("piece %d not kept: size %d", id, c.pieces[id].Size)
	}
}

func TestDropPieces(t *testing.T) {
	// 0, 1 dropped; 2 dropped but wanted; 3 wanted; 4 neither; 5 dropped without data
	c, tor := newPinSetTestCache(t, 6, 2*pinTestPieceLength, boolSet(6, 2, 3), boolSet(6, 0, 1, 2, 5))
	fillPiece(t, c, 0, 1)
	completePiece(t, c, tor, 1)
	for _, id := range []int{2, 3, 4} {
		fillPiece(t, c, id, 1)
	}

	c.dropPieces()

	assertReleased(t, c, tor, 0)
	assertReleased(t, c, tor, 1)
	for _, id := range []int{2, 3, 4} {
		assertKept(t, c, id)
	}
	if pieceFileExists(t, c, 5) {
		t.Fatal("piece file created for a dropped piece without data")
	}
}

func TestDropPiecesKeepsReaderFile(t *testing.T) {
	// files: 0..3, 4..7, 8..9; every piece dropped
	c, tor := newPinFilesTestCache(t, []int{4, 4, 2}, 2*pinTestPieceLength, boolSet(10), boolSet(10, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9))
	for id := 0; id < 10; id++ {
		fillPiece(t, c, id, 1)
	}
	// a reader at the start of the second file: its window covers only the
	// first pieces of the file, the rest of the file is kept too
	r := c.NewReader(tor.Files()[1])
	r.SetReadahead(0)
	if rng := r.getPiecesRange(); rng.End >= 7 {
		t.Fatalf("reader range = %d..%d, want it to end before piece 7", rng.Start, rng.End)
	}

	c.dropPieces()

	for _, id := range []int{4, 5, 6, 7} {
		assertKept(t, c, id)
	}
	for _, id := range []int{0, 1, 2, 3, 8, 9} {
		assertReleased(t, c, tor, id)
	}
}

func TestDropPiecesUnpinnedOrClosed(t *testing.T) {
	t.Run("unpinned", func(t *testing.T) {
		c, _ := newPinTestCache(t, 2, 4*pinTestPieceLength, false)
		fillPiece(t, c, 0, 1)
		c.dropPieces()
		assertKept(t, c, 0)
	})
	t.Run("closed", func(t *testing.T) {
		c, _ := newPinSetTestCache(t, 2, 4*pinTestPieceLength, boolSet(2), boolSet(2, 0))
		fillPiece(t, c, 0, 1)
		c.isClosed.Store(true)
		c.dropPieces()
		assertKept(t, c, 0)
	})
}

// runBlockedDrop starts dropPieces while muRemove is held, runs change once
// the pass waits for the first piece, then lets the pass finish.
func runBlockedDrop(t *testing.T, c *Cache, change func()) {
	t.Helper()
	c.muRemove.Lock()
	done := make(chan struct{})
	go func() {
		c.dropPieces()
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if slices.ContainsFunc(strings.Split(string(buf[:n]), "\n\n"), func(g string) bool {
			return strings.Contains(g, "torrstor.(*Cache).dropPieces") && strings.Contains(g, "sync.(*Mutex).Lock")
		}) {
			break
		}
		if time.Now().After(deadline) {
			c.muRemove.Unlock()
			t.Fatal("dropPieces did not wait for muRemove")
		}
		time.Sleep(time.Millisecond)
	}
	change()
	c.muRemove.Unlock()
	<-done
}

// A piece is dropped only when the set loaded under muRemove drops it, not the
// set seen when the pass started.
func TestDropPiecesReloadsCurrentSet(t *testing.T) {
	c, _ := newPinSetTestCache(t, 4, 4*pinTestPieceLength, boolSet(4), boolSet(4, 0, 1, 2, 3))
	for id := 0; id < 4; id++ {
		fillPiece(t, c, id, 1)
	}

	// SetPin would wait for muRemove, so the new set is stored directly
	runBlockedDrop(t, c, func() { c.pin.Store(&pinSet{want: boolSet(4), drop: boolSet(4)}) })

	for id := 0; id < 4; id++ {
		assertKept(t, c, id)
	}
}

// A pass that started before Close releases nothing after it.
func TestDropPiecesStopsWhenClosed(t *testing.T) {
	c, _ := newPinSetTestCache(t, 4, 4*pinTestPieceLength, boolSet(4), boolSet(4, 0, 1, 2, 3))
	for id := 0; id < 4; id++ {
		fillPiece(t, c, id, 1)
	}

	runBlockedDrop(t, c, func() { c.isClosed.Store(true) })

	for id := 0; id < 4; id++ {
		assertKept(t, c, id)
	}
}

func TestSetPinDropsAsync(t *testing.T) {
	c, tor := newPinTestCache(t, 4, 4*pinTestPieceLength, false)
	for id := 0; id < 4; id++ {
		fillPiece(t, c, id, 1)
	}

	c.SetPin(boolSet(4, 2, 3), boolSet(4, 0, 1))
	deadline := time.Now().Add(5 * time.Second)
	for pieceFileExists(t, c, 0) || pieceFileExists(t, c, 1) {
		if time.Now().After(deadline) {
			t.Fatal("SetPin did not drop the pieces")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// a synchronous pass orders the field reads after the asynchronous releases
	c.dropPieces()
	assertReleased(t, c, tor, 0)
	assertReleased(t, c, tor, 1)
	assertKept(t, c, 2)
	assertKept(t, c, 3)
}

func TestSetPinUnpinRemovesNothing(t *testing.T) {
	c, _ := newPinSetTestCache(t, 2, 4*pinTestPieceLength, boolSet(2, 1), boolSet(2))
	fillPiece(t, c, 0, 1)
	fillPiece(t, c, 1, 1)

	c.SetPin(nil, nil)
	c.dropPieces()

	assertKept(t, c, 0)
	assertKept(t, c, 1)
}

// SetPin toggles race the eviction, drop, state and priority paths.
func TestSetPinConcurrent(t *testing.T) {
	c, _ := newPinTestCache(t, 6, 2*pinTestPieceLength, false)
	// dropped pieces have no data, so no release races the unlocked readers
	// of piece fields (GetState, PinPending)
	for id := 2; id < 6; id++ {
		fillPiece(t, c, id, int64(id+1))
	}
	allWant, allDrop := pinAllSet(6)
	sets := [][2][]bool{
		{allWant, allDrop},
		{boolSet(6, 2, 3), boolSet(6, 0, 1)},
		{nil, nil},
	}

	var wg sync.WaitGroup
	const iters = 200
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			set := sets[i%len(sets)]
			c.SetPin(set[0], set[1])
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			c.clearPriority()
			c.GetState()
			c.cleanPieces()
			c.PinPending()
			c.dropPieces()
		}
	}()
	wg.Wait()
}

// withTorrentRow stores a DB row with the given pin mode and removes it afterwards.
func withTorrentRow(t *testing.T, hash metainfo.Hash, mode string) {
	t.Helper()
	settings.AddTorrent(&settings.TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}})
	settings.SetTorrentPin(hash, mode, 0)
	t.Cleanup(func() { settings.RemTorrent(hash) })
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

func TestCloseRemoveCacheOnDropKeepsPinned(t *testing.T) {
	for _, mode := range []string{settings.PinModeAll, settings.PinModeNext} {
		t.Run(mode, func(t *testing.T) {
			c, tor := newPinTestCache(t, 2, 4*pinTestPieceLength, false)
			settings.BTsets.RemoveCacheOnDrop = true
			withTorrentRow(t, c.hash, mode)

			files, dir := closeWithPieceFiles(t, c, tor)
			assertPathsExist(t, append(files, dir), true)
		})
	}
}

func TestCloseRemoveCacheOnDropRemovesUnpinned(t *testing.T) {
	t.Run("unpinned row", func(t *testing.T) {
		c, tor := newPinTestCache(t, 2, 4*pinTestPieceLength, false)
		settings.BTsets.RemoveCacheOnDrop = true
		withTorrentRow(t, c.hash, "")

		files, dir := closeWithPieceFiles(t, c, tor)
		assertPathsExist(t, append(files, dir), false)
	})
	t.Run("no row", func(t *testing.T) {
		c, tor := newPinTestCache(t, 2, 4*pinTestPieceLength, false)
		settings.BTsets.RemoveCacheOnDrop = true

		files, dir := closeWithPieceFiles(t, c, tor)
		assertPathsExist(t, append(files, dir), false)
	})
}

func TestCloseWithoutRemoveCacheOnDropKeepsFiles(t *testing.T) {
	c, tor := newPinTestCache(t, 2, 4*pinTestPieceLength, false)
	withTorrentRow(t, c.hash, "")

	files, dir := closeWithPieceFiles(t, c, tor)
	assertPathsExist(t, append(files, dir), true)
}

// A paused pin (torr pushes an empty want set) raises no piece through any
// priority path, keeps every piece from eviction and still drops pieces.
func TestPausedPinSetRaisesNothing(t *testing.T) {
	c, tor := newPinSetTestCache(t, 8, 4*pinTestPieceLength, boolSet(8), boolSet(8, 7))
	none := []int{0, 1, 2, 3, 4, 5, 6, 7}

	// reader close path: the priority a reader set inside its range is kept,
	// every other piece is None
	r := c.NewReader(tor.Files()[0])
	r.SetReadahead(0)
	tor.Piece(2).SetPriority(torrent.PiecePriorityNow)
	tor.Piece(6).SetPriority(torrent.PiecePriorityNormal)
	c.clearPriority()
	assertPriority(t, tor, 2, torrent.PiecePriorityNow)
	for _, id := range []int{3, 4, 5, 6, 7} {
		assertPriority(t, tor, id, torrent.PiecePriorityNone)
	}

	// watchdog path with a reader: only the reader range is loaded
	c.getRemPieces()
	for _, id := range []int{4, 5, 6, 7} {
		assertPriority(t, tor, id, torrent.PiecePriorityNone)
	}

	// CloseReader removes the reader and runs clearPriority and getRemPieces
	// asynchronously; they run synchronously here (Reader.Close would race
	// on the eviction counter with the calls below)
	c.muReaders.Lock()
	delete(c.readers, r)
	c.muReaders.Unlock()
	r.isClosed = true
	r.Reader.Close()
	c.clearPriority()
	for _, id := range none {
		assertPriority(t, tor, id, torrent.PiecePriorityNone)
	}
	// watchdog path without readers
	c.getRemPieces()
	for _, id := range none {
		assertPriority(t, tor, id, torrent.PiecePriorityNone)
	}
	if c.PinPending() {
		t.Fatal("paused pin is pending")
	}

	for _, id := range none {
		fillPiece(t, c, id, int64(id+1))
	}
	c.cleanPieces()
	for _, id := range none {
		assertKept(t, c, id)
	}

	c.dropPieces()
	assertReleased(t, c, tor, 7)
	for _, id := range none[:7] {
		assertKept(t, c, id)
	}
}
