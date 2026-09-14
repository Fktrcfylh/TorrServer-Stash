package torrstor

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"

	"server/log"
	"server/settings"
	"server/torr/storage/state"
	"server/torr/utils"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

type Cache struct {
	storage.TorrentImpl
	storage *Storage

	capacity int64
	filled   int64
	hash     metainfo.Hash

	pieceLength int64
	pieceCount  int

	// pieces content is immutable after Init; muPieces guards the map
	// reference itself (nilled in Close), so holders of a snapshot may
	// safely iterate it without the lock
	pieces   map[int]*Piece
	muPieces sync.RWMutex

	readers   map[*Reader]struct{}
	muReaders sync.RWMutex

	isRemove atomic.Bool
	isClosed atomic.Bool
	muRemove sync.Mutex
	// muPrio serializes clearPriority and setLoadPriority so that the priority
	// reset of a reader that has just closed cannot wipe the priorities a
	// freshly created reader has already set.
	muPrio  sync.Mutex
	torrent *torrent.Torrent

	// pin is the pin set pushed by torr; nil means unpinned
	pin atomic.Pointer[pinSet]
}

// pinSet is immutable after it is stored. Every piece of a pinned torrent is
// kept (never evicted); want marks the pieces kept downloading, drop the
// pieces whose data is deleted by dropPieces.
type pinSet struct {
	want, drop []bool
}

func NewCache(capacity int64, storage *Storage) *Cache {
	ret := &Cache{
		capacity: capacity,
		filled:   0,
		pieces:   make(map[int]*Piece),
		storage:  storage,
		readers:  make(map[*Reader]struct{}),
	}

	return ret
}

func (c *Cache) Init(info *metainfo.Info, hash metainfo.Hash) {
	log.TLogln("Create cache for:", info.Name, hash.HexString())
	if c.capacity == 0 {
		c.capacity = info.PieceLength * 4
	}

	c.pieceLength = info.PieceLength
	c.pieceCount = info.NumPieces()
	c.hash = hash

	if settings.BTsets.UseDisk {
		name := filepath.Join(settings.BTsets.TorrentsSavePath, hash.HexString())
		err := os.MkdirAll(name, 0o777)
		if err != nil {
			log.TLogln("Error create dir:", err)
		}
	}

	for i := 0; i < c.pieceCount; i++ {
		c.pieces[i] = NewPiece(i, info.Piece(i).Length(), c)
	}

	go c.priorityWatchdog()
}

// priorityWatchdog re-arms piece priorities while readers are active or the
// cache is pinned.
//
// setLoadPriority is only reached through the cache cleanup path, which is
// driven by piece reads and writes (see mempiece.go and diskpiece.go). Should
// priorities ever end up cleared while a reader still needs data, nothing is
// downloaded, so no piece I/O happens, so cleanup never runs and the
// priorities are never restored - the torrent stalls indefinitely with peers
// connected. Re-arming them periodically breaks that cycle regardless of how
// the priorities were lost.
func (c *Cache) priorityWatchdog() {
	for {
		time.Sleep(5 * time.Second)
		if c.isClosed.Load() {
			return
		}
		if c.torrent == nil {
			continue
		}
		if c.GetUseReaders() > 0 || c.pin.Load() != nil {
			c.getRemPieces()
		}
	}
}

func (c *Cache) SetTorrent(torr *torrent.Torrent) {
	c.torrent = torr
}

// SetPin stores the pin set, want nil means unpinned. On a change it re-arms
// priorities and deletes the dropped pieces asynchronously.
func (c *Cache) SetPin(want, drop []bool) {
	if c == nil {
		return
	}
	var next *pinSet
	if want != nil {
		next = &pinSet{want: want, drop: drop}
	}
	// wait for a running cleanPieces or piece drop, so a piece selected under
	// the previous set cannot be released after the new set is stored
	c.muRemove.Lock()
	cur := c.pin.Load()
	if (cur == nil && next == nil) ||
		(cur != nil && next != nil && slices.Equal(cur.want, next.want) && slices.Equal(cur.drop, next.drop)) {
		c.muRemove.Unlock()
		return
	}
	c.pin.Store(next)
	c.muRemove.Unlock()

	wanted, dropped := 0, 0
	for _, v := range want {
		if v {
			wanted++
		}
	}
	for _, v := range drop {
		if v {
			dropped++
		}
	}
	log.TLogln("Set cache pin:", c.hash.HexString(), "pinned:", next != nil, "wanted:", wanted, "drop:", dropped)
	if c.torrent != nil && !c.isClosed.Load() {
		// priorities are lowered before the piece files are removed
		go func() {
			c.clearPriority()
			c.dropPieces()
		}()
	}
}

