package torr

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"server/settings"
	"server/torr/state"
)

// bits returns a slice of n values with the given pieces set.
func bits(n int, ids ...int) []bool {
	ret := make([]bool, n)
	for _, id := range ids {
		ret[id] = true
	}
	return ret
}

func TestPinPieces(t *testing.T) {
	l := pinLayouts()
	contiguous, interleaved, subBoundary := l["contiguous"].files, l["interleaved"].files, l["subBoundary"].files
	zeroLength, naturalOrder, noEpisodes := l["zeroLength"].files, l["naturalOrder"].files, l["noEpisodes"].files

	tests := []struct {
		name        string
		files       []pinFile
		pieceLength int64
		pieceCount  int
		mode        string
		next        int
		anchor      int
		want, drop  []bool
	}{
		{"off", contiguous, 10, 10, "", 1, 0, nil, nil},
		{"invalid mode", contiguous, 10, 10, "bogus", 1, 0, nil, nil},
		{"zero piece length", contiguous, 0, 10, settings.PinModeAll, 1, 0, nil, nil},
		{"all no anchor", contiguous, 10, 10, settings.PinModeAll, 1, 0,
			bits(10, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9), bits(10)},
		{"next 1 no anchor", contiguous, 10, 10, settings.PinModeNext, 1, 0,
			bits(10, 0, 1, 2, 3), bits(10)},
		{"next 0", contiguous, 10, 10, settings.PinModeNext, 0, 0,
			bits(10, 0, 1), bits(10)},
		{"next clamps at the end", contiguous, 10, 10, settings.PinModeNext, 3, 4,
			bits(10, 6, 7, 8, 9), bits(10, 0, 1, 2, 3, 4, 5)},
		{"next anchor 3 keeps boundary", contiguous, 10, 10, settings.PinModeNext, 1, 3,
			bits(10, 3, 4, 5, 6, 7), bits(10, 0, 1, 2)},
		{"all anchor 3 keeps boundary", contiguous, 10, 10, settings.PinModeAll, 1, 3,
			bits(10, 3, 4, 5, 6, 7, 8, 9), bits(10, 0, 1, 2)},
		{"backward jump to 2", contiguous, 10, 10, settings.PinModeNext, 1, 2,
			bits(10, 2, 3, 4, 5), bits(10, 0, 1)},
		{"unknown anchor", contiguous, 10, 10, settings.PinModeNext, 0, 99,
			bits(10, 0, 1), bits(10)},
		{"interleaved next 1", interleaved, 10, 8, settings.PinModeNext, 1, 0,
			bits(8, 0, 1, 3), bits(8)},
		{"interleaved next 2 skips sample", interleaved, 10, 8, settings.PinModeNext, 2, 0,
			bits(8, 0, 1, 3, 5, 6), bits(8)},
		{"interleaved all", interleaved, 10, 8, settings.PinModeAll, 2, 0,
			bits(8, 0, 1, 2, 3, 4, 5, 6, 7), bits(8)},
		{"interleaved next anchor E03", interleaved, 10, 8, settings.PinModeNext, 0, 5,
			bits(8, 5, 6), bits(8, 0, 1, 3)},
		{"interleaved all anchor E03", interleaved, 10, 8, settings.PinModeAll, 0, 5,
			bits(8, 2, 4, 5, 6, 7), bits(8, 0, 1, 3)},
		{"anchor on subtitle", interleaved, 10, 8, settings.PinModeNext, 0, 2,
			bits(8, 0, 1), bits(8)},
		{"anchor on sample", interleaved, 10, 8, settings.PinModeNext, 0, 4,
			bits(8, 0, 1), bits(8)},
		{"subtitle boundary", subBoundary, 10, 6, settings.PinModeNext, 0, 4,
			bits(6, 4, 5), bits(6, 0, 1, 2)},
		{"zero length episode", zeroLength, 10, 4, settings.PinModeNext, 0, 2,
			bits(4), bits(4, 0, 1)},
		{"natural order", naturalOrder, 10, 6, settings.PinModeNext, 0, 2,
			bits(6, 0, 1), bits(6, 2, 3)},
		{"no episodes next", noEpisodes, 10, 4, settings.PinModeNext, 0, 1,
			bits(4, 0, 1, 2, 3), bits(4)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, drop := pinPieces(tt.files, tt.pieceLength, tt.pieceCount, tt.mode, tt.next, tt.anchor)
			if (want == nil) != (tt.want == nil) || !slices.Equal(want, tt.want) {
				t.Fatalf("want = %v, expected %v", want, tt.want)
			}
			if (drop == nil) != (tt.drop == nil) || !slices.Equal(drop, tt.drop) {
				t.Fatalf("drop = %v, expected %v", drop, tt.drop)
			}
		})
	}
}

