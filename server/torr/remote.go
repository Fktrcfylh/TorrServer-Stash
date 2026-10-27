package torr

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"server/log"
	"server/remotedl"
	sets "server/settings"
	"server/torr/utils"
)

var (
	// ErrReadOnly is returned for writes in read-only DB mode.
	ErrReadOnly = errors.New("read-only DB mode")
	// ErrTorrentNotFound is returned for a hash that is neither loaded nor in the DB.
	ErrTorrentNotFound = errors.New("torrent not found")
	// ErrInfoPending is returned for a loaded torrent without a DB row whose
	// info has not arrived yet: it cannot be saved to the DB before that.
	ErrInfoPending = errors.New("torrent info is not loaded yet, try again later")
	// ErrRemoteConfig wraps errors of the remote downloader configuration
	// (not configured, invalid URL or type).
	ErrRemoteConfig = errors.New("remote downloader settings")
)

// remoteTimeout bounds one remote write (all its HTTP requests together).
var remoteTimeout = 30 * time.Second

// remoteLocks is a fixed table of striped mutexes keyed by info hash. The
// lock of a hash serialises the remote downloader writes and RemTorrent of
// that torrent, so the client and the DB flag never diverge through
// interleaved on/off/rem calls; other torrents wait on its RPC only when they
// share the stripe (1 in 64). Fixed size: request-supplied hashes never grow
// it. Lock order: hash lock before bt.mu / muTorrent.
var remoteLocks [64]sync.Mutex

func lockRemote(hash metainfo.Hash) func() {
	m := &remoteLocks[hash[0]%byte(len(remoteLocks))]
	m.Lock()
	return m.Unlock
}

func (t *Torrent) remoteDownload() bool {
	t.muTorrent.Lock()
	defer t.muTorrent.Unlock()
	return t.RemoteDownload
}

func (t *Torrent) hasInfo() bool {
	t.muTorrent.Lock()
	lt := t.Torrent
	t.muTorrent.Unlock()
	return lt != nil && lt.Info() != nil
}

func (t *Torrent) setRemoteDownload(on bool) {
	t.muTorrent.Lock()
	t.RemoteDownload = on
	t.muTorrent.Unlock()
}

// remoteDownloader returns the shared client for the saved settings.
func remoteDownloader() (remotedl.Downloader, error) {
	d, err := remotedl.Shared(sets.BTsets.RemoteDownloader)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRemoteConfig, err)
	}
	return d, nil
}

