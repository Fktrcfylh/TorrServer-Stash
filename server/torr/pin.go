package torr

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"server/log"
	"server/settings"
	"server/torr/state"
	utils2 "server/utils"
)

// pinFreeSpaceMargin is the free space a pin leaves on the file system of the
// torrents save path.
const pinFreeSpaceMargin = 1 << 30

// freeSpace returns the free space of a path; a variable for tests.
var freeSpace = utils2.FreeSpace

// no-space set: the hashes of pins paused for lack of free space, in memory only
var (
	pinNoSpaceMu sync.Mutex
	pinNoSpace   = map[metainfo.Hash]struct{}{}
)

// pinNoSpaceSet adds (on) or removes the hash and reports whether the set changed.
func pinNoSpaceSet(hash metainfo.Hash, on bool) (changed bool) {
	pinNoSpaceMu.Lock()
	defer pinNoSpaceMu.Unlock()
	_, has := pinNoSpace[hash]
	if on {
		pinNoSpace[hash] = struct{}{}
	} else {
		delete(pinNoSpace, hash)
	}
	return has != on
}

// pinNoSpaceHas reports whether the pin of the hash is paused for lack of space.
func pinNoSpaceHas(hash metainfo.Hash) bool {
	pinNoSpaceMu.Lock()
	defer pinNoSpaceMu.Unlock()
	_, has := pinNoSpace[hash]
	return has
}

// pinFile is the layout of one torrent file used to compute the pin set.
type pinFile struct {
	id             int
	path           string
	offset, length int64
}

// pinEpisodes returns the indexes into files of the episodes (video files that
// are not samples) and the index k into episodes of the anchor episode, 0 when
// the anchor is not an episode.
func pinEpisodes(files []pinFile, anchor int) (episodes []int, k int) {
	for i, f := range files {
		if utils2.IsVideoFile(f.path) && !utils2.IsSampleFile(f.path) {
			episodes = append(episodes, i)
		}
	}
	for i, fi := range episodes {
		if files[fi].id == anchor {
			return episodes, i
		}
	}
	return episodes, 0
}

// pinTargets marks the files (by index, files in id order) the pin downloads,
// nil when unpinned. Mode next with episodes targets the anchor episode and
// the next episodes after it; mode all, or a torrent without episodes,
// targets every file except the episodes before the anchor.
func pinTargets(files []pinFile, mode string, next, anchor int) []bool {
	if mode != settings.PinModeAll && mode != settings.PinModeNext {
		return nil
	}
	targets := make([]bool, len(files))
	episodes, k := pinEpisodes(files, anchor)
	if mode == settings.PinModeNext && len(episodes) > 0 {
		last := k + next
		if last > len(episodes)-1 {
			last = len(episodes) - 1
		}
		for i := k; i <= last; i++ {
			targets[episodes[i]] = true
		}
		return targets
	}
	for i := range targets {
		targets[i] = true
	}
	for _, fi := range episodes[:k] {
		targets[fi] = false
	}
	return targets
}

// pinProgress returns the downloaded percent of the target files, done
// returning the downloaded bytes of the file at an index.
func pinProgress(files []pinFile, targets []bool, done func(i int) int64) int {
	var total, sum int64
	for i, f := range files {
		if targets[i] && f.length > 0 {
			total += f.length
			sum += done(i)
		}
	}
	if total == 0 {
		return 0
	}
	return int(100 * sum / total)
}

// pinPieces computes the wanted and dropped pieces of a pin. files are in id
// order. Episodes before the anchor are dropped where no other file shares
// their pieces; mode all wants every other piece, mode next wants the pieces
// of the anchor episode and the next episodes after it.
func pinPieces(files []pinFile, pieceLength int64, pieceCount int, mode string, next, anchor int) (want, drop []bool) {
	if (mode != settings.PinModeAll && mode != settings.PinModeNext) || pieceLength <= 0 {
		return nil, nil
	}
	want = make([]bool, pieceCount)
	drop = make([]bool, pieceCount)

	// pieceRange returns the half-open piece range of a file
	pieceRange := func(f pinFile) (int, int) {
		if f.length <= 0 {
			return 0, 0
		}
		start := int(f.offset / pieceLength)
		end := int((f.offset + f.length + pieceLength - 1) / pieceLength)
		if start > pieceCount {
			start = pieceCount
		}
		if end > pieceCount {
			end = pieceCount
		}
		return start, end
	}

	episodes, k := pinEpisodes(files, anchor)
	deleted := make(map[int]bool, k)
	for _, fi := range episodes[:k] {
		deleted[fi] = true
	}

	deletedPieces := make([]bool, pieceCount)
	livePieces := make([]bool, pieceCount)
	for i, f := range files {
		start, end := pieceRange(f)
		for p := start; p < end; p++ {
			if deleted[i] {
				deletedPieces[p] = true
			} else {
				livePieces[p] = true
			}
		}
	}
	for p := range drop {
		drop[p] = deletedPieces[p] && !livePieces[p]
	}

	if mode == settings.PinModeAll || len(episodes) == 0 {
		for p := range want {
			want[p] = !drop[p]
		}
		return want, drop
	}

	last := k + next
	if last > len(episodes)-1 {
		last = len(episodes) - 1
	}
	for i := k; i <= last; i++ {
		start, end := pieceRange(files[episodes[i]])
		for p := start; p < end; p++ {
			want[p] = true
		}
	}
	return want, drop
}

