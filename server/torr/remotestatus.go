package torr

import (
	"context"
	"strings"
	"sync"
	"time"

	"server/log"
	"server/remotedl"
	sets "server/settings"
	"server/torr/state"
)

// Remote downloader states of the flagged torrents, for the status JSON.
// Status() only reads a cached snapshot: it never starts RPC, so the stream
// path and every other Status() caller stay RPC-free. The web API list/get
// handlers call RefreshRemoteStatus, which starts one background torrent-get
// for all flagged rows when the snapshot is stale. No loop: nothing refreshes
// while nobody lists a flagged torrent.
var (
	// remoteStatusTTL is the age after which a list starts a refresh; the
	// old snapshot is served meanwhile. Also the pause after a failure.
	remoteStatusTTL = 5 * time.Second
	// remoteStatusTimeout bounds one refresh (all its HTTP requests).
	remoteStatusTimeout = 15 * time.Second
)

type statusCache struct {
	mu       sync.Mutex
	cfg      sets.RemoteDownloaderConfig
	gen      uint64 // bumped on config change and invalidation; older refresh results are dropped
	running  uint64 // gen of the refresh in flight (0: none); one of an older gen does not block a new one
	statuses map[string]remotedl.RemoteStatus
	err      error // last refresh failed: every flagged torrent shows it
	next     time.Time
}

var remoteStatuses statusCache

// fetchRemoteStatus runs the batch status request. A var so tests replace
// the RPC; read and replaced only under remoteStatuses.mu.
var fetchRemoteStatus = func(ctx context.Context, cfg sets.RemoteDownloaderConfig, hashes []string) (map[string]remotedl.RemoteStatus, error) {
	d, err := remotedl.Shared(cfg)
	if err != nil {
		return nil, err
	}
	return d.Status(ctx, hashes)
}

// remoteConfig returns the saved downloader config, ok false when none.
func remoteConfig() (sets.RemoteDownloaderConfig, bool) {
	if sets.BTsets == nil {
		return sets.RemoteDownloaderConfig{}, false
	}
	cfg := sets.BTsets.RemoteDownloader
	return cfg, strings.TrimSpace(cfg.URL) != ""
}

// syncConfigLocked drops the snapshot of another config. c.mu is held.
func (c *statusCache) syncConfigLocked(cfg sets.RemoteDownloaderConfig) {
	if cfg != c.cfg {
		c.cfg, c.statuses, c.err, c.next = cfg, nil, nil, time.Time{}
		c.gen++
	}
}

// RefreshRemoteStatus starts one background refresh of the flagged torrents'
// remote states when the snapshot is stale or of another config. It never
// blocks; the result shows in later Status() reads.
func RefreshRemoteStatus() {
	cfg, ok := remoteConfig()
	if !ok {
		return
	}
	c := &remoteStatuses
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncConfigLocked(cfg)
	if c.running != c.gen && !time.Now().Before(c.next) {
		c.running = c.gen
		go c.refresh(cfg, c.gen, fetchRemoteStatus, remoteStatusTimeout)
	}
}

// cachedRemoteStatus returns the cached state of a flagged torrent, or the
// error of the last refresh; ok is false while nothing is known. It never
// starts a refresh.
func cachedRemoteStatus(hash string) (rs remotedl.RemoteStatus, rpcErr error, ok bool) {
	cfg, configured := remoteConfig()
	if !configured {
		return rs, nil, false
	}
	c := &remoteStatuses
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncConfigLocked(cfg)
	if c.err != nil {
		return rs, c.err, true
	}
	rs, ok = c.statuses[hash]
	return rs, nil, ok
}

func (c *statusCache) refresh(cfg sets.RemoteDownloaderConfig, gen uint64,
	fetch func(context.Context, sets.RemoteDownloaderConfig, []string) (map[string]remotedl.RemoteStatus, error),
	timeout time.Duration,
) {
	var hashes []string
	for _, row := range sets.ListTorrent() {
		if row.RemoteDownload {
			hashes = append(hashes, row.InfoHash.HexString())
		}
	}
	var (
		got map[string]remotedl.RemoteStatus
		err error
	)
	if len(hashes) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		got, err = fetch(ctx, cfg, hashes)
		cancel()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return // config changed or invalidated meanwhile
	}
	c.running = 0
	c.next = time.Now().Add(remoteStatusTTL)
	if err != nil {
		if c.err == nil {
			log.TLogln("remote status: torrent-get failed, retrying every", remoteStatusTTL, "while listed, err:", err)
		}
		c.statuses, c.err = nil, err
		return
	}
	if c.err != nil {
		log.TLogln("remote status: torrent-get works again, torrents:", len(hashes))
	}
	c.statuses, c.err = got, nil
}

// invalidateRemoteStatus makes the next RefreshRemoteStatus refresh, e.g. after a flag
// change; a refresh already running is dropped. The snapshot stays until then.
func invalidateRemoteStatus() {
	c := &remoteStatuses
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next = time.Time{}
	c.gen++
}

// setRemoteStatus fills the remote_* fields of a flagged torrent's status.
// remote_progress is set whenever the client reported the torrent, so 0 %
// still shows; it is absent for missing torrents and RPC errors.
func setRemoteStatus(st *state.TorrentStatus) {
	rs, rpcErr, ok := cachedRemoteStatus(st.Hash)
	if !ok {
		return
	}
	if rpcErr != nil {
		st.RemoteState, st.RemoteError = remotedl.StateError, rpcErr.Error()
		return
	}
	st.RemoteState, st.RemoteError = rs.State, rs.Error
	if rs.State != remotedl.StateMissing {
		p := rs.Progress
		st.RemoteProgress = &p
	}
}