// SetTorrentRemote adds the torrent to the remote downloader (on) or removes
// it there with its data (off), and saves the flag only when that succeeded.
// A loaded torrent without a DB row is saved first: the flag lives in the row.
func SetTorrentRemote(hashHex string, on bool) (*Torrent, error) {
	if sets.ReadOnly {
		log.TLogln("API SetTorrentRemote: Read-only DB mode!", hashHex)
		return nil, ErrReadOnly
	}
	var hash metainfo.Hash
	if err := hash.FromHexString(hashHex); err != nil {
		return nil, fmt.Errorf("%w: invalid hash %q", ErrTorrentNotFound, hashHex)
	}
	hashHex = hash.HexString()

	defer lockRemote(hash)()

	loaded := bts.GetTorrent(hash)
	row := GetTorrentDB(hash)
	if row == nil {
		if loaded == nil {
			return nil, ErrTorrentNotFound
		}
		// AddTorrentDB needs the info (size, files)
		if !loaded.hasInfo() {
			log.TLogln("remote", onOff(on), "refused, info not loaded yet, hash:", hashHex)
			return nil, ErrInfoPending
		}
		AddTorrentDB(loaded)
		if row = GetTorrentDB(hash); row == nil {
			return nil, fmt.Errorf("save torrent %s to DB failed", hashHex)
		}
	}

	if !on && !row.RemoteDownload {
		// never touch a client torrent that is not flagged here
		log.TLogln("remote off: not flagged, nothing to remove, hash:", hashHex)
		return remoteResult(loaded, row), nil
	}

	d, err := remoteDownloader()
	if err != nil {
		log.TLogln("remote", onOff(on), "failed, hash:", hashHex, "err:", err)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer cancel()

	if on {
		req, mode, err := remoteAddRequest(hash, row, loaded)
		if err != nil {
			log.TLogln("remote on: build request failed, hash:", hashHex, "err:", err)
			return nil, err
		}
		req.DownloadDir = sets.BTsets.RemoteDownloader.DownloadDir
		res, err := d.Add(ctx, req)
		if err != nil {
			log.TLogln("remote on failed, hash:", hashHex, "mode:", mode, "err:", err)
			return nil, err
		}
		log.TLogln("remote on: added, hash:", hashHex, "mode:", mode, "duplicate:", res.Duplicate, "client hash:", res.Hash, "name:", res.Name)
		if res.Hash != "" && res.Hash != hashHex {
			log.TLogln("remote on: warning: client reports another hash, hash:", hashHex, "client hash:", res.Hash)
		}
	} else {
		if err := d.Remove(ctx, hashHex); err != nil {
			log.TLogln("remote off failed, hash:", hashHex, "err:", err)
			return nil, err
		}
		log.TLogln("remote off: removed with data, hash:", hashHex)
	}

	if !sets.SetTorrentRemoteDownload(hash, on) {
		return nil, fmt.Errorf("save remote flag of %s failed", hashHex)
	}
	row.RemoteDownload = on
	if on {
		invalidateRemoteStatus() // show the new torrent's state on the next list
	}
	if loaded != nil {
		loaded.setRemoteDownload(on)
		if on {
			loaded.addRemotePeer()
		}
	}
	return remoteResult(loaded, row), nil
}

func remoteResult(loaded, row *Torrent) *Torrent {
	if loaded != nil {
		return loaded
	}
	return row
}

// removeRemote removes a flagged torrent from the remote downloader; an
// unflagged or unknown one is left alone. The caller holds lockRemote(hash).
func removeRemote(hash metainfo.Hash) error {
	row := GetTorrentDB(hash)
	if row == nil || !row.RemoteDownload {
		return nil
	}
	hashHex := hash.HexString()
	d, err := remoteDownloader()
	if err != nil {
		log.TLogln("RemTorrent: remote remove failed, torrent kept, hash:", hashHex, "err:", err)
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout)
	defer cancel()
	if err := d.Remove(ctx, hashHex); err != nil {
		log.TLogln("RemTorrent: remote remove failed, torrent kept, hash:", hashHex, "err:", err)
		return err
	}
	log.TLogln("RemTorrent: removed from remote downloader with data, hash:", hashHex)
	return nil
}

// remoteAddRequest builds the torrent-add payload: the .torrent from the
// info bytes (row, else the loaded torrent), or a magnet when they are not
// known yet. TorrServer's retrackers are left out.
func remoteAddRequest(hash metainfo.Hash, row, loaded *Torrent) (remotedl.AddRequest, string, error) {
	spec := row.TorrentSpec
	var infoBytes []byte
	if spec != nil {
		infoBytes = spec.InfoBytes
	}
	if len(infoBytes) == 0 && loaded != nil && loaded.hasInfo() {
		loaded.muTorrent.Lock()
		lt := loaded.Torrent
		loaded.muTorrent.Unlock()
		if lt != nil {
			infoBytes = lt.Metainfo().InfoBytes
		}
	}
	var trackers [][]string
	if spec != nil {
		trackers = originalTrackers(spec.Trackers)
	}

	if len(infoBytes) > 0 {
		// a row whose info does not match its hash would add another torrent
		// to the client than the one flagged here
		if metainfo.HashBytes(infoBytes) != hash {
			return remotedl.AddRequest{}, "", fmt.Errorf("stored info of %s does not match its hash", hash.HexString())
		}
		mi := metainfo.MetaInfo{InfoBytes: infoBytes, AnnounceList: trackers}
		if len(trackers) > 0 {
			mi.Announce = trackers[0][0]
		}
		buf, err := bencode.Marshal(mi)
		if err != nil {
			return remotedl.AddRequest{}, "", fmt.Errorf("encode .torrent: %w", err)
		}
		return remotedl.AddRequest{Metainfo: buf}, "metainfo", nil
	}

	m := metainfo.Magnet{InfoHash: hash}
	if spec != nil {
		m.DisplayName = spec.DisplayName
	}
	if m.DisplayName == "" {
		m.DisplayName = row.Title
	}
	for _, tier := range trackers {
		m.Trackers = append(m.Trackers, tier...)
	}
	return remotedl.AddRequest{Magnet: m.String()}, "magnet", nil
}

// retrackers lists the trackers NewTorrent appends: the default list and
// trackers.txt. A var so tests do not depend on the async remote list.
var retrackers = func() []string {
	return append(utils.GetDefTrackers(), utils.GetTrackerFromFile()...)
}

// originalTrackers drops the retrackers, duplicates and empty tiers.
func originalTrackers(tiers [][]string) [][]string {
	added := map[string]bool{}
	for _, tr := range retrackers() {
		added[tr] = true
	}
	seen := map[string]bool{}
	var out [][]string
	for _, tier := range tiers {
		var kept []string
		for _, tr := range tier {
			if tr == "" || added[tr] || seen[tr] {
				continue
			}
			seen[tr] = true
			kept = append(kept, tr)
		}
		if len(kept) > 0 {
			out = append(out, kept)
		}
	}
	return out
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
