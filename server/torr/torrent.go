package torr

import (
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"server/torrshash"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"server/log"
	"server/settings"
	"server/torr/state"
	cacheSt "server/torr/storage/state"
	"server/torr/storage/torrstor"
	"server/torr/utils"
)

type Torrent struct {
	Title    string
	Category string
	Poster   string
	Data     string
	*torrent.TorrentSpec

	Stat      state.TorrentStat
	Timestamp int64
	Size      int64

	PinMode       string
	PinNext       int
	PinAnchor     int
	PinDownloaded []int

	*torrent.Torrent
	muTorrent sync.Mutex

	bt    *BTServer
	cache *torrstor.Cache

	lastTimeSpeed       time.Time
	DownloadSpeed       float64
	UploadSpeed         float64
	BytesReadUsefulData int64
	BytesWrittenData    int64

	PreloadSize    int64
	PreloadedBytes int64

	DurationSeconds float64
	BitRate         string

	expiredTime time.Time

	closed <-chan struct{}

	progressTicker *time.Ticker
}

func NewTorrent(spec *torrent.TorrentSpec, bt *BTServer) (*Torrent, error) {
	// https://github.com/anacrolix/torrent/issues/747
	if bt == nil || bt.client == nil {
		return nil, errors.New("BT client not connected")
	}
	switch settings.BTsets.RetrackersMode {
	case 1:
		spec.Trackers = append(spec.Trackers, [][]string{utils.GetDefTrackers()}...)
	case 2:
		spec.Trackers = nil
	case 3:
		spec.Trackers = [][]string{utils.GetDefTrackers()}
	}

	trackers := utils.GetTrackerFromFile()
	if len(trackers) > 0 {
		spec.Trackers = append(spec.Trackers, [][]string{trackers}...)
	}

	if len(spec.InfoBytes) == 0 {
		if db := GetTorrentDB(spec.InfoHash); db != nil && db.TorrentSpec != nil {
			spec.InfoBytes = db.TorrentSpec.InfoBytes
		}
	}

	goTorrent, _, err := bt.client.AddTorrentSpec(spec)
	if err != nil {
		return nil, err
	}

	bt.mu.Lock()
	defer bt.mu.Unlock()
	if tor, ok := bt.torrents[spec.InfoHash]; ok {
		return tor, nil
	}

	timeout := time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout)
	if timeout > time.Minute {
		timeout = time.Minute
	}

	torr := new(Torrent)
	torr.Torrent = goTorrent
	torr.Stat = state.TorrentAdded
	torr.lastTimeSpeed = time.Now()
	torr.bt = bt
	torr.closed = goTorrent.Closed()
	torr.TorrentSpec = spec
	torr.AddExpiredTime(timeout)
	torr.Timestamp = time.Now().Unix()

	go torr.watch()

	bt.torrents[spec.InfoHash] = torr
	return torr, nil
}

func (t *Torrent) WaitInfo() bool {
	if t == nil || t.Torrent == nil {
		return false
	}

	// Close torrent if no info in 1 minute + TorrentDisconnectTimeout config option
	tm := time.NewTimer(time.Minute + time.Second*time.Duration(settings.BTsets.TorrentDisconnectTimeout))

	select {
	case <-t.Torrent.GotInfo():
		if t.TorrentSpec != nil && len(t.TorrentSpec.InfoBytes) == 0 {
			t.TorrentSpec.InfoBytes = t.Torrent.Metainfo().InfoBytes
		}
		if t.bt != nil && t.bt.storage != nil {
			t.cache = t.bt.storage.GetCache(t.Hash())
			t.cache.SetTorrent(t.Torrent)
			t.applyPin()
		}
		return true
	case <-t.closed:
		return false
	case <-tm.C:
		return false
	}
}

