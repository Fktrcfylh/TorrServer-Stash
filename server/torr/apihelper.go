package torr

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"server/log"
	sets "server/settings"
	"server/torr/utils"
)

var bts *BTServer

func InitApiHelper(bt *BTServer) {
	bts = bt
}

func LoadTorrent(tor *Torrent) *Torrent {
	if tor.TorrentSpec == nil {
		return nil
	}
	tr, err := NewTorrent(tor.TorrentSpec, bts)
	if err != nil {
		return nil
	}
	if !tr.WaitInfo() {
		return nil
	}
	tr.Title = tor.Title
	tr.Poster = tor.Poster
	tr.Data = tor.Data
	copyPin(tr, tor)
	return tr
}

func AddTorrent(spec *torrent.TorrentSpec, title, poster string, data string, category string) (*Torrent, error) {
	torr, err := NewTorrent(spec, bts)
	if err != nil {
		log.TLogln("error add torrent:", err)
		return nil, err
	}

	torDB := GetTorrentDB(spec.InfoHash)

	if torr.Title == "" {
		torr.Title = title
		if title == "" && torDB != nil {
			torr.Title = torDB.Title
		}
		if torr.Title == "" && torr.Torrent != nil && torr.Torrent.Info() != nil {
			torr.Title = torr.Info().Name
		}
	}

	if torr.Category == "" {
		torr.Category = category
		if torr.Category == "" && torDB != nil {
			torr.Category = torDB.Category
		}
	}

	if torr.Poster == "" {
		torr.Poster = poster
		if torr.Poster == "" && torDB != nil {
			torr.Poster = torDB.Poster
		}
	}

	if torr.Data == "" {
		torr.Data = data
		if torr.Data == "" && torDB != nil {
			torr.Data = torDB.Data
		}
	}

	// the DB row owns the pin, the in-memory torrent mirrors it
	if torDB != nil {
		copyPin(torr, torDB)
	}

	return torr, nil
}

// copyPin copies the pin state from src to dst.
func copyPin(dst, src *Torrent) {
	dst.muTorrent.Lock()
	dst.PinMode = src.PinMode
	dst.PinNext = src.PinNext
	dst.PinAnchor = src.PinAnchor
	dst.PinDownloaded = slices.Clone(src.PinDownloaded)
	dst.muTorrent.Unlock()
	dst.applyPin()
}

func SaveTorrentToDB(torr *Torrent) {
	log.TLogln("save to db:", torr.Hash())
	AddTorrentDB(torr)
}

func GetTorrent(hashHex string) *Torrent {
	hash := metainfo.NewHashFromHex(hashHex)
	timeout := time.Second * time.Duration(sets.BTsets.TorrentDisconnectTimeout)
	if timeout > time.Minute {
		timeout = time.Minute
	}
	tor := bts.GetTorrent(hash)
	if tor != nil {
		tor.AddExpiredTime(timeout)
		return tor
	}

	tr := GetTorrentDB(hash)
	if tr != nil {
		tor = tr
		go func() {
			log.TLogln("New torrent", tor.Hash())
			tr, _ := NewTorrent(tor.TorrentSpec, bts)
			if tr != nil {
				tr.Title = tor.Title
				tr.Poster = tor.Poster
				tr.Data = tor.Data
				tr.Size = tor.Size
				tr.Timestamp = tor.Timestamp
				tr.Category = tor.Category
				copyPin(tr, tor)
				tr.GotInfo()
			}
		}()
	}
	return tor
}

