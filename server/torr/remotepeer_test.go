package torr

import (
	"bytes"
	"context"
	"errors"
	"io"
	stdlog "log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"

	"server/settings"
)

var nasPeer = torrent.Peer{IP: net.IPv4(192, 168, 0, 111), Port: 51413}

// peerSpy records what addPeers received.
type peerSpy struct {
	mu    sync.Mutex
	calls [][]torrent.Peer
	tors  []*torrent.Torrent
}

func (s *peerSpy) add(lt *torrent.Torrent, pp []torrent.Peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, pp)
	s.tors = append(s.tors, lt)
}

func (s *peerSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *peerSpy) countFor(lt *torrent.Torrent) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, x := range s.tors {
		if x == lt {
			n++
		}
	}
	return n
}

// lockedPeerSeams replaces the peer seams and timings under remotePeers.mu
// (production reads them under it) and clears the cache; restored on cleanup.
func lockedPeerSeams(t *testing.T,
	resolve func(context.Context, settings.RemoteDownloaderConfig) (torrent.Peer, error),
	add func(*torrent.Torrent, []torrent.Peer),
) {
	t.Helper()
	c := &remotePeers
	c.mu.Lock()
	oldResolve, oldAdd, oldTTL, oldRetry := resolveRemotePeer, addPeers, remotePeerTTL, remotePeerRetry
	if resolve != nil {
		resolveRemotePeer = resolve
	}
	if add != nil {
		addPeers = add
	}
	resetPeerCacheLocked()
	c.mu.Unlock()
	t.Cleanup(func() {
		c.mu.Lock()
		resolveRemotePeer, addPeers, remotePeerTTL, remotePeerRetry = oldResolve, oldAdd, oldTTL, oldRetry
		resetPeerCacheLocked()
		c.mu.Unlock()
	})
}

func resetPeerCacheLocked() {
	c := &remotePeers
	c.cfg, c.peer, c.ok, c.next, c.resolving, c.failing = settings.RemoteDownloaderConfig{}, torrent.Peer{}, false, time.Time{}, false, false
	c.gen++ // drop results of resolves still running from earlier tests
}

func resolving() bool {
	remotePeers.mu.Lock()
	defer remotePeers.mu.Unlock()
	return remotePeers.resolving
}

// captureLog redirects the std logger (log.TLogln) for one test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	b := &syncBuffer{}
	stdlog.SetOutput(b)
	t.Cleanup(func() { stdlog.SetOutput(os.Stderr) })
	return b
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func setPeerTimings(ttl, retry time.Duration) {
	remotePeers.mu.Lock()
	remotePeerTTL, remotePeerRetry = ttl, retry
	remotePeers.mu.Unlock()
}

// countingResolver returns peer (or err) and counts calls per URL.
type countingResolver struct {
	mu    sync.Mutex
	calls map[string]int
	peer  torrent.Peer
	err   error
}

func (r *countingResolver) resolve(_ context.Context, cfg settings.RemoteDownloaderConfig) (torrent.Peer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[cfg.URL]++
	return r.peer, r.err
}

func (r *countingResolver) count(u string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[u]
}

func withRemoteURL(u string) {
	settings.BTsets.RemoteDownloader = settings.RemoteDownloaderConfig{Type: settings.DefaultRemoteDownloaderType, URL: u}
}

