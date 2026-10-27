package torr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"

	"server/log"
	"server/remotedl"
	sets "server/settings"
)

// The NAS peer address of the remote downloader: host of its RPC URL plus the
// peer port from session-get. It is resolved in the background and cached, so
// adding it to a torrent never waits on RPC or DNS (stream path).
var (
	// remotePeerTTL is how long a resolved address is used before it is
	// refreshed (in the background; the old one is used meanwhile).
	remotePeerTTL = 10 * time.Minute
	// remotePeerRetry is the pause after a failed resolve.
	remotePeerRetry = time.Minute
	// remotePeerTimeout bounds one resolve (session-get and DNS).
	remotePeerTimeout = 15 * time.Second
	// remotePeerReadd is the re-add interval for a loaded flagged torrent:
	// the client does not queue a peer again after its connection closes.
	remotePeerReadd = 30 * time.Second
)

// peerCache holds the resolved address for one remote downloader config.
type peerCache struct {
	mu        sync.Mutex
	cfg       sets.RemoteDownloaderConfig
	gen       uint64 // bumped on config change; results of older resolves are dropped
	peer      torrent.Peer
	ok        bool
	next      time.Time // no resolve starts before this
	resolving bool
	failing   bool // last resolve failed: failures are logged once until a success
}

var remotePeers peerCache

// resolveRemotePeer asks the client for its peer port and resolves the host
// of its URL. A var so tests replace the RPC and DNS; read and replaced only
// under remotePeers.mu.
var resolveRemotePeer = func(ctx context.Context, cfg sets.RemoteDownloaderConfig) (torrent.Peer, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil {
		return torrent.Peer{}, errors.New("invalid remote downloader URL")
	}
	d, err := remotedl.Shared(cfg)
	if err != nil {
		return torrent.Peer{}, err
	}
	info, err := d.Session(ctx)
	if err != nil {
		return torrent.Peer{}, err
	}
	if info.PeerPort <= 0 || info.PeerPort > 65535 {
		return torrent.Peer{}, fmt.Errorf("client reports peer port %d", info.PeerPort)
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", u.Hostname())
	if err != nil {
		return torrent.Peer{}, err
	}
	ip := ips[0]
	for _, cand := range ips {
		if cand.To4() != nil {
			ip = cand
			break
		}
	}
	return torrent.Peer{IP: ip, Port: info.PeerPort}, nil
}

// addPeers hands peers to the BT client. A var so tests see the calls; read
// and replaced only under remotePeers.mu.
var addPeers = func(lt *torrent.Torrent, pp []torrent.Peer) {
	lt.AddPeers(pp)
}

// remotePeer returns the cached NAS peer for the saved settings and the
// function that adds it, and never blocks: a missing or stale entry starts
// one background resolve, and a changed config drops the old entry.
func remotePeer() (torrent.Peer, bool, func(*torrent.Torrent, []torrent.Peer)) {
	if sets.BTsets == nil {
		return torrent.Peer{}, false, nil
	}
	cfg := sets.BTsets.RemoteDownloader
	if strings.TrimSpace(cfg.URL) == "" {
		return torrent.Peer{}, false, nil
	}
	c := &remotePeers
	c.mu.Lock()
	defer c.mu.Unlock()
	if cfg != c.cfg {
		c.cfg, c.peer, c.ok, c.next, c.resolving, c.failing = cfg, torrent.Peer{}, false, time.Time{}, false, false
		c.gen++
	}
	if !c.resolving && !time.Now().Before(c.next) {
		c.resolving = true
		go c.resolve(cfg, c.gen, resolveRemotePeer)
	}
	return c.peer, c.ok, addPeers
}

func (c *peerCache) resolve(cfg sets.RemoteDownloaderConfig, gen uint64,
	resolveFn func(context.Context, sets.RemoteDownloaderConfig) (torrent.Peer, error),
) {
	ctx, cancel := context.WithTimeout(context.Background(), remotePeerTimeout)
	defer cancel()
	p, err := resolveFn(ctx, cfg)

	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return // config changed meanwhile
	}
	c.resolving = false
	if err != nil {
		c.next = time.Now().Add(remotePeerRetry)
		if !c.failing {
			log.TLogln("remote peer: resolve failed, retrying every", remotePeerRetry, "err:", err)
		}
		c.failing = true
		return
	}
	if !c.ok || c.failing || !c.peer.IP.Equal(p.IP) || c.peer.Port != p.Port {
		log.TLogln("remote peer:", net.JoinHostPort(p.IP.String(), fmt.Sprint(p.Port)))
	}
	c.peer, c.ok, c.next, c.failing = p, true, time.Now().Add(remotePeerTTL), false
}

// addRemotePeer adds the NAS peer to a flagged torrent when the address is
// cached; it reports whether it did. Unflagged torrents never get it.
func (t *Torrent) addRemotePeer() bool {
	t.muTorrent.Lock()
	lt, on := t.Torrent, t.RemoteDownload
	t.muTorrent.Unlock()
	if !on || lt == nil {
		return false
	}
	p, ok, add := remotePeer()
	if !ok {
		return false
	}
	add(lt, []torrent.Peer{p})
	return true
}

// readdRemotePeer runs addRemotePeer at most once per remotePeerReadd for a
// torrent; until the address is cached it retries on every call.
func (t *Torrent) readdRemotePeer() {
	t.muTorrent.Lock()
	last := t.remotePeerAt
	due := t.RemoteDownload && time.Since(last) >= remotePeerReadd
	if due {
		t.remotePeerAt = time.Now() // claimed: overlapping progress events skip
	}
	t.muTorrent.Unlock()
	if due && !t.addRemotePeer() {
		t.muTorrent.Lock()
		t.remotePeerAt = last
		t.muTorrent.Unlock()
	}
}