// PinPending reports whether a wanted piece is not complete yet.
func (c *Cache) PinPending() bool {
	if c == nil || c.pin.Load() == nil {
		return false
	}
	for id, p := range c.getPieces() {
		if c.isWanted(id) && !p.Complete {
			return true
		}
	}
	return false
}

// isKept reports whether the piece is exempt from eviction: every piece of a
// pinned torrent is.
func (c *Cache) isKept(id int) bool {
	return c.pin.Load() != nil
}

// isWanted reports whether the piece is kept downloading by the pin.
func (c *Cache) isWanted(id int) bool {
	ps := c.pin.Load()
	return ps != nil && id < len(ps.want) && ps.want[id]
}

// dropPieces releases the pieces with data that the current pin set drops and
// does not want, outside the files that have a reader.
func (c *Cache) dropPieces() {
	if c.isClosed.Load() || c.torrent == nil || c.pin.Load() == nil {
		return
	}
	// every piece of a file with a reader is kept, not only the reader window:
	// a reader may advance or seek while the pass runs, and playback must not
	// lose data it is about to read
	ranges := make([]Range, 0)
	for _, r := range c.readersSnapshot() {
		if r.file.Length() > 0 {
			ranges = append(ranges, Range{
				Start: int(r.file.Offset() / c.pieceLength),
				End:   int((r.file.Offset() + r.file.Length() - 1) / c.pieceLength),
				File:  r.file,
			})
		}
	}

	count := 0
	for id, p := range c.getPieces() {
		// the lock is taken per piece, so SetPin waits for one release only;
		// the set is re-loaded so a piece is dropped only by the current set
		c.muRemove.Lock()
		cur := c.pin.Load()
		if !c.isClosed.Load() && cur != nil && id < len(cur.drop) && cur.drop[id] &&
			!(id < len(cur.want) && cur.want[id]) && !inRanges(ranges, id) && (p.Size > 0 || p.Complete) {
			p.Release()
			count++
		}
		c.muRemove.Unlock()
	}
	if count > 0 {
		log.TLogln("Drop pinned pieces:", c.hash.HexString(), count)
	}
}

func (c *Cache) getPieces() map[int]*Piece {
	c.muPieces.RLock()
	defer c.muPieces.RUnlock()
	return c.pieces
}

func (c *Cache) readersSnapshot() []*Reader {
	c.muReaders.RLock()
	defer c.muReaders.RUnlock()
	list := make([]*Reader, 0, len(c.readers))
	for r := range c.readers {
		list = append(list, r)
	}
	return list
}

func (c *Cache) Piece(m metainfo.Piece) storage.PieceImpl {
	if val, ok := c.getPieces()[m.Index()]; ok {
		return val
	}
	return &PieceFake{}
}

func (c *Cache) Close() error {
	if c.torrent != nil {
		log.TLogln("Close cache for:", c.torrent.Name(), c.hash)
	} else {
		log.TLogln("Close cache for:", c.hash)
	}
	c.isClosed.Store(true)

	c.storage.removeCache(c.hash)

	// the data of a pinned torrent is removed only by rem or pin off
	if settings.BTsets.RemoveCacheOnDrop {
		if settings.IsTorrentPinned(c.hash) {
			log.TLogln("Keep pinned cache on close:", c.hash)
		} else {
			name := filepath.Join(settings.BTsets.TorrentsSavePath, c.hash.HexString())
			if name != "" && name != "/" {
				for _, v := range c.getPieces() {
					if v.dPiece != nil {
						os.Remove(v.dPiece.name)
					}
				}
				os.Remove(name)
			}
		}
	}

	c.muReaders.Lock()
	c.readers = nil
	c.muReaders.Unlock()

	c.muPieces.Lock()
	c.pieces = nil
	c.muPieces.Unlock()

	utils.FreeOSMemGC()
	return nil
}

func (c *Cache) removePiece(piece *Piece) {
	if c.isClosed.Load() || c.isKept(piece.Id) {
		return
	}
	piece.Release()
}