func waitPeer(t *testing.T) torrent.Peer {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, ok, _ := remotePeer(); ok {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatal("peer address never cached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRemotePeerNotConfiguredResolvesNothing(t *testing.T) {
	withTorrDB(t)
	r := &countingResolver{peer: nasPeer}
	lockedPeerSeams(t, r.resolve, nil)

	if _, ok, _ := remotePeer(); ok {
		t.Fatal("peer without a configured downloader")
	}
	time.Sleep(20 * time.Millisecond)
	if n := r.count(""); n != 0 {
		t.Fatalf("resolve ran %d times without a config", n)
	}
}

func TestRemotePeerResolvesInBackgroundAndCaches(t *testing.T) {
	withTorrDB(t)
	r := &countingResolver{peer: nasPeer}
	lockedPeerSeams(t, r.resolve, nil)
	withRemoteURL("http://nas.example:9091/transmission/rpc")

	if _, ok, _ := remotePeer(); ok {
		t.Fatal("first call must not wait for the resolve")
	}
	p := waitPeer(t)
	if !p.IP.Equal(nasPeer.IP) || p.Port != nasPeer.Port {
		t.Fatalf("peer = %v:%d", p.IP, p.Port)
	}
	for range 10 {
		remotePeer()
	}
	time.Sleep(20 * time.Millisecond)
	if n := r.count("http://nas.example:9091/transmission/rpc"); n != 1 {
		t.Fatalf("resolve ran %d times, want 1 (cached)", n)
	}
}

func TestRemotePeerConfigChangeInvalidatesAndDropsStaleResult(t *testing.T) {
	withTorrDB(t)
	oldURL, newURL := "http://old.example/transmission/rpc", "http://new.example/transmission/rpc"
	release := make(chan struct{})
	var oldStarted atomic.Bool
	resolve := func(_ context.Context, cfg settings.RemoteDownloaderConfig) (torrent.Peer, error) {
		if cfg.URL == oldURL {
			oldStarted.Store(true)
			<-release // the old resolve finishes after the config changed
			return torrent.Peer{IP: net.IPv4(10, 0, 0, 1), Port: 1}, nil
		}
		return nasPeer, nil
	}
	lockedPeerSeams(t, resolve, nil)
	withRemoteURL(oldURL)
	remotePeer()
	waitFor(t, "old resolve", oldStarted.Load)

	withRemoteURL(newURL)
	if _, ok, _ := remotePeer(); ok {
		t.Fatal("changed config served an address")
	}
	p := waitPeer(t)
	close(release)
	time.Sleep(20 * time.Millisecond)
	if p, _, _ = remotePeer(); !p.IP.Equal(nasPeer.IP) {
		t.Fatalf("stale resolve of the old config overwrote the cache: %v", p.IP)
	}
}

func TestRemotePeerRetriesAfterFailureNotBefore(t *testing.T) {
	withTorrDB(t)
	logs := captureLog(t)
	r := &countingResolver{err: errors.New("unreachable")}
	lockedPeerSeams(t, r.resolve, nil)
	setPeerTimings(time.Hour, 100*time.Millisecond)
	u := "http://nas.example/transmission/rpc"
	withRemoteURL(u)

	remotePeer()
	waitFor(t, "first resolve", func() bool { return r.count(u) == 1 && !resolving() })
	for range 10 {
		if _, ok, _ := remotePeer(); ok {
			t.Fatal("failed resolve cached a peer")
		}
	}
	time.Sleep(20 * time.Millisecond) // a wrongly started resolve would have run
	if n := r.count(u); n != 1 {
		t.Fatalf("resolve ran %d times inside the retry pause", n)
	}
	time.Sleep(120 * time.Millisecond)
	remotePeer()
	waitFor(t, "retry after the pause", func() bool { return r.count(u) == 2 && !resolving() })
	if n := strings.Count(logs.String(), "remote peer: resolve failed"); n != 1 {
		t.Fatalf("failure logged %d times, want once until a success", n)
	}
}

func TestRemotePeerRefreshAfterTTLKeepsServingOldAddress(t *testing.T) {
	withTorrDB(t)
	r := &countingResolver{peer: nasPeer}
	lockedPeerSeams(t, r.resolve, nil)
	setPeerTimings(50*time.Millisecond, time.Hour)
	u := "http://nas.example/transmission/rpc"
	withRemoteURL(u)
	waitPeer(t)

	time.Sleep(60 * time.Millisecond)
	r.mu.Lock()
	r.err = errors.New("down") // refresh fails: the old address stays
	r.mu.Unlock()
	if _, ok, _ := remotePeer(); !ok {
		t.Fatal("stale address not served while refreshing")
	}
	waitFor(t, "refresh", func() bool { return r.count(u) == 2 })
	if _, ok, _ := remotePeer(); !ok {
		t.Fatal("failed refresh dropped the known address")
	}
}

// The real resolver: host of the URL plus session-get's peer-port.
func TestResolveRemotePeerUsesURLHostAndSessionPeerPort(t *testing.T) {
	withTorrDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		io.WriteString(w, `{"result":"success","arguments":{"version":"2.82","rpc-version":15,"peer-port":51413,"lpd-enabled":true}}`)
	}))
	t.Cleanup(srv.Close)
	su, _ := url.Parse(srv.URL)
	for _, host := range []string{"127.0.0.1", "localhost"} {
		cfg := settings.RemoteDownloaderConfig{Type: settings.DefaultRemoteDownloaderType, URL: "http://" + net.JoinHostPort(host, su.Port()) + "/transmission/rpc"}
		p, err := resolveRemotePeer(context.Background(), cfg)
		if err != nil {
			t.Fatal(host, err)
		}
		// IPv4 preferred: localhost resolves to 127.0.0.1 and ::1
		if p.Port != 51413 || !p.IP.IsLoopback() || p.IP.To4() == nil {
			t.Fatalf("%s: peer = %v:%d, want 127.0.0.1:51413", host, p.IP, p.Port)
		}
	}
}

