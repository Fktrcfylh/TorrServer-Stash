package torr

import (
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"server/log"
	sets "server/settings"
)

const resumePinnedInterval = time.Minute

// resume loop state; nil channels mean the loop is not running
var (
	resumeMu   sync.Mutex
	resumeStop chan struct{}
	resumeDone chan struct{}
)

// pinnedToResume returns the unfinished pinned DB torrents that are not loaded.
func pinnedToResume() []metainfo.Hash {
	if bts == nil || !sets.BTsets.UseDisk {
		return nil
	}
	var list []metainfo.Hash
	for _, db := range sets.ListTorrent() {
		if (db.PinMode == sets.PinModeAll || db.PinMode == sets.PinModeNext) && !pinFinished(db) && bts.GetTorrent(db.InfoHash) == nil {
			list = append(list, db.InfoHash)
		}
	}
	return list
}

// resumePinned loads the pinned torrents that are not in memory and applies
// the pin of the loaded ones again, which rechecks the free space.
func resumePinned() {
	for _, hash := range pinnedToResume() {
		hashHex := hash.HexString()
		log.TLogln("Resume pinned torrent:", hashHex)
		GetTorrent(hashHex)
	}
	if bts == nil {
		return
	}
	for _, t := range bts.ListTorrents() {
		t.muTorrent.Lock()
		pinned := t.PinMode == sets.PinModeAll || t.PinMode == sets.PinModeNext
		t.muTorrent.Unlock()
		if pinned {
			t.applyPin()
		}
	}
}

// StartResumePinned resumes pinned torrents now and then periodically until
// StopResumePinned.
func StartResumePinned() {
	resumeMu.Lock()
	defer resumeMu.Unlock()
	if resumeDone != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	resumeStop, resumeDone = stop, done
	go func() {
		defer close(done)
		resumePinned()
		ticker := time.NewTicker(resumePinnedInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				resumePinned()
			case <-stop:
				return
			}
		}
	}()
}

// StopResumePinned stops the resume loop and waits for it to exit.
func StopResumePinned() {
	resumeMu.Lock()
	defer resumeMu.Unlock()
	if resumeDone == nil {
		return
	}
	close(resumeStop)
	<-resumeDone
	resumeStop, resumeDone = nil, nil
}