func (c *Cache) AdjustRA(readahead int64) {
	if c == nil {
		return
	}
	if settings.BTsets.CacheSize == 0 {
		c.capacity = readahead * 3
	}
	for _, r := range c.readersSnapshot() {
		r.SetReadahead(readahead)
	}
}

func (c *Cache) GetState() *state.CacheState {
	cState := new(state.CacheState)

	piecesState := make(map[int]state.ItemState, 0)
	var fill int64 = 0

	for _, p := range c.getPieces() {
		if p.Size > 0 {
			fill += p.Size
			piecesState[p.Id] = state.ItemState{
				Id:        p.Id,
				Size:      p.Size,
				Length:    c.pieceLength,
				Completed: p.Complete,
				Priority:  int(c.torrent.PieceState(p.Id).Priority),
			}
		}
	}

	readersState := make([]*state.ReaderState, 0)

	for _, r := range c.readersSnapshot() {
		rng := r.getPiecesRange()
		pc := r.getReaderPiece()
		readersState = append(readersState, &state.ReaderState{
			Start:  rng.Start,
			End:    rng.End,
			Reader: pc,
		})
	}

	// c.filled is owned by getRemPieces, which excludes pinned pieces
	cState.Capacity = c.capacity
	cState.PiecesLength = c.pieceLength
	cState.PiecesCount = c.pieceCount
	cState.Hash = c.hash.HexString()
	cState.Filled = fill
	cState.Pieces = piecesState
	cState.Readers = readersState
	return cState
}

func (c *Cache) cleanPieces() {
	if c.isRemove.Load() || c.isClosed.Load() {
		return
	}

	// Protection against concurrent deletion
	if !c.muRemove.TryLock() {
		return // Cleanup is already in progress in another goroutine
	}
	defer c.muRemove.Unlock()

	c.isRemove.Store(true)
	defer func() { c.isRemove.Store(false) }()

	remPieces := c.getRemPieces()
	if c.filled > c.capacity {
		rems := (c.filled-c.capacity)/c.pieceLength + 1
		for _, p := range remPieces {
			c.removePiece(p)
			rems--
			if rems <= 0 {
				utils.FreeOSMemGC()
				return
			}
		}
	}
}

func (c *Cache) getRemPieces() []*Piece {
	readers := c.readersSnapshot()

	// Collect read ranges from active readers
	ranges := make([]Range, 0)
	for _, r := range readers {
		r.checkReader()
		if r.isUse {
			ranges = append(ranges, r.getPiecesRange())
		}
	}
	ranges = mergeRange(ranges)

	piecesRemove := make([]*Piece, 0)
	fill := int64(0)

	// Determine which chunks can be deleted
	for id, p := range c.getPieces() {
		// pieces of a pinned torrent are never evicted and do not count against capacity
		if c.isKept(id) {
			continue
		}
		if p.Size > 0 {
			fill += p.Size
		}
		if len(ranges) > 0 {
			if !inRanges(ranges, id) {
				if p.Size > 0 && !c.isIdInFileBE(ranges, id) {
					piecesRemove = append(piecesRemove, p)
				}
			}
		} else {
			// When preloading, clear everything except the beginning and end of the file
			if p.Size > 0 && !c.isIdInFileBE(ranges, id) {
				piecesRemove = append(piecesRemove, p)
			}
		}
	}

	c.clearPriority()
	c.setLoadPriority(ranges)

	// Sort by last access time (oldest first)
	sort.Slice(piecesRemove, func(i, j int) bool {
		return piecesRemove[i].Accessed < piecesRemove[j].Accessed
	})

	c.filled = fill
	return piecesRemove
}

func (c *Cache) setLoadPriority(ranges []Range) {
	readers := c.readersSnapshot()
	pieces := c.getPieces()
	if len(readers) == 0 || pieces == nil {
		return
	}
	c.muPrio.Lock()
	defer c.muPrio.Unlock()
	for _, r := range readers {
		if !r.isUse {
			continue
		}
		if c.isIdInFileBE(ranges, r.getReaderPiece()) {
			continue
		}
		readerPos := r.getReaderPiece()
		readerRAHPos := r.getReaderRAHPiece()
		end := r.getPiecesRange().End
		count := settings.BTsets.ConnectionsLimit / len(readers) // max concurrent loading blocks
		limit := 0
		for i := readerPos; i < end && limit < count; i++ {
			if !pieces[i].Complete {
				if i == readerPos {
					c.torrent.Piece(i).SetPriority(torrent.PiecePriorityNow)
				} else if i == readerPos+1 {
					c.torrent.Piece(i).SetPriority(torrent.PiecePriorityNext)
				} else if i > readerPos && i <= readerRAHPos {
					c.torrent.Piece(i).SetPriority(torrent.PiecePriorityReadahead)
				} else if i > readerRAHPos && i <= readerRAHPos+5 && c.torrent.PieceState(i).Priority != torrent.PiecePriorityHigh {
					c.torrent.Piece(i).SetPriority(torrent.PiecePriorityHigh)
				} else if i > readerRAHPos+5 && c.torrent.PieceState(i).Priority != torrent.PiecePriorityNormal {
					c.torrent.Piece(i).SetPriority(torrent.PiecePriorityNormal)
				}
				limit++
			}
		}
	}
}