// applyPin pushes the pin set computed from the mirror to the cache. Only disk
// storage with metadata pins pieces.
func (t *Torrent) applyPin() {
	// the cache is updated under the lock so concurrent calls cannot apply a
	// stale pin after a newer one
	t.muTorrent.Lock()
	defer t.muTorrent.Unlock()
	pinned := t.PinMode == settings.PinModeAll || t.PinMode == settings.PinModeNext
	if !pinned || !settings.BTsets.UseDisk {
		// Hash does not take muTorrent
		pinNoSpaceSet(t.Hash(), false)
		t.cache.SetPin(nil, nil)
		return
	}
	if t.Torrent == nil || t.Torrent.Info() == nil {
		// the free space is unknown without info, so the no-space state is kept
		t.cache.SetPin(nil, nil)
		return
	}
	info := t.Torrent.Info()
	var files []pinFile
	for i, f := range sortedFiles(t.Torrent.Files()) {
		files = append(files, pinFile{id: i + 1, path: f.Path(), offset: f.Offset(), length: f.Length()})
	}
	want, drop := pinPieces(files, info.PieceLength, t.Torrent.NumPieces(), t.PinMode, t.PinNext, t.PinAnchor)

	// the pin is paused while the save path has no room for the missing
	// wanted pieces plus the margin; an unknown free space never pauses
	var required int64
	for i, w := range want {
		if w && !t.Torrent.PieceState(i).Complete {
			required += info.Piece(i).Length()
		}
	}
	free, ok := freeSpace(settings.BTsets.TorrentsSavePath)
	paused := ok && required > 0 && free < uint64(required)+pinFreeSpaceMargin
	hash := t.Torrent.InfoHash()
	if pinNoSpaceSet(hash, paused) {
		if paused {
			log.TLogln("Pin paused, no space:", hash.HexString(), free, required)
		} else {
			log.TLogln("Pin resumed, space available:", hash.HexString(), free, required)
		}
	}
	if paused {
		// a non-nil empty want set keeps every piece from eviction and the drop
		// working, but raises no piece
		t.cache.SetPin(make([]bool, len(want)), drop)
		return
	}
	t.cache.SetPin(want, drop)
}

func (t *Torrent) GotInfo() bool {
	// log.TLogln("GotInfo state:", t.Stat)
	if t == nil || t.Stat == state.TorrentClosed {
		return false
	}
	// assume we have info in preload state
	// and dont override with TorrentWorking
	if t.Stat == state.TorrentPreload {
		return true
	}
	// read before waiting: nothing after WaitInfo touches settings, so the end
	// of an asynchronous load is ordered by the pin apply inside WaitInfo
	timeout := time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout)
	t.Stat = state.TorrentGettingInfo
	if t.WaitInfo() {
		t.Stat = state.TorrentWorking
		t.AddExpiredTime(timeout)
		return true
	} else {
		t.Close()
		return false
	}
}

func (t *Torrent) AddExpiredTime(duration time.Duration) {
	newExpiredTime := time.Now().Add(duration)
	if t.expiredTime.Before(newExpiredTime) {
		t.expiredTime = newExpiredTime
	}
}

func (t *Torrent) watch() {
	t.progressTicker = time.NewTicker(time.Second)
	defer t.progressTicker.Stop()

	for {
		select {
		case <-t.progressTicker.C:
			go t.progressEvent()
		case <-t.closed:
			return
		}
	}
}

func (t *Torrent) progressEvent() {
	// the downloaded files are recorded before an expiry can unload the torrent
	t.updatePinDownloaded()
	if t.expired() {
		if t.TorrentSpec != nil {
			log.TLogln("Torrent close by timeout", t.TorrentSpec.InfoHash.HexString())
		}
		t.bt.RemoveTorrent(t.Hash())
		return
	}

	t.muTorrent.Lock()
	if t.Torrent != nil && t.Torrent.Info() != nil {
		st := t.Torrent.Stats()
		deltaDlBytes := st.BytesRead.Int64() - t.BytesReadUsefulData
		deltaUpBytes := st.BytesWritten.Int64() - t.BytesWrittenData
		deltaTime := time.Since(t.lastTimeSpeed).Seconds()

		t.DownloadSpeed = float64(deltaDlBytes) / deltaTime
		t.UploadSpeed = float64(deltaUpBytes) / deltaTime

		t.BytesReadUsefulData = st.BytesRead.Int64()
		t.BytesWrittenData = st.BytesWritten.Int64()

		if t.cache != nil {
			t.PreloadedBytes = t.cache.GetState().Filled
		}
	} else {
		t.DownloadSpeed = 0
		t.UploadSpeed = 0
	}
	t.muTorrent.Unlock()

	t.lastTimeSpeed = time.Now()
	t.updateRA()
}