const pinTestPieceLength = 16

// episodesSpec is a multi-file torrent of 5 episodes in pieces of 16 bytes:
// E01 0-1, E02 2-3, E03 3-5 (piece 3 shared with E02), E04 6-7, E05 8-9.
func episodesSpec(t *testing.T, name string) *torrent.TorrentSpec {
	t.Helper()
	lengths := []int64{32, 24, 40, 32, 32}
	info := metainfo.Info{Name: name, PieceLength: pinTestPieceLength, Pieces: make([]byte, 20*10)}
	for i, l := range lengths {
		info.Files = append(info.Files, metainfo.FileInfo{Length: l, Path: []string{"E0" + string(rune('1'+i)) + ".mkv"}})
	}
	buf, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return &torrent.TorrentSpec{InfoBytes: buf, InfoHash: metainfo.HashBytes(buf)}
}

// episodesTorrent loads the episodes spec on the offline client as an unpinned
// working torrent with a DB row and the given piece files written, without
// the watch loop.
func episodesTorrent(t *testing.T, name string, pieces ...int) *Torrent {
	t.Helper()
	spec := episodesSpec(t, name)
	goTorrent, _, err := bts.client.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	cache := bts.storage.GetCache(spec.InfoHash)
	if cache == nil {
		t.Fatal("storage has no cache for the torrent")
	}
	cache.SetTorrent(goTorrent)
	tor := &Torrent{TorrentSpec: spec, Torrent: goTorrent, Stat: state.TorrentWorking, bt: bts, cache: cache}
	bts.mu.Lock()
	bts.torrents[spec.InfoHash] = tor
	bts.mu.Unlock()
	AddTorrentDB(tor)
	tor.applyPin()
	for _, id := range pieces {
		if _, err := cache.Piece(goTorrent.Info().Piece(id)).WriteAt([]byte{1}, 0); err != nil {
			t.Fatal(err)
		}
	}
	return tor
}

func pieceFile(tor *Torrent, id int) string {
	return filepath.Join(settings.BTsets.TorrentsSavePath, tor.Hash().HexString(), string(rune('0'+id)))
}

