package torr

import (
	"encoding/json"

	"server/settings"
	"server/torr/state"
	"server/torr/utils"

	"github.com/anacrolix/torrent/metainfo"
)

type tsFiles struct {
	TorrServer struct {
		Files []*state.TorrentFileStat `json:"Files"`
	} `json:"TorrServer"`
}

func AddTorrentDB(torr *Torrent) {
	t := new(settings.TorrentDB)
	t.TorrentSpec = torr.TorrentSpec
	t.Title = torr.Title
	t.Category = torr.Category
	if torr.Data == "" {
		files := new(tsFiles)
		// only the layout is stored, not the live pin state of the files
		for _, fs := range torr.Status().FileStats {
			files.TorrServer.Files = append(files.TorrServer.Files, &state.TorrentFileStat{Id: fs.Id, Path: fs.Path, Length: fs.Length})
		}
		buf, err := json.Marshal(files)
		if err == nil {
			t.Data = string(buf)
			torr.Data = t.Data
		}
	} else {
		t.Data = torr.Data
	}

	if torr.Poster != "" && utils.CheckImgUrl(torr.Poster) {
		t.Poster = torr.Poster
	}
	t.Size = torr.Size
	if t.Size == 0 && torr.Torrent != nil {
		t.Size = torr.Length()
	}
	// don't override timestamp from DB on edit
	t.Timestamp = torr.Timestamp // time.Now().Unix()
	// used only for a new row; an existing row keeps its pin (settings.AddTorrent)
	torr.muTorrent.Lock()
	t.PinMode = torr.PinMode
	t.PinNext = torr.PinNext
	t.PinAnchor = torr.PinAnchor
	t.PinDownloaded = torr.PinDownloaded
	torr.muTorrent.Unlock()

	settings.AddTorrent(t)
}

func GetTorrentDB(hash metainfo.Hash) *Torrent {
	list := settings.ListTorrent()
	for _, db := range list {
		if hash == db.InfoHash {
			torr := new(Torrent)
			torr.TorrentSpec = db.TorrentSpec
			torr.Title = db.Title
			torr.Poster = db.Poster
			torr.Category = db.Category
			torr.Timestamp = db.Timestamp
			torr.Size = db.Size
			torr.Data = db.Data
			torr.PinMode = db.PinMode
			torr.PinNext = db.PinNext
			torr.PinAnchor = db.PinAnchor
			torr.PinDownloaded = db.PinDownloaded
			torr.Stat = state.TorrentInDB
			return torr
		}
	}
	return nil
}

func RemTorrentDB(hash metainfo.Hash) {
	settings.RemTorrent(hash)
}

func ListTorrentsDB() map[metainfo.Hash]*Torrent {
	ret := make(map[metainfo.Hash]*Torrent)
	list := settings.ListTorrent()
	for _, db := range list {
		torr := new(Torrent)
		torr.TorrentSpec = db.TorrentSpec
		torr.Title = db.Title
		torr.Poster = db.Poster
		torr.Category = db.Category
		torr.Timestamp = db.Timestamp
		torr.Size = db.Size
		torr.Data = db.Data
		torr.PinMode = db.PinMode
		torr.PinNext = db.PinNext
		torr.PinAnchor = db.PinAnchor
		torr.PinDownloaded = db.PinDownloaded
		torr.Stat = state.TorrentInDB
		ret[torr.TorrentSpec.InfoHash] = torr
	}
	return ret
}