func TestResolveRemotePeerRejectsMissingPeerPort(t *testing.T) {
	withTorrDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		io.WriteString(w, `{"result":"success","arguments":{"version":"2.82","rpc-version":15}}`)
	}))
	t.Cleanup(srv.Close)
	cfg := settings.RemoteDownloaderConfig{Type: settings.DefaultRemoteDownloaderType, URL: srv.URL + "/transmission/rpc"}
	if _, err := resolveRemotePeer(context.Background(), cfg); err == nil {
		t.Fatal("peer port 0 accepted")
	}
}

// cachedPeer fills the cache with nasPeer for a configured downloader.
func cachedPeer(t *testing.T, spy *peerSpy) {
	t.Helper()
	r := &countingResolver{peer: nasPeer}
	lockedPeerSeams(t, r.resolve, spy.add)
	withRemoteURL("http://nas.example/transmission/rpc")
	waitPeer(t)
}

func TestReaddRemotePeerThrottled(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spy := &peerSpy{}
	cachedPeer(t, spy)
	goTorrent, _, err := bts.client.AddTorrentSpec(specWithInfo(t, "throttle"))
	if err != nil {
		t.Fatal(err)
	}
	tor := &Torrent{Torrent: goTorrent, RemoteDownload: true}

	for range 5 {
		tor.readdRemotePeer()
	}
	if n := spy.count(); n != 1 {
		t.Fatalf("adds = %d, want 1 inside the interval", n)
	}
	tor.muTorrent.Lock()
	tor.remotePeerAt = time.Now().Add(-remotePeerReadd)
	tor.muTorrent.Unlock()
	tor.readdRemotePeer()
	if n := spy.count(); n != 2 {
		t.Fatalf("adds = %d, want 2 after the interval", n)
	}
	if p := spy.calls[0]; len(p) != 1 || !p[0].IP.Equal(nasPeer.IP) || p[0].Port != nasPeer.Port {
		t.Fatalf("added %+v", p)
	}
}