func pieceFileExists(t *testing.T, tor *Torrent, id int) bool {
	t.Helper()
	_, err := os.Stat(pieceFile(tor, id))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// waitWanted polls until wanted pieces have priority Normal and the others
// None, as set by the asynchronous priority refresh of a pin change.
func waitWanted(t *testing.T, tor *Torrent, want []bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := make([]bool, tor.Torrent.NumPieces())
		exact := true
		for i := range got {
			prio := any(tor.Torrent.PieceState(i).Priority)
			got[i] = prio == any(torrent.PiecePriorityNormal)
			if !got[i] && prio != any(torrent.PiecePriorityNone) {
				exact = false
			}
		}
		if exact && slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("wanted pieces = %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitPieceFilesGone polls an asynchronous piece drop.
func waitPieceFilesGone(t *testing.T, tor *Torrent, ids ...int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, id := range ids {
		for pieceFileExists(t, tor, id) {
			if time.Now().After(deadline) {
				t.Fatalf("piece file %d still exists", id)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func assertPieceFiles(t *testing.T, tor *Torrent, ids ...int) {
	t.Helper()
	for _, id := range ids {
		if !pieceFileExists(t, tor, id) {
			t.Fatalf("piece file %d was removed", id)
		}
	}
}

func TestApplyPinNextWindow(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "window", 6, 7, 8, 9)
	hash := tor.Hash().HexString()

	SetTorrentPin(hash, settings.PinModeNext, 1)
	waitWanted(t, tor, bits(10, 0, 1, 2, 3))
	if !tor.GetCache().PinPending() {
		t.Fatal("next window with incomplete pieces is not pending")
	}

	SetTorrentPin(hash, settings.PinModeAll, 1)
	waitWanted(t, tor, bits(10, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9))

	SetTorrentPin(hash, settings.PinModeNext, 0)
	waitWanted(t, tor, bits(10, 0, 1))
	// the anchor is unchanged, so the drop set is empty and data outside the
	// window stays
	if _, drop := pinPieces(pinFilesOf(tor), pinTestPieceLength, 10, settings.PinModeNext, 0, 0); slices.Contains(drop, true) {
		t.Fatalf("drop set without an anchor = %v", drop)
	}
	assertPieceFiles(t, tor, 6, 7, 8, 9)
}

// pinFilesOf builds the pin layout of a loaded torrent as applyPin does.
func pinFilesOf(tor *Torrent) []pinFile {
	var files []pinFile
	for i, f := range sortedFiles(tor.Torrent.Files()) {
		files = append(files, pinFile{id: i + 1, path: f.Path(), offset: f.Offset(), length: f.Length()})
	}
	return files
}

func TestUpdatePinAnchorConditions(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "conditions")
	hash := tor.Hash()
	SetTorrentPin(hash.HexString(), settings.PinModeNext, 1)
	episode := tor.Torrent.Files()[2].Path()

	anchor := func() int {
		tor.muTorrent.Lock()
		defer tor.muTorrent.Unlock()
		return tor.PinAnchor
	}
	assertUnchanged := func(name string) {
		t.Helper()
		if got := anchor(); got != 0 {
			t.Fatalf("%s: mirror anchor = %d, want 0", name, got)
		}
		if db := GetTorrentDB(hash); db.PinAnchor != 0 {
			t.Fatalf("%s: DB anchor = %d, want 0", name, db.PinAnchor)
		}
	}

	// a condition that fails schedules no save, so nothing can be written later
	skip := func(name string, fileID int, path, method string, probe bool) {
		t.Helper()
		if tor.updatePinAnchor(fileID, path, method, probe) {
			t.Fatalf("%s: a pin anchor save was scheduled", name)
		}
		assertUnchanged(name)
	}

	skip("HEAD", 3, episode, http.MethodHead, false)
	skip("probe", 3, episode, http.MethodGet, true)

	tor.Stat = state.TorrentPreload
	skip("preload", 3, episode, http.MethodGet, false)
	tor.Stat = state.TorrentWorking

	skip("subtitle", 3, "show/E03.srt", http.MethodGet, false)
	skip("sample", 3, "show/E03.sample.mkv", http.MethodGet, false)

	settings.ReadOnly = true
	skip("read-only", 3, episode, http.MethodGet, false)
	settings.ReadOnly = false

	tor.muTorrent.Lock()
	tor.PinMode = ""
	tor.muTorrent.Unlock()
	skip("unpinned", 3, episode, http.MethodGet, false)
	tor.muTorrent.Lock()
	tor.PinMode = settings.PinModeNext
	tor.muTorrent.Unlock()

	if !tor.updatePinAnchor(3, episode, http.MethodGet, false) {
		t.Fatal("GET: no pin anchor save was scheduled")
	}
	if got := anchor(); got != 3 {
		t.Fatalf("GET: mirror anchor = %d, want 3", got)
	}
	waitDBAnchor(t, hash, 3)

	// the same file again leaves the anchor as is and schedules no save
	if tor.updatePinAnchor(3, episode, http.MethodGet, false) {
		t.Fatal("same file: a pin anchor save was scheduled")
	}
	if got := anchor(); got != 3 {
		t.Fatalf("same file: mirror anchor = %d, want 3", got)
	}
}

func waitDBAnchor(t *testing.T, hash metainfo.Hash, anchor int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if db := GetTorrentDB(hash); db != nil && db.PinAnchor == anchor {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("DB anchor was not set to %d", anchor)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSavePinAnchor(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "save")
	hash := tor.Hash()
	SetTorrentPin(hash.HexString(), settings.PinModeNext, 0)
	waitWanted(t, tor, bits(10, 0, 1))

	tor.muTorrent.Lock()
	tor.PinAnchor = 4
	tor.muTorrent.Unlock()
	tor.savePinAnchor(hash)

	if db := GetTorrentDB(hash); db.PinAnchor != 4 {
		t.Fatalf("DB anchor = %d, want 4", db.PinAnchor)
	}
	waitWanted(t, tor, bits(10, 6, 7))
}

func TestPinAnchorMoveDropsWatched(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "move", 0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	hash := tor.Hash()
	SetTorrentPin(hash.HexString(), settings.PinModeNext, 1)
	waitWanted(t, tor, bits(10, 0, 1, 2, 3))
	files := tor.Torrent.Files()

	tor.updatePinAnchor(3, files[2].Path(), http.MethodGet, false)
	waitDBAnchor(t, hash, 3)
	waitPieceFilesGone(t, tor, 0, 1, 2)
	waitWanted(t, tor, bits(10, 3, 4, 5, 6, 7))
	assertPieceFiles(t, tor, 3, 4, 5, 6, 7, 8, 9)

	tor.updatePinAnchor(2, files[1].Path(), http.MethodGet, false)
	waitDBAnchor(t, hash, 2)
	waitWanted(t, tor, bits(10, 2, 3, 4, 5))
	assertPieceFiles(t, tor, 3, 4, 5, 6, 7, 8, 9)
	for _, id := range []int{0, 1, 2} {
		if pieceFileExists(t, tor, id) {
			t.Fatalf("piece file %d recreated", id)
		}
	}
}

// applyPin numbers files in natural path order, not in torrent byte order,
// even before Status has sorted the file list.
func TestApplyPinUsesNaturalFileOrder(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	info := metainfo.Info{Name: "order", PieceLength: pinTestPieceLength, Pieces: make([]byte, 20*6)}
	for _, name := range []string{"E10.mkv", "E3.mkv", "E2.mkv"} {
		info.Files = append(info.Files, metainfo.FileInfo{Length: 32, Path: []string{name}})
	}
	buf, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	spec := &torrent.TorrentSpec{InfoBytes: buf, InfoHash: metainfo.HashBytes(buf)}
	goTorrent, _, err := bts.client.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	cache := bts.storage.GetCache(spec.InfoHash)
	if cache == nil {
		t.Fatal("storage has no cache for the torrent")
	}
	cache.SetTorrent(goTorrent)
	tor := &Torrent{TorrentSpec: spec, Torrent: goTorrent, Stat: state.TorrentWorking, bt: bts, cache: cache}
	tor.PinMode = settings.PinModeNext

	tor.applyPin()
	// E2 is the first episode by id and the last file by bytes
	waitWanted(t, tor, bits(6, 4, 5))
}

// completePieces writes the full data of the pieces through the cache, marks
// them complete and makes anacrolix re-read their completion, so PieceState
// and File.State see them complete.
func completePieces(t *testing.T, tor *Torrent, ids ...int) {
	t.Helper()
	info := tor.Torrent.Info()
	for _, id := range ids {
		p := tor.GetCache().Piece(info.Piece(id))
		if _, err := p.WriteAt(make([]byte, info.Piece(id).Length()), 0); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkComplete(); err != nil {
			t.Fatal(err)
		}
		tor.Torrent.Piece(id).UpdateCompletion()
		if !tor.Torrent.PieceState(id).Complete {
			t.Fatalf("piece %d is not complete in anacrolix", id)
		}
	}
}

// pinLayout is a file layout of TestPinPieces in pieces of 10 bytes.
type pinLayout struct {
	files      []pinFile
	pieceCount int
}

func pinLayouts() map[string]pinLayout {
	return map[string]pinLayout{
		// 5 episodes; piece 3 is shared by E02 and E03
		"contiguous": {[]pinFile{
			{1, "show/E01.mkv", 0, 20},
			{2, "show/E02.mkv", 20, 15},
			{3, "show/E03.mkv", 35, 25},
			{4, "show/E04.mkv", 60, 20},
			{5, "show/E05.mkv", 80, 20},
		}, 10},
		// subtitle and sample between episodes, each in its own piece
		"interleaved": {[]pinFile{
			{1, "show/E01.mkv", 0, 20},
			{2, "show/E01.srt", 20, 10},
			{3, "show/E02.mkv", 30, 10},
			{4, "show/E02.sample.mkv", 40, 10},
			{5, "show/E03.mkv", 50, 20},
			{6, "show/E04.mkv", 70, 10},
		}, 8},
		// piece 3 is shared by E02 and a subtitle
		"subBoundary": {[]pinFile{
			{1, "show/E01.mkv", 0, 20},
			{2, "show/E02.mkv", 20, 15},
			{3, "show/E02.srt", 35, 5},
			{4, "show/E03.mkv", 40, 20},
		}, 6},
		"zeroLength": {[]pinFile{
			{1, "show/E01.mkv", 0, 20},
			{2, "show/E02.mkv", 25, 0},
			{3, "show/E03.mkv", 30, 10},
			{4, "show/extra.nfo", 20, 10},
		}, 4},
		// natural order puts E2 before E10, their bytes are in the other order
		"naturalOrder": {[]pinFile{
			{1, "show/E2.mkv", 20, 20},
			{2, "show/E10.mkv", 0, 20},
			{3, "show/E11.mkv", 40, 20},
		}, 6},
		"noEpisodes": {[]pinFile{
			{1, "book/a.mp3", 0, 20},
			{2, "book/b.txt", 20, 20},
		}, 4},
	}
}

func TestPinTargets(t *testing.T) {
	l := pinLayouts()
	contiguous, interleaved, noEpisodes := l["contiguous"].files, l["interleaved"].files, l["noEpisodes"].files
	tests := []struct {
		name   string
		files  []pinFile
		mode   string
		next   int
		anchor int
		want   []bool
	}{
		{"off", contiguous, "", 1, 0, nil},
		{"invalid mode", contiguous, "bogus", 1, 0, nil},
		{"all no anchor", contiguous, settings.PinModeAll, 1, 0, bits(5, 0, 1, 2, 3, 4)},
		{"next 1", contiguous, settings.PinModeNext, 1, 0, bits(5, 0, 1)},
		{"next 0", contiguous, settings.PinModeNext, 0, 0, bits(5, 0)},
		{"next clamps at the end", contiguous, settings.PinModeNext, 3, 4, bits(5, 3, 4)},
		{"all anchor 3", contiguous, settings.PinModeAll, 1, 3, bits(5, 2, 3, 4)},
		{"next anchor 3", contiguous, settings.PinModeNext, 1, 3, bits(5, 2, 3)},
		{"interleaved next 2", interleaved, settings.PinModeNext, 2, 0, bits(6, 0, 2, 4)},
		{"interleaved all", interleaved, settings.PinModeAll, 0, 0, bits(6, 0, 1, 2, 3, 4, 5)},
		{"interleaved all anchor E03", interleaved, settings.PinModeAll, 0, 5, bits(6, 1, 3, 4, 5)},
		{"interleaved next anchor E03", interleaved, settings.PinModeNext, 1, 5, bits(6, 4, 5)},
		{"no episodes next", noEpisodes, settings.PinModeNext, 0, 1, bits(2, 0, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pinTargets(tt.files, tt.mode, tt.next, tt.anchor)
			if (got == nil) != (tt.want == nil) || !slices.Equal(got, tt.want) {
				t.Fatalf("pinTargets = %v, want %v", got, tt.want)
			}
		})
	}
}

// For every layout the wanted pieces are the pieces of the target files.
func TestPinTargetsMatchPinPieces(t *testing.T) {
	const pieceLength = 10
	for name, l := range pinLayouts() {
		for _, mode := range []string{settings.PinModeAll, settings.PinModeNext} {
			for next := 0; next <= 3; next++ {
				for anchor := 0; anchor <= len(l.files)+1; anchor++ {
					want, _ := pinPieces(l.files, pieceLength, l.pieceCount, mode, next, anchor)
					targets := pinTargets(l.files, mode, next, anchor)
					union := make([]bool, l.pieceCount)
					for i, f := range l.files {
						if !targets[i] || f.length <= 0 {
							continue
						}
						for p := f.offset / pieceLength; p < (f.offset+f.length+pieceLength-1)/pieceLength; p++ {
							union[p] = true
						}
					}
					if !slices.Equal(want, union) {
						t.Fatalf("%s %s next %d anchor %d: want %v, target pieces %v", name, mode, next, anchor, want, union)
					}
				}
			}
		}
	}
}

func TestDataFiles(t *testing.T) {
	data := `{"TorrServer":{"Files":[{"id":2,"path":"b.mkv","length":20},{"path":"x.nfo","length":5},{"id":1,"path":"a.mkv","length":10}]}}`
	want := []pinFile{{id: 1, path: "a.mkv", length: 10}, {id: 2, path: "b.mkv", length: 20}}
	if got := dataFiles(data); !slices.Equal(got, want) {
		t.Fatalf("dataFiles = %v, want %v", got, want)
	}
	for name, data := range map[string]string{
		"client data":  `{"poster":"x","files":[1]}`,
		"empty":        "",
		"invalid json": `{"TorrServer":`,
	} {
		if got := dataFiles(data); got != nil {
			t.Fatalf("%s: dataFiles = %v, want nil", name, got)
		}
	}
}

// episodesData is the TorrServer file list of episodesSpec.
func episodesData(t *testing.T) string {
	t.Helper()
	var files tsFiles
	for i, l := range []int64{32, 24, 40, 32, 32} {
		files.TorrServer.Files = append(files.TorrServer.Files, &state.TorrentFileStat{
			Id: i + 1, Path: "E0" + string(rune('1'+i)) + ".mkv", Length: l,
		})
	}
	buf, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func TestPinFinished(t *testing.T) {
	episodes := episodesData(t)
	withSubtitle := `{"TorrServer":{"Files":[{"id":1,"path":"E01.mkv","length":32},{"id":2,"path":"E01.srt","length":8},{"id":3,"path":"E02.mkv","length":32}]}}`
	zeroTarget := `{"TorrServer":{"Files":[{"id":1,"path":"E01.mkv","length":32},{"id":2,"path":"E02.mkv"}]}}`
	onlyZero := `{"TorrServer":{"Files":[{"id":1,"path":"E01.mkv"}]}}`
	tests := []struct {
		name       string
		data, mode string
		next       int
		anchor     int
		downloaded []int
		want       bool
	}{
		{"all targets downloaded", episodes, settings.PinModeNext, 1, 0, []int{1, 2}, true},
		{"one target missing", episodes, settings.PinModeNext, 1, 0, []int{1}, false},
		{"all mode missing one", episodes, settings.PinModeAll, 0, 0, []int{1, 2, 3, 4}, false},
		{"all mode complete", episodes, settings.PinModeAll, 0, 0, []int{1, 2, 3, 4, 5}, true},
		{"non-target missing only", withSubtitle, settings.PinModeNext, 0, 0, []int{1}, true},
		{"data without list", `{"poster":"x"}`, settings.PinModeNext, 1, 0, []int{1, 2}, false},
		{"zero-length target ignored", zeroTarget, settings.PinModeNext, 1, 0, []int{1}, true},
		{"only zero-length targets", onlyZero, settings.PinModeNext, 1, 0, nil, false},
		{"mode off", episodes, "", 1, 0, []int{1, 2, 3, 4, 5}, false},
		{"anchor moved to a new target", episodes, settings.PinModeNext, 1, 2, []int{1, 2}, false},
		{"N raised to a new target", episodes, settings.PinModeNext, 2, 0, []int{1, 2}, false},
	}
	withTorrDB(t)
	withDiskMode(t)
	hash := testHash(1)
	if err := os.MkdirAll(filepath.Join(settings.BTsets.TorrentsSavePath, hash.HexString()), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &settings.TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}, Data: tt.data, PinMode: tt.mode, PinNext: tt.next, PinAnchor: tt.anchor, PinDownloaded: tt.downloaded}
			if got := pinFinished(db); got != tt.want {
				t.Fatalf("pinFinished = %v, want %v", got, tt.want)
			}
		})
	}

	// the save path changed to one without the data dir
	db := &settings.TorrentDB{TorrentSpec: &torrent.TorrentSpec{InfoHash: hash}, Data: episodes, PinMode: settings.PinModeNext, PinNext: 1, PinDownloaded: []int{1, 2}}
	settings.BTsets.TorrentsSavePath = t.TempDir()
	if pinFinished(db) {
		t.Fatal("pinFinished = true without the data dir in the save path")
	}
}

func TestPinNoSpaceSet(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	if pinNoSpaceHas(hash) {
		t.Fatal("empty set has the hash")
	}
	if !pinNoSpaceSet(hash, true) || !pinNoSpaceHas(hash) {
		t.Fatal("adding the hash did not change the set")
	}
	if pinNoSpaceSet(hash, true) {
		t.Fatal("adding the hash again changed the set")
	}
	if pinNoSpaceHas(testHash(2)) {
		t.Fatal("set has another hash")
	}
	if !pinNoSpaceSet(hash, false) || pinNoSpaceHas(hash) {
		t.Fatal("removing the hash did not change the set")
	}
	if pinNoSpaceSet(hash, false) {
		t.Fatal("removing a missing hash changed the set")
	}
}

func mirrorDownloaded(tor *Torrent) []int {
	tor.muTorrent.Lock()
	defer tor.muTorrent.Unlock()
	return slices.Clone(tor.PinDownloaded)
}

func TestUpdatePinDownloaded(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "downloaded")
	hash := tor.Hash()
	SetTorrentPin(hash.HexString(), settings.PinModeNext, 1)
	waitWanted(t, tor, bits(10, 0, 1, 2, 3))

	// E02 is only partly complete
	completePieces(t, tor, 0, 1, 2)
	tor.updatePinDownloaded()
	if got := mirrorDownloaded(tor); !slices.Equal(got, []int{1}) {
		t.Fatalf("mirror downloaded = %v, want [1]", got)
	}
	if db := GetTorrentDB(hash); !slices.Equal(db.PinDownloaded, []int{1}) {
		t.Fatalf("DB downloaded = %v, want [1]", db.PinDownloaded)
	}

	// no change: the row edited behind the mirror's back is not rewritten
	settings.SetTorrentPinDownloaded(hash, []int{9})
	tor.updatePinDownloaded()
	if db := GetTorrentDB(hash); !slices.Equal(db.PinDownloaded, []int{9}) {
		t.Fatalf("unchanged ids were written: DB downloaded = %v", db.PinDownloaded)
	}

	// moving the anchor to E03 drops the pieces of E01, which is then removed
	tor.updatePinAnchor(3, tor.Torrent.Files()[2].Path(), http.MethodGet, false)
	waitPieceFilesGone(t, tor, 0, 1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		tor.updatePinDownloaded()
		if mirrorDownloaded(tor) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mirror downloaded = %v after the drop, want none", mirrorDownloaded(tor))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if db := GetTorrentDB(hash); db.PinDownloaded != nil {
		t.Fatalf("DB downloaded = %v after the drop, want none", db.PinDownloaded)
	}
}

func TestUpdatePinDownloadedSkips(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)

	assertNothing := func(name string, tor *Torrent) {
		t.Helper()
		tor.updatePinDownloaded()
		if got := mirrorDownloaded(tor); got != nil {
			t.Fatalf("%s: mirror downloaded = %v", name, got)
		}
		if db := GetTorrentDB(tor.Hash()); db.PinDownloaded != nil {
			t.Fatalf("%s: DB downloaded = %v", name, db.PinDownloaded)
		}
	}

	unpinned := episodesTorrent(t, "skip-unpinned")
	completePieces(t, unpinned, 0, 1)
	assertNothing("unpinned", unpinned)

	pinned := episodesTorrent(t, "skip-pinned")
	SetTorrentPin(pinned.Hash().HexString(), settings.PinModeAll, 0)
	completePieces(t, pinned, 0, 1)

	settings.ReadOnly = true
	assertNothing("read-only", pinned)
	// status reports the stored ids, not the live completion
	if fs := pinned.Status().FileStats[0]; fs.Completed != 32 || fs.Downloaded {
		t.Fatalf("read-only status file 1 = %+v, want completed 32 not downloaded", fs)
	}
	settings.ReadOnly = false

	settings.BTsets.UseDisk = false
	assertNothing("memory storage", pinned)
	settings.BTsets.UseDisk = true

	pinned.updatePinDownloaded()
	if got := mirrorDownloaded(pinned); !slices.Equal(got, []int{1}) {
		t.Fatalf("mirror downloaded = %v, want [1]", got)
	}
}

// the downloaded files are recorded before an expired torrent is unloaded
func TestProgressEventRecordsDownloadedBeforeExpiry(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "progress")
	hash := tor.Hash()
	SetTorrentPin(hash.HexString(), settings.PinModeNext, 0)
	completePieces(t, tor, 0, 1)
	tor.expiredTime = time.Now().Add(-time.Minute)
	if !tor.expired() {
		t.Fatal("torrent with a complete window did not expire")
	}

	tor.progressEvent()
	if bts.GetTorrent(hash) != nil {
		t.Fatal("expired torrent was not removed")
	}
	if db := GetTorrentDB(hash); !slices.Equal(db.PinDownloaded, []int{1}) {
		t.Fatalf("DB downloaded = %v, want [1]", db.PinDownloaded)
	}
}

