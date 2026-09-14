package settings

import (
	"encoding/json"
	"sort"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"server/log"
)

// Pin modes accepted by the API; off is stored as an empty string.
const (
	PinModeOff  = "off"
	PinModeAll  = "all"
	PinModeNext = "next"
)

type TorrentDB struct {
	*torrent.TorrentSpec

	Title    string `json:"title,omitempty"`
	Category string `json:"category,omitempty"`
	Poster   string `json:"poster,omitempty"`
	Data     string `json:"data,omitempty"`

	Timestamp int64 `json:"timestamp,omitempty"`
	Size      int64 `json:"size,omitempty"`

	PinMode       string `json:"pin_mode,omitempty"`
	PinNext       int    `json:"pin_next,omitempty"`
	PinAnchor     int    `json:"pin_anchor,omitempty"`
	PinDownloaded []int  `json:"pin_downloaded,omitempty"`
}

type File struct {
	Name string `json:"name,omitempty"`
	Id   int    `json:"id,omitempty"`
	Size int64  `json:"size,omitempty"`
}

var mu sync.Mutex

func AddTorrent(torr *TorrentDB) {
	mu.Lock()
	defer mu.Unlock()
	// the pin is owned by the stored row: a general save never changes it
	if cur := getTorrentRow(torr.InfoHash); cur != nil {
		torr.PinMode = cur.PinMode
		torr.PinNext = cur.PinNext
		torr.PinAnchor = cur.PinAnchor
		torr.PinDownloaded = cur.PinDownloaded
	}
	// only the saved row is written, so a stale snapshot never reverts other rows
	buf, err := json.Marshal(torr)
	if err == nil {
		tdb.Set("Torrents", torr.InfoHash.HexString(), buf)
	}
}

// SetTorrentPin updates only the pin mode and N of an existing row; off also
// clears the downloaded file ids.
func SetTorrentPin(hash metainfo.Hash, mode string, next int) bool {
	mu.Lock()
	defer mu.Unlock()
	db := getTorrentRow(hash)
	if db == nil {
		return false
	}
	db.PinMode = mode
	db.PinNext = next
	if mode == "" {
		db.PinDownloaded = nil
	}
	buf, err := json.Marshal(db)
	if err != nil {
		log.TLogln("Error marshal torrent pin", hash.HexString(), err)
		return false
	}
	tdb.Set("Torrents", hash.HexString(), buf)
	return true
}

// SetTorrentPinAnchor updates only the pin anchor (file id) of an existing row.
func SetTorrentPinAnchor(hash metainfo.Hash, anchor int) bool {
	mu.Lock()
	defer mu.Unlock()
	db := getTorrentRow(hash)
	if db == nil {
		return false
	}
	db.PinAnchor = anchor
	buf, err := json.Marshal(db)
	if err != nil {
		log.TLogln("Error marshal torrent pin anchor", hash.HexString(), err)
		return false
	}
	tdb.Set("Torrents", hash.HexString(), buf)
	return true
}

// SetTorrentPinDownloaded updates only the downloaded file ids of an existing
// pinned row.
func SetTorrentPinDownloaded(hash metainfo.Hash, ids []int) bool {
	mu.Lock()
	defer mu.Unlock()
	db := getTorrentRow(hash)
	// an unpinned row keeps no downloaded ids, so a save racing pin off cannot
	// store them again
	if db == nil || db.PinMode == "" {
		return false
	}
	db.PinDownloaded = ids
	buf, err := json.Marshal(db)
	if err != nil {
		log.TLogln("Error marshal torrent pin downloaded", hash.HexString(), err)
		return false
	}
	tdb.Set("Torrents", hash.HexString(), buf)
	return true
}

// IsTorrentPinned reports whether the stored row has a pin of any mode.
func IsTorrentPinned(hash metainfo.Hash) bool {
	mu.Lock()
	defer mu.Unlock()
	db := getTorrentRow(hash)
	return db != nil && db.PinMode != ""
}

// getTorrentRow reads one stored row; the caller holds mu.
func getTorrentRow(hash metainfo.Hash) *TorrentDB {
	buf := tdb.Get("Torrents", hash.HexString())
	if len(buf) == 0 {
		return nil
	}
	var db *TorrentDB
	if err := json.Unmarshal(buf, &db); err != nil {
		return nil
	}
	return db
}

func ListTorrent() []*TorrentDB {
	// Use read lock to prevent migration during read
	dbMigrationLock.RLock()
	defer dbMigrationLock.RUnlock()

	mu.Lock()
	defer mu.Unlock()

	var list []*TorrentDB
	keys := tdb.List("Torrents")
	for _, key := range keys {
		buf := tdb.Get("Torrents", key)
		if len(buf) > 0 {
			var torr *TorrentDB
			err := json.Unmarshal(buf, &torr)
			if err == nil {
				list = append(list, torr)
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Timestamp > list[j].Timestamp
	})
	return list
}

func RemTorrent(hash metainfo.Hash) {
	mu.Lock()
	tdb.Rem("Torrents", hash.HexString())
	mu.Unlock()
}