func SetTorrent(hashHex, title, poster, category string, data string) *Torrent {
	hash := metainfo.NewHashFromHex(hashHex)
	torr := bts.GetTorrent(hash)
	torrDb := GetTorrentDB(hash)

	if title == "" && torr == nil && torrDb != nil {
		torr = GetTorrent(hashHex)
		torr.GotInfo()
		if torr.Torrent != nil && torr.Torrent.Info() != nil {
			title = torr.Info().Name
		}
	}

	if torr != nil {
		if title == "" && torr.Torrent != nil && torr.Torrent.Info() != nil {
			title = torr.Info().Name
		}
		torr.Title = title
		torr.Poster = poster
		torr.Category = category
		if data != "" {
			torr.Data = data
		}
	}
	// update torrent data in DB
	if torrDb != nil {
		torrDb.Title = title
		torrDb.Poster = poster
		torrDb.Category = category
		if data != "" {
			torrDb.Data = data
		}
		AddTorrentDB(torrDb)
	}
	if torr != nil {
		return torr
	} else {
		return torrDb
	}
}

// SetTorrentPin sets the pin of a loaded or DB torrent without loading it.
// Off is stored as an empty mode with N 0.
func SetTorrentPin(hashHex, mode string, next int) *Torrent {
	if sets.ReadOnly {
		log.TLogln("API SetTorrentPin: Read-only DB mode!", hashHex)
		return nil
	}
	log.TLogln("set torrent pin:", hashHex, "mode:", mode, "next:", next)
	if mode == sets.PinModeOff {
		mode = ""
		next = 0
	}
	hash := metainfo.NewHashFromHex(hashHex)
	torr := bts.GetTorrent(hash)
	// off deletes data only of a torrent that was pinned before
	wasPinned := sets.IsTorrentPinned(hash)
	saved := sets.SetTorrentPin(hash, mode, next)
	if mode == "" {
		pinNoSpaceSet(hash, false)
	}
	if torr == nil {
		if !saved {
			return nil
		}
		if mode == "" && wasPinned {
			removeTorrentDir(hashHex)
		}
		return GetTorrentDB(hash)
	}
	torr.muTorrent.Lock()
	wasPinned = wasPinned || torr.PinMode != ""
	torr.PinMode = mode
	torr.PinNext = next
	if mode == "" {
		torr.PinDownloaded = nil
	}
	torr.muTorrent.Unlock()
	torr.applyPin()
	if !saved {
		// pinning implies persistence, so a loaded torrent is saved to DB;
		// the pin is set again in case a concurrent save created the row first
		AddTorrentDB(torr)
		sets.SetTorrentPin(hash, mode, next)
	}
	// a loaded cache never recreates its dir, so the data is removed after close
	if mode == "" && wasPinned && torr.closed != nil && sets.BTsets.UseDisk && hashHex != "" && hashHex != "/" {
		go removeTorrentDirAfterClose(torr, hash, filepath.Join(sets.BTsets.TorrentsSavePath, hashHex))
	}
	return torr
}

// removeTorrentDir removes the disk cache dir of a torrent; no loaded cache
// may use it.
func removeTorrentDir(hashHex string) {
	if sets.BTsets.UseDisk && hashHex != "" && hashHex != "/" {
		name := filepath.Join(sets.BTsets.TorrentsSavePath, hashHex)
		if _, err := os.Stat(name); err == nil {
			log.TLogln("Removing cache files for:", hashHex)
			os.RemoveAll(name)
		}
	}
}

// removeTorrentDirAfterClose removes the dir name once the torrent is closed,
// unless it is loaded or pinned again by then.
func removeTorrentDirAfterClose(torr *Torrent, hash metainfo.Hash, name string) {
	<-torr.closed
	// drop() holds muTorrent while the storage is being closed; a pin set on
	// the loaded torrent after off is kept even when the DB is already closed
	torr.muTorrent.Lock()
	repinned := torr.PinMode != ""
	torr.muTorrent.Unlock()
	if repinned {
		return
	}
	// drop() and client.Close leave the closed torrent in the map, so only
	// another torrent under the same hash means it was loaded again
	if torr.bt != nil {
		if cur := torr.bt.GetTorrent(hash); cur != nil && cur != torr {
			return
		}
	}
	if sets.IsTorrentPinned(hash) {
		return
	}
	log.TLogln("Removing cache files for:", hash.HexString())
	os.RemoveAll(name)
}

