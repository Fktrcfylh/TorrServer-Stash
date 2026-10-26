package settings

import (
	"encoding/json"
	"sort"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"server/log"
)

type TorrentDB struct {
	*torrent.TorrentSpec

	Title    string `json:"title,omitempty"`
	Category string `json:"category,omitempty"`
	Poster   string `json:"poster,omitempty"`
	Data     string `json:"data,omitempty"`

	Timestamp int64 `json:"timestamp,omitempty"`
	Size      int64 `json:"size,omitempty"`

	// RemoteDownload marks a torrent added to the remote downloader (NAS).
	// The stored row owns it: only SetTorrentRemoteDownload changes it.
	RemoteDownload bool `json:"remote_download,omitempty"`
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
	// the remote flag is owned by the stored row: a general save never changes it
	if cur := getTorrentRow(torr.InfoHash); cur != nil {
		torr.RemoteDownload = cur.RemoteDownload
	}
	// only the saved row is written, so a stale snapshot never reverts other rows
	buf, err := json.Marshal(torr)
	if err == nil {
		tdb.Set("Torrents", torr.InfoHash.HexString(), buf)
	}
}

// SetTorrentRemoteDownload updates only the remote flag of an existing row.
// It returns false, writing nothing, when there is no row.
func SetTorrentRemoteDownload(hash metainfo.Hash, on bool) bool {
	mu.Lock()
	defer mu.Unlock()
	db := getTorrentRow(hash)
	if db == nil {
		return false
	}
	db.RemoteDownload = on
	buf, err := json.Marshal(db)
	if err != nil {
		log.TLogln("Error marshal torrent remote flag", hash.HexString(), err)
		return false
	}
	tdb.Set("Torrents", hash.HexString(), buf)
	return true
}

// getTorrentRow reads one stored row; the caller holds mu.
func getTorrentRow(hash metainfo.Hash) *TorrentDB {
	buf := tdb.Get("Torrents", hash.HexString())
	if len(buf) == 0 {
		return nil
	}
	var torr *TorrentDB
	if err := json.Unmarshal(buf, &torr); err != nil {
		return nil
	}
	return torr
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