func TestReaddRemotePeerRetriesUntilAddressCached(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spy := &peerSpy{}
	release := make(chan struct{})
	lockedPeerSeams(t, func(context.Context, settings.RemoteDownloaderConfig) (torrent.Peer, error) {
		<-release
		return nasPeer, nil
	}, spy.add)
	withRemoteURL("http://nas.example/transmission/rpc")
	goTorrent, _, err := bts.client.AddTorrentSpec(specWithInfo(t, "retry"))
	if err != nil {
		t.Fatal(err)
	}
	tor := &Torrent{Torrent: goTorrent, RemoteDownload: true}

	tor.readdRemotePeer() // starts the resolve, nothing to add yet
	if spy.count() != 0 {
		t.Fatal("added before the address was known")
	}
	close(release)
	waitPeer(t)
	tor.readdRemotePeer() // not throttled: the first try added nothing
	if n := spy.count(); n != 1 {
		t.Fatalf("adds = %d, want 1 once cached", n)
	}
}

func TestUnflaggedTorrentGetsNoRemotePeer(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spy := &peerSpy{}
	cachedPeer(t, spy)
	goTorrent, _, err := bts.client.AddTorrentSpec(specWithInfo(t, "unflagged"))
	if err != nil {
		t.Fatal(err)
	}
	tor := &Torrent{Torrent: goTorrent}

	tor.readdRemotePeer()
	if tor.addRemotePeer() || spy.count() != 0 {
		t.Fatal("unflagged torrent got the NAS peer")
	}
}

// progressEvent (the watch loop tick) adds the peer to a flagged torrent and
// never to an unflagged one. Called once per torrent without the loop:
// repeated ticks race upstream (WaitInfo/expired on t.cache, AdjustRA).
func TestProgressEventAddsPeerToFlaggedTorrentOnly(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spy := &peerSpy{}
	cachedPeer(t, spy)
	flagged := expiredTorrent(t, specWithInfo(t, "flagged-tick"))
	unflagged := expiredTorrent(t, specWithInfo(t, "unflagged-tick"))
	for _, tor := range []*Torrent{flagged, unflagged} {
		tor.expiredTime = time.Now().Add(time.Minute)
		tor.lastTimeSpeed = time.Now()
	}
	flagged.RemoteDownload = true

	flagged.progressEvent()
	unflagged.progressEvent()
	if n := spy.countFor(flagged.Torrent); n != 1 {
		t.Fatalf("flagged torrent got %d adds from one tick, want 1", n)
	}
	if n := spy.countFor(unflagged.Torrent); n != 0 {
		t.Fatalf("unflagged torrent got %d adds", n)
	}
}

func TestSetTorrentRemoteOnAddsPeerImmediately(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	withRemote(t)
	spy := &peerSpy{}
	r := &countingResolver{peer: nasPeer}
	lockedPeerSeams(t, r.resolve, spy.add)
	waitPeer(t)
	tor := loadedTorrent(t, "flag-on")
	tor.muTorrent.Lock()
	tor.remotePeerAt = time.Now() // the watch loop would be throttled now
	tor.muTorrent.Unlock()

	if _, err := SetTorrentRemote(tor.Hash().HexString(), true); err != nil {
		t.Fatal(err)
	}
	if n := spy.countFor(tor.Torrent); n != 1 {
		t.Fatalf("adds right after flag on = %d, want 1", n)
	}
}

// Stream path: a flagged DB torrent is served by GetTorrent at once even when
// the NAS RPC hangs, and its watch loop keeps running.
func TestGetTorrentDoesNotWaitForUnreachableRemote(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	release := make(chan struct{})
	var sessionCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		sessionCalls.Add(1)
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	lockedPeerSeams(t, nil, nil) // real resolver
	withRemoteURL(srv.URL + "/transmission/rpc")
	spec := specWithInfo(t, "stream-path")
	row := dbTorrent(spec.InfoHash)
	row.TorrentSpec = spec
	AddTorrentDB(row)
	settings.SetTorrentRemoteDownload(spec.InfoHash, true)

	start := time.Now()
	if GetTorrent(spec.InfoHash.HexString()) == nil {
		t.Fatal("GetTorrent returned nil")
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("GetTorrent took %v with the NAS hanging", d)
	}
	waitFor(t, "async load", func() bool {
		tor := bts.GetTorrent(spec.InfoHash)
		return tor != nil && tor.remoteDownload()
	})
	tor := bts.GetTorrent(spec.InfoHash)
	// what the watch loop does each tick; called directly so the test ends
	// before the first tick (upstream WaitInfo/expired race on t.cache)
	tickStart := time.Now()
	tor.readdRemotePeer()
	if d := time.Since(tickStart); d > 100*time.Millisecond {
		t.Fatalf("watch tick took %v with the NAS hanging", d)
	}
	waitFor(t, "session-get sent in the background", func() bool { return sessionCalls.Load() > 0 })
	again := time.Now()
	if GetTorrent(spec.InfoHash.HexString()) != tor || time.Since(again) > 100*time.Millisecond {
		t.Fatal("loaded torrent not served at once while the resolve hangs")
	}
}