// pausedTorrent loads the episodes as a pin next N=1 with the window pieces
// wanted, then applies it again with free space short of the margin.
func pausedTorrent(t *testing.T, name string) (*Torrent, *freeSpaceDouble) {
	t.Helper()
	savePath := settings.BTsets.TorrentsSavePath
	fs := withFreeSpace(t, map[string]uint64{savePath: 1 << 40})
	tor := episodesTorrent(t, name)
	SetTorrentPin(tor.Hash().HexString(), settings.PinModeNext, 1)
	waitWanted(t, tor, bits(10, 0, 1, 2, 3))
	if pinNoSpaceHas(tor.Hash()) {
		t.Fatal("pin with enough space is paused")
	}

	// the window E01, E02 is 4 pieces of 16 bytes
	fs.set(savePath, pinFreeSpaceMargin+4*pinTestPieceLength-1)
	tor.applyPin()
	if !pinNoSpaceHas(tor.Hash()) {
		t.Fatal("pin short of free space is not paused")
	}
	waitWanted(t, tor, bits(10))
	return tor, fs
}

func TestApplyPinPausesWithoutSpace(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor, fs := pausedTorrent(t, "pause")
	savePath := settings.BTsets.TorrentsSavePath
	if !fs.calledWith(savePath) {
		t.Fatal("free space was not checked on the torrents save path")
	}
	if tor.GetCache().PinPending() {
		t.Fatal("paused pin is pending")
	}
	tor.expiredTime = time.Now().Add(-time.Minute)
	if !tor.expired() {
		t.Fatal("paused pin past its expiry did not expire")
	}
	if st := tor.Status(); st.PinError != state.PinErrorNoSpace {
		t.Fatalf("status pin error = %q, want %q", st.PinError, state.PinErrorNoSpace)
	}

	// exactly the required bytes plus the margin is enough
	fs.set(savePath, pinFreeSpaceMargin+4*pinTestPieceLength)
	tor.applyPin()
	if pinNoSpaceHas(tor.Hash()) {
		t.Fatal("pin with the required space plus the margin is paused")
	}
	waitWanted(t, tor, bits(10, 0, 1, 2, 3))
	if !tor.GetCache().PinPending() {
		t.Fatal("resumed pin is not pending")
	}
}