func RemTorrent(hashHex string) {
	if sets.ReadOnly {
		log.TLogln("API RemTorrent: Read-only DB mode!", hashHex)
		return
	}
	hash := metainfo.NewHashFromHex(hashHex)
	pinNoSpaceSet(hash, false)

	// Download the torrent before deleting it to get the "closed" status
	torr := bts.GetTorrent(hash)
	if torr == nil {
		// If the torrent isn't in memory, just delete it from the database and the files
		RemTorrentDB(hash)
		removeTorrentDir(hashHex)
		return
	}

	closedChan := torr.closed

	// Delete from the database first, so a pinned-torrent resume cannot load
	// it again while it is being removed
	RemTorrentDB(hash)

	// Clear from memory
	if bts.RemoveTorrent(hash) {
		// Waiting for confirmation from the library via the closed channel
		select {
		case <-closedChan:
			// The library has confirmed the closure
			log.TLogln("Torrent closed by library:", hashHex)
		case <-time.After(5 * time.Second):
			log.TLogln("Warning: timeout waiting for torrent close:", hashHex)
		}

		// Now we can safely delete the files from the disk
		removeTorrentDir(hashHex)
	}
}

func ListTorrent() []*Torrent {
	btlist := bts.ListTorrents()
	dblist := ListTorrentsDB()

	for hash, t := range dblist {
		if _, ok := btlist[hash]; !ok {
			btlist[hash] = t
		}
	}
	var ret []*Torrent

	for _, t := range btlist {
		ret = append(ret, t)
	}

	sort.Slice(ret, func(i, j int) bool {
		if ret[i].Timestamp != ret[j].Timestamp {
			return ret[i].Timestamp > ret[j].Timestamp
		} else {
			return ret[i].Title > ret[j].Title
		}
	})

	return ret
}

func DropTorrent(hashHex string) {
	hash := metainfo.NewHashFromHex(hashHex)
	bts.RemoveTorrent(hash)
}

func SetSettings(set *sets.BTSets) {
	if sets.ReadOnly {
		log.TLogln("API SetSettings: Read-only DB mode!")
		return
	}
	sets.SetBTSets(set)
	utils.InvalidateTrackersCache()
	log.TLogln("drop all torrents")
	dropAllTorrent()
	time.Sleep(time.Second * 1)
	log.TLogln("disconect")
	bts.Disconnect()
	log.TLogln("connect")
	bts.Connect()
	resumePinned()
	time.Sleep(time.Second * 1)
	log.TLogln("end set settings")
}

func SetDefSettings() {
	if sets.ReadOnly {
		log.TLogln("API SetDefSettings: Read-only DB mode!")
		return
	}
	sets.SetDefaultConfig()
	utils.InvalidateTrackersCache()
	log.TLogln("drop all torrents")
	dropAllTorrent()
	time.Sleep(time.Second * 1)
	log.TLogln("disconect")
	bts.Disconnect()
	log.TLogln("connect")
	bts.Connect()
	resumePinned()
	time.Sleep(time.Second * 1)
	log.TLogln("end set default settings")
}

func dropAllTorrent() {
	for _, torr := range bts.ListTorrents() {
		torr.drop()
		<-torr.closed
	}
}

func Shutdown() {
	if sets.Embedded {
		log.TLogln("Received shutdown (embedded)")
		if sets.EmbeddedStop != nil {
			sets.EmbeddedStop()
		}
		return
	}
	bts.Disconnect()
	sets.CloseDB()
	log.TLogln("Received shutdown. Quit")
	os.Exit(0)
}

func WriteStatus(w io.Writer) {
	bts.client.WriteStatus(w)
}

func Preload(torr *Torrent, index int) {
	cache := float32(sets.BTsets.CacheSize)
	preload := float32(sets.BTsets.PreloadCache)
	size := int64((cache / 100.0) * preload)
	if size <= 0 {
		return
	}
	if size > sets.BTsets.CacheSize {
		size = sets.BTsets.CacheSize
	}
	torr.Preload(index, size)
}