// The real addPeers makes the BT client dial the NAS address: a torrent that
// still needs its info connects to the listener and sends its info hash.
func TestAddRemotePeerDialsNASAddress(t *testing.T) {
	withTorrDB(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	nas := torrent.Peer{IP: net.IPv4(127, 0, 0, 1), Port: ln.Addr().(*net.TCPAddr).Port}
	lockedPeerSeams(t, func(context.Context, settings.RemoteDownloaderConfig) (torrent.Peer, error) {
		return nas, nil
	}, nil) // real addPeers
	withRemoteURL("http://nas.example/transmission/rpc")
	waitPeer(t)

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisableUTP = true
	cfg.DisableIPv6 = true
	cfg.DisableEncryption = true // plain handshake, so the info hash is readable
	cfg.NoDefaultPortForwarding = true
	cfg.LocalServiceDiscovery = nil
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	hash := testHash(9)
	goTorrent, _ := client.AddTorrentInfoHash(hash)
	tor := &Torrent{Torrent: goTorrent, RemoteDownload: true}

	if !tor.addRemotePeer() {
		t.Fatal("peer not added")
	}
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal("client never dialed the NAS address:", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	hs := make([]byte, 48)
	if _, err := io.ReadFull(conn, hs); err != nil {
		t.Fatal("handshake:", err)
	}
	if string(hs[1:20]) != "BitTorrent protocol" || [20]byte(hs[28:48]) != [20]byte(hash) {
		t.Fatalf("handshake %q", hs)
	}
}

// A flagged torrent still fetching its info gets the peer from the tick
// (the NAS can serve the metadata).
func TestProgressEventAddsPeerBeforeInfo(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spy := &peerSpy{}
	cachedPeer(t, spy)
	goTorrent, _ := bts.client.AddTorrentInfoHash(testHash(11))
	tor := &Torrent{Torrent: goTorrent, RemoteDownload: true, lastTimeSpeed: time.Now()}

	tor.progressEvent()
	if n := spy.countFor(goTorrent); n != 1 {
		t.Fatalf("adds before info = %d, want 1", n)
	}
}

// drop() clears t.Torrent while a progress event may still run.
func TestAddRemotePeerAfterDropAddsNothing(t *testing.T) {
	withTorrDB(t)
	spy := &peerSpy{}
	cachedPeer(t, spy)
	tor := &Torrent{RemoteDownload: true}

	if tor.addRemotePeer() || spy.count() != 0 {
		t.Fatal("peer added to a dropped torrent")
	}
}

func TestResolveRemotePeerRejectsPortOutOfRange(t *testing.T) {
	withTorrDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		io.WriteString(w, `{"result":"success","arguments":{"version":"2.82","rpc-version":15,"peer-port":70000}}`)
	}))
	t.Cleanup(srv.Close)
	cfg := settings.RemoteDownloaderConfig{Type: settings.DefaultRemoteDownloaderType, URL: srv.URL + "/transmission/rpc"}
	if _, err := resolveRemotePeer(context.Background(), cfg); err == nil {
		t.Fatal("peer port 70000 accepted")
	}
}