func TestApplyPinUnknownSpaceNotPaused(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	withFreeSpace(t, map[string]uint64{"/other": 0})
	tor := episodesTorrent(t, "unknown")

	SetTorrentPin(tor.Hash().HexString(), settings.PinModeNext, 1)
	if pinNoSpaceHas(tor.Hash()) {
		t.Fatal("pin with unknown free space is paused")
	}
	waitWanted(t, tor, bits(10, 0, 1, 2, 3))
}

func TestApplyPinCompleteWindowNotPaused(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "complete")
	completePieces(t, tor, 0, 1, 2, 3)
	withFreeSpace(t, map[string]uint64{settings.BTsets.TorrentsSavePath: 0})

	SetTorrentPin(tor.Hash().HexString(), settings.PinModeNext, 1)
	if pinNoSpaceHas(tor.Hash()) {
		t.Fatal("pin with nothing to download is paused")
	}
	if tor.GetCache().PinPending() {
		t.Fatal("complete window is pending")
	}
}

func TestApplyPinClearsNoSpace(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)

	memory, _ := pausedTorrent(t, "clear-memory")
	settings.BTsets.UseDisk = false
	memory.applyPin()
	if pinNoSpaceHas(memory.Hash()) {
		t.Fatal("applyPin without disk storage kept the no-space state")
	}
	settings.BTsets.UseDisk = true

	off, _ := pausedTorrent(t, "clear-off")
	off.muTorrent.Lock()
	off.PinMode = ""
	off.muTorrent.Unlock()
	off.applyPin()
	if pinNoSpaceHas(off.Hash()) {
		t.Fatal("applyPin of an unpinned torrent kept the no-space state")
	}

	// without info the free space is unknown, the state is kept
	stub := &Torrent{TorrentSpec: &torrent.TorrentSpec{InfoHash: testHash(7)}, PinMode: settings.PinModeAll}
	pinNoSpaceSet(testHash(7), true)
	stub.applyPin()
	if !pinNoSpaceHas(testHash(7)) {
		t.Fatal("applyPin without info cleared the no-space state")
	}
}