func (t *Torrent) updateRA() {
	// t.muTorrent.Lock()
	// defer t.muTorrent.Unlock()
	// if t.Torrent != nil && t.Torrent.Info() != nil {
	// 	pieceLen := t.Torrent.Info().PieceLength
	// 	adj := pieceLen * int64(t.Torrent.Stats().ActivePeers) / int64(1+t.cache.Readers())
	// 	switch {
	// 	case adj < pieceLen:
	// 		adj = pieceLen
	// 	case adj > pieceLen*4:
	// 		adj = pieceLen * 4
	// 	}
	// 	go t.cache.AdjustRA(adj)
	// }
	adj := int64(16 << 20) // 16 MB fixed RA
	go t.cache.AdjustRA(adj)
}

func (t *Torrent) expired() bool {
	if t.cache == nil {
		return false
	}
	// an unfinished pin keeps the torrent loaded
	if t.cache.PinPending() {
		return false
	}
	return t.cache.Readers() == 0 && t.expiredTime.Before(time.Now()) && (t.Stat == state.TorrentWorking || t.Stat == state.TorrentClosed)
}

func (t *Torrent) Files() []*torrent.File {
	if t.Torrent != nil && t.Torrent.Info() != nil {
		files := t.Torrent.Files()
		return files
	}
	return nil
}

func (t *Torrent) Hash() metainfo.Hash {
	if t.Torrent != nil {
		return t.Torrent.InfoHash()
	}
	if t.TorrentSpec != nil {
		return t.TorrentSpec.InfoHash
	}
	return [20]byte{}
}

func (t *Torrent) Length() int64 {
	if t.Info() == nil {
		return 0
	}
	return t.Torrent.Length()
}

func (t *Torrent) NewReader(file *torrent.File) *torrstor.Reader {
	if t.Stat == state.TorrentClosed {
		return nil
	}
	reader := t.cache.NewReader(file)
	return reader
}

func (t *Torrent) CloseReader(reader *torrstor.Reader) {
	t.cache.CloseReader(reader)
	t.AddExpiredTime(time.Second * time.Duration(settings.BTsets.TorrentDisconnectTimeout))
}

func (t *Torrent) GetCache() *torrstor.Cache {
	return t.cache
}

func (t *Torrent) drop() {
	t.muTorrent.Lock()
	defer t.muTorrent.Unlock()
	if t.Torrent != nil {
		t.Torrent.Drop()
		t.Torrent = nil
	}
}

func (t *Torrent) Close() bool {
	if t == nil {
		return false
	}
	if t.Stat == state.TorrentClosed {
		return true
	}
	if settings.ReadOnly && t.cache != nil && t.cache.GetUseReaders() > 0 {
		return false
	}
	t.Stat = state.TorrentClosed

	if t.bt != nil {
		t.bt.mu.Lock()
		if _, ok := t.bt.torrents[t.Hash()]; ok {
			delete(t.bt.torrents, t.Hash())
		}
		t.bt.mu.Unlock()
	}

	t.drop()
	return true
}