func (c *Cache) isIdInFileBE(ranges []Range, id int) bool {
	// keep 8/16 MB
	FileRangeNotDelete := int64(c.pieceLength)
	if FileRangeNotDelete < 8<<20 {
		FileRangeNotDelete = 8 << 20
	}

	for _, rng := range ranges {
		ss := int(rng.File.Offset() / c.pieceLength)
		se := int((rng.File.Offset() + FileRangeNotDelete) / c.pieceLength)

		es := int((rng.File.Offset() + rng.File.Length() - FileRangeNotDelete) / c.pieceLength)
		ee := int((rng.File.Offset() + rng.File.Length()) / c.pieceLength)

		if id >= ss && id < se || id > es && id <= ee {
			return true
		}
	}
	return false
}

//////////////////
// Reader section
////////

func (c *Cache) NewReader(file *torrent.File) *Reader {
	return newReader(file, c)
}

func (c *Cache) GetUseReaders() int {
	if c == nil {
		return 0
	}
	c.muReaders.RLock()
	defer c.muReaders.RUnlock()
	readers := 0
	for reader := range c.readers {
		if reader.isUse {
			readers++
		}
	}
	return readers
}

func (c *Cache) Readers() int {
	if c == nil {
		return 0
	}
	c.muReaders.RLock()
	defer c.muReaders.RUnlock()
	return len(c.readers)
}

func (c *Cache) CloseReader(r *Reader) {
	r.cache.muReaders.Lock()
	delete(r.cache.readers, r)
	r.cache.muReaders.Unlock()
	// Reader.Close touches anacrolix internals, keep it outside muReaders
	r.Close()
	go c.clearPriority()
}

func (c *Cache) clearPriority() {
	if c.torrent == nil {
		return
	}
	// This used to sleep for a second before clearing priorities. A reader
	// created during that window could have its PiecePriorityNow/Next/Readahead
	// reset to None right after setLoadPriority had assigned them, starving the
	// player. A mutex provides the same ordering without the race window.
	c.muPrio.Lock()
	defer c.muPrio.Unlock()
	ranges := make([]Range, 0)
	for _, r := range c.readersSnapshot() {
		r.checkReader()
		if r.isUse {
			ranges = append(ranges, r.getPiecesRange())
		}
	}
	ranges = mergeRange(ranges)

	for id := range c.getPieces() {
		if c.isWanted(id) {
			// a wanted piece keeps downloading: raise it to Normal, but never
			// lower the priority a reader has set inside its range
			ps := c.torrent.PieceState(id)
			if ps.Complete {
				continue
			}
			if len(ranges) == 0 || !inRanges(ranges, id) {
				if ps.Priority != torrent.PiecePriorityNormal {
					c.torrent.Piece(id).SetPriority(torrent.PiecePriorityNormal)
				}
			} else if ps.Priority == torrent.PiecePriorityNone {
				c.torrent.Piece(id).SetPriority(torrent.PiecePriorityNormal)
			}
			continue
		}
		if len(ranges) > 0 {
			if !inRanges(ranges, id) {
				if c.torrent.PieceState(id).Priority != torrent.PiecePriorityNone {
					c.torrent.Piece(id).SetPriority(torrent.PiecePriorityNone)
				}
			}
		} else {
			if c.torrent.PieceState(id).Priority != torrent.PiecePriorityNone {
				c.torrent.Piece(id).SetPriority(torrent.PiecePriorityNone)
			}
		}
	}
}

func (c *Cache) GetCapacity() int64 {
	if c == nil {
		return 0
	}
	return c.capacity
}