// A paused pin keeps its pieces from eviction and still drops by the anchor;
// the synchronous priority paths are covered by torrstor
// TestPausedPinSetRaisesNothing.
func TestPausedPinDropsAndRaisesNothing(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor, _ := pausedTorrent(t, "paused-drop")
	hash := tor.Hash()
	for _, id := range []int{0, 1, 2, 3, 4, 5} {
		if _, err := tor.GetCache().Piece(tor.Torrent.Info().Piece(id)).WriteAt([]byte{1}, 0); err != nil {
			t.Fatal(err)
		}
	}

	// a reader opened and closed does not raise the paused window
	reader := tor.NewReader(tor.Torrent.Files()[0])
	tor.CloseReader(reader)

	tor.updatePinAnchor(3, tor.Torrent.Files()[2].Path(), http.MethodGet, false)
	waitDBAnchor(t, hash, 3)
	waitPieceFilesGone(t, tor, 0, 1, 2)
	assertPieceFiles(t, tor, 3, 4, 5)
	if !pinNoSpaceHas(hash) {
		t.Fatal("anchor move resumed a pin without space")
	}
	waitWanted(t, tor, bits(10))
}

func TestResumePinnedRecoversPaused(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor, fs := pausedTorrent(t, "recover")

	fs.set(settings.BTsets.TorrentsSavePath, 1<<40)
	resumePinned()
	if pinNoSpaceHas(tor.Hash()) {
		t.Fatal("resumePinned kept the no-space state with enough space")
	}
	waitWanted(t, tor, bits(10, 0, 1, 2, 3))
	if !tor.GetCache().PinPending() {
		t.Fatal("recovered pin is not pending")
	}

	// no BTServer: nothing to apply
	bts = nil
	resumePinned()
}