func (t *Torrent) Status() *state.TorrentStatus {
	t.muTorrent.Lock()
	defer t.muTorrent.Unlock()

	st := new(state.TorrentStatus)

	st.Stat = t.Stat
	st.StatString = t.Stat.String()
	st.Title = t.Title
	st.Category = t.Category
	st.Poster = t.Poster
	st.Data = t.Data
	st.Timestamp = t.Timestamp
	st.TorrentSize = t.Size
	st.BitRate = t.BitRate
	st.DurationSeconds = t.DurationSeconds
	st.PinMode = t.PinMode
	st.PinNext = t.PinNext

	pinned := t.PinMode == settings.PinModeAll || t.PinMode == settings.PinModeNext
	var hash metainfo.Hash
	if t.TorrentSpec != nil {
		hash = t.TorrentSpec.InfoHash
		st.Hash = t.TorrentSpec.InfoHash.HexString()
	}
	if t.Torrent != nil {
		st.Name = t.Torrent.Name()
		hash = t.Torrent.InfoHash()
		st.Hash = t.Torrent.InfoHash().HexString()
		st.LoadedSize = t.Torrent.BytesCompleted()

		st.PreloadedBytes = t.PreloadedBytes
		st.PreloadSize = t.PreloadSize
		st.DownloadSpeed = t.DownloadSpeed
		st.UploadSpeed = t.UploadSpeed

		tst := t.Torrent.Stats()
		st.BytesWritten = tst.BytesWritten.Int64()
		st.BytesWrittenData = tst.BytesWrittenData.Int64()
		st.BytesRead = tst.BytesRead.Int64()
		st.BytesReadData = tst.BytesReadData.Int64()
		st.BytesReadUsefulData = tst.BytesReadUsefulData.Int64()
		st.ChunksWritten = tst.ChunksWritten.Int64()
		st.ChunksRead = tst.ChunksRead.Int64()
		st.ChunksReadUseful = tst.ChunksReadUseful.Int64()
		st.ChunksReadWasted = tst.ChunksReadWasted.Int64()
		st.PiecesDirtiedGood = tst.PiecesDirtiedGood.Int64()
		st.PiecesDirtiedBad = tst.PiecesDirtiedBad.Int64()
		st.TotalPeers = tst.TotalPeers
		st.PendingPeers = tst.PendingPeers
		st.ActivePeers = tst.ActivePeers
		st.ConnectedSeeders = tst.ConnectedSeeders
		st.HalfOpenPeers = tst.HalfOpenPeers

		if t.Torrent.Info() != nil {
			st.TorrentSize = t.Torrent.Length()

			files := sortedFiles(t.Files())
			var pinFiles []pinFile
			for i, f := range files {
				st.FileStats = append(st.FileStats, &state.TorrentFileStat{
					Id:     i + 1, // in web id 0 is undefined
					Path:   f.Path(),
					Length: f.Length(),
				})
				if pinned {
					pinFiles = append(pinFiles, pinFile{id: i + 1, path: f.Path(), offset: f.Offset(), length: f.Length()})
				}
			}
			if pinned {
				targets := pinTargets(pinFiles, t.PinMode, t.PinNext, t.PinAnchor)
				for i, fs := range st.FileStats {
					fs.Pinned = targets[i]
					fs.Completed = fileCompleted(files[i])
					fs.Downloaded = slices.Contains(t.PinDownloaded, fs.Id)
				}
				st.PinProgress = pinProgress(pinFiles, targets, func(i int) int64 { return st.FileStats[i].Completed })
			}

			th := torrshash.New(st.Hash)
			th.AddField(torrshash.TagTitle, st.Title)
			th.AddField(torrshash.TagPoster, st.Poster)
			th.AddField(torrshash.TagCategory, st.Category)
			th.AddField(torrshash.TagSize, strconv.FormatInt(st.TorrentSize, 10))

			if t.TorrentSpec != nil {
				if len(t.TorrentSpec.Trackers) > 0 && len(t.TorrentSpec.Trackers[0]) > 0 {
					for _, tr := range t.TorrentSpec.Trackers[0] {
						th.AddField(torrshash.TagTracker, tr)
					}
				}
			}
			token, err := torrshash.Pack(th)
			if err == nil {
				st.TorrsHash = token
			}
		}
	} else if pinned {
		// a DB stub reports the pin from the stored file list and row
		files := dataFiles(t.Data)
		if len(files) > 0 {
			targets := pinTargets(files, t.PinMode, t.PinNext, t.PinAnchor)
			for i, f := range files {
				st.FileStats = append(st.FileStats, &state.TorrentFileStat{
					Id:         f.id,
					Path:       f.path,
					Length:     f.length,
					Pinned:     targets[i],
					Downloaded: slices.Contains(t.PinDownloaded, f.id),
				})
			}
			st.PinProgress = pinProgress(files, targets, func(i int) int64 {
				if st.FileStats[i].Downloaded {
					return files[i].length
				}
				return 0
			})
		}
	}
	// a pin whose target files are all downloaded needs no space
	if pinned && st.PinProgress < 100 && pinNoSpaceHas(hash) && settings.BTsets.UseDisk {
		st.PinError = state.PinErrorNoSpace
	}

	return st
}

func (t *Torrent) CacheState() *cacheSt.CacheState {
	if t.Torrent != nil && t.cache != nil {
		st := t.cache.GetState()
		st.Torrent = t.Status()
		return st
	}
	return nil
}