// sortedFiles sorts files in place by path in natural order, which defines
// the file ids (i+1), and returns them.
func sortedFiles(files []*torrent.File) []*torrent.File {
	sort.Slice(files, func(i, j int) bool {
		return utils2.CompareStrings(files[i].Path(), files[j].Path())
	})
	return files
}

// ProbeQuery marks a loopback metadata probe of a stream URL (ffprobe); such a
// request is not playback and never moves the pin anchor.
const ProbeQuery = "probe"

// updatePinAnchor makes the started episode the pin anchor of a pinned
// torrent. The anchor is persisted and the pin applied asynchronously, so the
// stream is never blocked. It reports whether a save was scheduled.
func (t *Torrent) updatePinAnchor(fileID int, path, method string, probe bool) bool {
	if method != http.MethodGet || probe || t.Stat == state.TorrentPreload || settings.ReadOnly ||
		!utils2.IsVideoFile(path) || utils2.IsSampleFile(path) {
		return false
	}
	t.muTorrent.Lock()
	if (t.PinMode != settings.PinModeAll && t.PinMode != settings.PinModeNext) || t.PinAnchor == fileID {
		t.muTorrent.Unlock()
		return false
	}
	t.PinAnchor = fileID
	t.muTorrent.Unlock()

	hash := t.Hash()
	log.TLogln("Set pin anchor:", hash.HexString(), fileID)
	go t.savePinAnchor(hash)
	return true
}

// savePinAnchor stores the mirror anchor in the DB row and applies the pin.
func (t *Torrent) savePinAnchor(hash metainfo.Hash) {
	// the DB write runs outside muTorrent so concurrent streams do not wait for
	// it; it is repeated until the stored anchor is still the newest one
	for {
		t.muTorrent.Lock()
		anchor := t.PinAnchor
		t.muTorrent.Unlock()
		settings.SetTorrentPinAnchor(hash, anchor)
		t.muTorrent.Lock()
		newest := t.PinAnchor == anchor
		t.muTorrent.Unlock()
		if newest {
			break
		}
	}
	t.applyPin()
}

// fileCompleted returns the bytes of the file inside complete pieces; the
// torrent must have info.
func fileCompleted(f *torrent.File) int64 {
	var done int64
	for _, ps := range f.State() {
		if ps.Complete {
			done += ps.Bytes
		}
	}
	return done
}

// dataFiles returns the file list stored by TorrServer in the torrent data, in
// id order, nil when the data has no such list.
func dataFiles(data string) []pinFile {
	var tf tsFiles
	if err := json.Unmarshal([]byte(data), &tf); err != nil {
		return nil
	}
	var files []pinFile
	for _, f := range tf.TorrServer.Files {
		if f != nil && f.Id > 0 {
			files = append(files, pinFile{id: f.Id, path: f.Path, length: f.Length})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].id < files[j].id })
	return files
}

// pinFinished reports whether every non-empty target file of a stored pin is
// downloaded and its data dir exists. A row without a file list is never
// finished.
func pinFinished(db *settings.TorrentDB) bool {
	if db.PinMode != settings.PinModeAll && db.PinMode != settings.PinModeNext {
		return false
	}
	files := dataFiles(db.Data)
	targets := pinTargets(files, db.PinMode, db.PinNext, db.PinAnchor)
	found := false
	for i, f := range files {
		if !targets[i] || f.length <= 0 {
			continue
		}
		if !slices.Contains(db.PinDownloaded, f.id) {
			return false
		}
		found = true
	}
	if !found || db.TorrentSpec == nil {
		return false
	}
	// the ids describe the data in the save path they were recorded in; a
	// missing data dir (save path changed, data removed) is not finished
	_, err := os.Stat(filepath.Join(settings.BTsets.TorrentsSavePath, db.InfoHash.HexString()))
	return err == nil
}

// updatePinDownloaded stores the ids of the completely downloaded files of a
// pinned torrent when they changed.
func (t *Torrent) updatePinDownloaded() {
	if settings.ReadOnly || !settings.BTsets.UseDisk {
		return
	}
	t.muTorrent.Lock()
	pinned := t.PinMode == settings.PinModeAll || t.PinMode == settings.PinModeNext
	if !pinned || t.Torrent == nil || t.Torrent.Info() == nil {
		t.muTorrent.Unlock()
		return
	}
	var ids []int
	for i, f := range sortedFiles(t.Torrent.Files()) {
		if f.Length() > 0 && fileCompleted(f) == f.Length() {
			ids = append(ids, i+1)
		}
	}
	if slices.Equal(ids, t.PinDownloaded) {
		t.muTorrent.Unlock()
		return
	}
	t.PinDownloaded = ids
	hash := t.Torrent.InfoHash()
	t.muTorrent.Unlock()

	log.TLogln("Pin downloaded files:", hash.HexString(), ids)
	t.savePinDownloaded(hash)
}

// savePinDownloaded stores the mirror downloaded ids in the DB row.
func (t *Torrent) savePinDownloaded(hash metainfo.Hash) {
	// written outside muTorrent and repeated until the stored ids are still
	// the newest ones, as savePinAnchor
	for {
		t.muTorrent.Lock()
		ids := slices.Clone(t.PinDownloaded)
		t.muTorrent.Unlock()
		settings.SetTorrentPinDownloaded(hash, ids)
		t.muTorrent.Lock()
		newest := slices.Equal(t.PinDownloaded, ids)
		t.muTorrent.Unlock()
		if newest {
			break
		}
	}
}