func TestPausedStubKeepsPinError(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor, _ := pausedTorrent(t, "stub-error")
	hash := tor.Hash()

	bts.RemoveTorrent(hash)
	if bts.GetTorrent(hash) != nil {
		t.Fatal("torrent was not removed")
	}
	if st := GetTorrentDB(hash).Status(); st.PinError != state.PinErrorNoSpace {
		t.Fatalf("stub pin error = %q, want %q", st.PinError, state.PinErrorNoSpace)
	}
}

func TestPinConcurrentStatusDownloadedApply(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	withOfflineClient(t)
	tor := episodesTorrent(t, "concurrent")
	SetTorrentPin(tor.Hash().HexString(), settings.PinModeNext, 1)

	var wg sync.WaitGroup
	for _, run := range []func(){
		func() { tor.Status() },
		tor.updatePinDownloaded,
		tor.applyPin,
		func() {
			tor.muTorrent.Lock()
			tor.PinNext = 1 - tor.PinNext
			tor.muTorrent.Unlock()
		},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				run()
			}
		}()
	}
	completePieces(t, tor, 0, 1)
	wg.Wait()
	tor.updatePinDownloaded()
	if got := mirrorDownloaded(tor); !slices.Equal(got, []int{1}) {
		t.Fatalf("mirror downloaded = %v, want [1]", got)
	}
}
