package torr

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	"server/remotedl"
	"server/settings"
	"server/torr/state"
)

// statusSpy is a fetchRemoteStatus fake: it records each batch and answers
// with reply (or err) as they were when the call started. Each gate queued
// for a URL blocks one call for that URL until it is released.
type statusSpy struct {
	mu      sync.Mutex
	batches [][]string
	cfgs    []string
	reply   map[string]remotedl.RemoteStatus
	err     error
	gates   map[string][]chan struct{}
}

func (s *statusSpy) fetch(ctx context.Context, cfg settings.RemoteDownloaderConfig, hashes []string) (map[string]remotedl.RemoteStatus, error) {
	s.mu.Lock()
	s.batches = append(s.batches, append([]string(nil), hashes...))
	s.cfgs = append(s.cfgs, cfg.URL)
	reply, err := s.reply, s.err
	var gate chan struct{}
	if q := s.gates[cfg.URL]; len(q) > 0 {
		gate, s.gates[cfg.URL] = q[0], q[1:]
	}
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return reply, err
}

// hold makes the next not yet gated call for url block until the returned
// release runs; release is idempotent and also runs on cleanup.
func (s *statusSpy) hold(t *testing.T, url string) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	s.mu.Lock()
	if s.gates == nil {
		s.gates = map[string][]chan struct{}{}
	}
	s.gates[url] = append(s.gates[url], gate)
	s.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return release
}

func (s *statusSpy) set(reply map[string]remotedl.RemoteStatus, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reply, s.err = reply, err
}

func (s *statusSpy) calls() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.batches...)
}

// lockedStatusSeams replaces fetchRemoteStatus and the TTL under
// remoteStatuses.mu (production reads them under it) and clears the cache;
// restored on cleanup.
func lockedStatusSeams(t *testing.T, fetch func(context.Context, settings.RemoteDownloaderConfig, []string) (map[string]remotedl.RemoteStatus, error), ttl time.Duration) {
	t.Helper()
	c := &remoteStatuses
	c.mu.Lock()
	oldFetch, oldTTL := fetchRemoteStatus, remoteStatusTTL
	fetchRemoteStatus, remoteStatusTTL = fetch, ttl
	resetStatusCacheLocked()
	c.mu.Unlock()
	t.Cleanup(func() {
		c.mu.Lock()
		fetchRemoteStatus, remoteStatusTTL = oldFetch, oldTTL
		resetStatusCacheLocked()
		c.mu.Unlock()
	})
}

func resetStatusCacheLocked() {
	c := &remoteStatuses
	c.cfg, c.statuses, c.err, c.next, c.running = settings.RemoteDownloaderConfig{}, nil, nil, time.Time{}, 0
	c.gen++ // drop results of refreshes still running from earlier tests
}

func refreshing() bool {
	remoteStatuses.mu.Lock()
	defer remoteStatuses.mu.Unlock()
	return remoteStatuses.running != 0 && remoteStatuses.running == remoteStatuses.gen
}

// waitRefreshed waits until no refresh runs and n fetches happened.
func waitRefreshed(t *testing.T, s *statusSpy, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.calls()) < n || refreshing() {
		if time.Now().After(deadline) {
			t.Fatalf("fetches = %d, want %d (refreshing %v)", len(s.calls()), n, refreshing())
		}
		time.Sleep(time.Millisecond)
	}
}

const (
	nasURL   = "http://nas.invalid:9091/transmission/rpc"
	otherURL = "http://other.invalid:9091/transmission/rpc"
)

// withStatusSpy configures a remote downloader URL and a status fake.
func withStatusSpy(t *testing.T, ttl time.Duration) *statusSpy {
	t.Helper()
	settings.BTsets.RemoteDownloader = settings.RemoteDownloaderConfig{Type: settings.DefaultRemoteDownloaderType, URL: nasURL}
	s := &statusSpy{}
	lockedStatusSeams(t, s.fetch, ttl)
	return s
}

func statusFields(t *testing.T, st *state.TorrentStatus) map[string]json.RawMessage {
	t.Helper()
	buf, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(buf, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func remoteKeys(m map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if strings.HasPrefix(k, "remote_") {
			out[k] = string(v)
		}
	}
	return out
}

func stubStatus(t *testing.T, hash metainfo.Hash) *state.TorrentStatus {
	t.Helper()
	row := GetTorrentDB(hash)
	if row == nil {
		t.Fatal("row missing")
	}
	return row.Status()
}

// listed is what the list/get handlers do: kick a refresh, then read.
func listed(t *testing.T, hash metainfo.Hash) *state.TorrentStatus {
	t.Helper()
	RefreshRemoteStatus()
	return stubStatus(t, hash)
}

// Status() alone (stream path, tgbot, mcp, dlna, torrfs) never starts RPC,
// for stub and loaded flagged torrents, with an empty or a stale snapshot.
func TestStatusAloneNeverStartsRefresh(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	s := withStatusSpy(t, 0)
	tor := expiredTorrent(t, specWithInfo(t, "status-alone"))
	flaggedRow(t, tor.Hash())
	tor.RemoteDownload = true

	for i := 0; i < 20; i++ {
		tor.Status()
		stubStatus(t, tor.Hash())
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(s.calls()); n != 0 {
		t.Fatalf("Status() started %d fetches", n)
	}
	RefreshRemoteStatus()
	waitRefreshed(t, s, 1)
}

func TestRemoteStatusUnflaggedUnchanged(t *testing.T) {
	withTorrDB(t)
	s := withStatusSpy(t, time.Hour)
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))
	flaggedRow(t, testHash(2)) // a flagged row elsewhere must not leak into this one

	for i := 0; i < 3; i++ {
		got := remoteKeys(statusFields(t, stubStatus(t, hash)))
		if len(got) != 0 {
			t.Fatalf("unflagged status has %v", got)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(s.calls()); n != 0 {
		t.Fatalf("unflagged reads fetched %d times", n)
	}
}

func TestRemoteStatusNotConfiguredFetchesNothing(t *testing.T) {
	withTorrDB(t)
	s := withStatusSpy(t, time.Hour)
	settings.BTsets.RemoteDownloader.URL = " "
	flaggedRow(t, testHash(1))

	got := remoteKeys(statusFields(t, listed(t, testHash(1))))
	time.Sleep(20 * time.Millisecond)
	if !reflect.DeepEqual(got, map[string]string{"remote_download": "true"}) || len(s.calls()) != 0 {
		t.Fatalf("status %v, fetches %d", got, len(s.calls()))
	}
}

// One read of any flagged torrent refreshes all flagged rows in one batch;
// unflagged rows are not asked. Missing has no progress, 0 % keeps it.
func TestRemoteStatusStubBatchAndMapping(t *testing.T) {
	withTorrDB(t)
	s := withStatusSpy(t, time.Hour)
	a, b, c, u := testHash(1), testHash(2), testHash(3), testHash(4)
	for _, h := range []metainfo.Hash{a, b, c} {
		flaggedRow(t, h)
	}
	AddTorrentDB(dbTorrent(u))
	s.set(map[string]remotedl.RemoteStatus{
		a.HexString(): {State: remotedl.StateDownloading, Progress: 42.5},
		b.HexString(): {State: remotedl.StateQueued, Progress: 0, Error: "tracker warning"},
		c.HexString(): {State: remotedl.StateMissing},
	}, nil)

	first := remoteKeys(statusFields(t, listed(t, a)))
	if !reflect.DeepEqual(first, map[string]string{"remote_download": "true"}) {
		t.Fatalf("before the first refresh: %v", first)
	}
	waitRefreshed(t, s, 1)
	calls := s.calls()
	got := append([]string(nil), calls[0]...)
	sort.Strings(got)
	want := []string{a.HexString(), b.HexString(), c.HexString()}
	if len(calls) != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("batches = %v, want one with %v", calls, want)
	}

	cases := map[metainfo.Hash]map[string]string{
		a: {"remote_download": "true", "remote_state": `"downloading"`, "remote_progress": "42.5"},
		b: {"remote_download": "true", "remote_state": `"queued"`, "remote_progress": "0", "remote_error": `"tracker warning"`},
		c: {"remote_download": "true", "remote_state": `"missing"`},
	}
	for h, want := range cases {
		if got := remoteKeys(statusFields(t, listed(t, h))); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", h.HexString()[:4], got, want)
		}
	}
	if got := remoteKeys(statusFields(t, listed(t, u))); len(got) != 0 {
		t.Errorf("unflagged: %v", got)
	}
	if n := len(s.calls()); n != 1 {
		t.Fatalf("fresh snapshot refetched: %d fetches", n)
	}
}

func TestRemoteStatusLoadedTorrent(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	s := withStatusSpy(t, time.Hour)
	tor := expiredTorrent(t, specWithInfo(t, "remote-status-loaded"))
	flaggedRow(t, tor.Hash())
	tor.RemoteDownload = true
	s.set(map[string]remotedl.RemoteStatus{tor.Hash().HexString(): {State: remotedl.StateSeeding, Progress: 100}}, nil)

	RefreshRemoteStatus()
	waitRefreshed(t, s, 1)
	got := remoteKeys(statusFields(t, tor.Status()))
	want := map[string]string{"remote_download": "true", "remote_state": `"seeding"`, "remote_progress": "100"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded: %v, want %v", got, want)
	}
}

// A hanging client never delays Status: reads return at once, only one
// refresh runs, and nothing is shown until it ends.
func TestRemoteStatusNeverBlocksOnHangingFetch(t *testing.T) {
	withTorrDB(t)
	s := withStatusSpy(t, 0)
	s.hold(t, nasURL)
	flaggedRow(t, testHash(1))

	start := time.Now()
	for i := 0; i < 50; i++ {
		if got := remoteKeys(statusFields(t, listed(t, testHash(1)))); len(got) != 1 {
			t.Fatalf("status while refreshing: %v", got)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("50 reads took %v", d)
	}
	for deadline := time.Now().Add(5 * time.Second); len(s.calls()) == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(s.calls()); n != 1 {
		t.Fatalf("fetches = %d, want 1 in flight", n)
	}
}

// Stale snapshot: served while the refresh runs, replaced when it lands.
func TestRemoteStatusTTLRefreshServesOldSnapshot(t *testing.T) {
	withTorrDB(t)
	s := withStatusSpy(t, time.Hour)
	h := testHash(1)
	flaggedRow(t, h)
	s.set(map[string]remotedl.RemoteStatus{h.HexString(): {State: remotedl.StateDownloading, Progress: 10}}, nil)
	listed(t, h)
	waitRefreshed(t, s, 1)

	// expire it
	remoteStatuses.mu.Lock()
	remoteStatuses.next = time.Now().Add(-time.Millisecond)
	remoteStatuses.mu.Unlock()
	release := s.hold(t, nasURL)
	s.set(map[string]remotedl.RemoteStatus{h.HexString(): {State: remotedl.StateDownloading, Progress: 20}}, nil)

	if got := remoteKeys(statusFields(t, listed(t, h)))["remote_progress"]; got != "10" {
		t.Fatalf("stale read progress = %s, want the old 10", got)
	}
	release()
	waitRefreshed(t, s, 2)
	if got := remoteKeys(statusFields(t, listed(t, h)))["remote_progress"]; got != "20" {
		t.Fatalf("after refresh progress = %s, want 20", got)
	}
	if n := len(s.calls()); n != 2 {
		t.Fatalf("fetches = %d, want 2", n)
	}
}

// RPC failure: every flagged torrent shows state error with the reason, the
// failure is logged once, and the next attempt waits for the TTL.
func TestRemoteStatusRPCErrorShownLoggedOnceRetriedAfterTTL(t *testing.T) {
	withTorrDB(t)
	logs := captureLog(t)
	s := withStatusSpy(t, time.Hour)
	h := testHash(1)
	flaggedRow(t, h)
	s.set(nil, errors.New("dial tcp 192.168.0.111:9091: connection refused"))

	listed(t, h)
	waitRefreshed(t, s, 1)
	got := remoteKeys(statusFields(t, listed(t, h)))
	want := map[string]string{"remote_download": "true", "remote_state": `"error"`, "remote_error": `"dial tcp 192.168.0.111:9091: connection refused"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status %v, want %v", got, want)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(s.calls()); n != 1 {
		t.Fatalf("retried before the TTL: %d fetches", n)
	}

	remoteStatuses.mu.Lock()
	remoteStatuses.next = time.Time{}
	remoteStatuses.mu.Unlock()
	listed(t, h)
	waitRefreshed(t, s, 2)
	if n := strings.Count(logs.String(), "remote status: torrent-get failed"); n != 1 {
		t.Fatalf("failure logged %d times, want once:\n%s", n, logs)
	}

	s.set(map[string]remotedl.RemoteStatus{h.HexString(): {State: remotedl.StateSeeding, Progress: 100}}, nil)
	remoteStatuses.mu.Lock()
	remoteStatuses.next = time.Time{}
	remoteStatuses.mu.Unlock()
	listed(t, h)
	waitRefreshed(t, s, 3)
	if got := remoteKeys(statusFields(t, listed(t, h))); got["remote_state"] != `"seeding"` || got["remote_error"] != "" {
		t.Fatalf("after recovery: %v", got)
	}
	if !strings.Contains(logs.String(), "remote status: torrent-get works again") {
		t.Fatalf("recovery not logged:\n%s", logs)
	}
}

// A config change drops the old snapshot and the result of a refresh still
// running for the old config, and refreshes for the new one at once.
func TestRemoteStatusConfigChangeInvalidates(t *testing.T) {
	withTorrDB(t)
	s := withStatusSpy(t, time.Hour)
	h := testHash(1)
	flaggedRow(t, h)
	s.set(map[string]remotedl.RemoteStatus{h.HexString(): {State: remotedl.StateSeeding, Progress: 100}}, nil)
	listed(t, h)
	waitRefreshed(t, s, 1)

	releaseOther, releaseNAS := s.hold(t, otherURL), s.hold(t, nasURL)
	settings.BTsets.RemoteDownloader.URL = otherURL
	if got := remoteKeys(statusFields(t, listed(t, h))); len(got) != 1 {
		t.Fatalf("old config's snapshot shown after change: %v", got)
	}
	// back to the first URL while the refresh for "other" still runs; it has
	// captured the old (seeding) reply before the reply changes
	for deadline := time.Now().Add(5 * time.Second); len(s.calls()) < 2; {
		if time.Now().After(deadline) {
			t.Fatal("refresh for the other config never started")
		}
		time.Sleep(time.Millisecond)
	}
	settings.BTsets.RemoteDownloader.URL = nasURL
	s.set(map[string]remotedl.RemoteStatus{h.HexString(): {State: remotedl.StateStopped, Progress: 5}}, nil)
	listed(t, h)
	releaseNAS()
	waitRefreshed(t, s, 3)
	// the "other" refresh (answering seeding) ends last and must be dropped
	releaseOther()
	time.Sleep(50 * time.Millisecond)
	got := remoteKeys(statusFields(t, listed(t, h)))
	if got["remote_state"] != `"stopped"` {
		t.Fatalf("state = %v, want the newest config's result", got)
	}
	s.mu.Lock()
	cfgs := append([]string(nil), s.cfgs...)
	s.mu.Unlock()
	sort.Strings(cfgs[1:]) // the two refreshes after the changes start in any order
	if want := []string{nasURL, nasURL, otherURL}; !reflect.DeepEqual(cfgs, want) {
		t.Fatalf("fetch configs = %v", cfgs)
	}
}

// Turning the flag on refreshes on the next read even when the snapshot is
// fresh, so the new torrent's state appears without waiting for the TTL.
func TestSetTorrentRemoteOnInvalidatesStatus(t *testing.T) {
	withTorrDB(t)
	withRemote(t)
	s := &statusSpy{}
	lockedStatusSeams(t, s.fetch, time.Hour)
	old, h := testHash(1), testHash(2)
	flaggedRow(t, old)
	AddTorrentDB(dbTorrent(h))
	listed(t, old)
	waitRefreshed(t, s, 1)

	got, err := SetTorrentRemote(h.HexString(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status().RemoteState != "" {
		t.Fatal("state shown before a refresh")
	}
	RefreshRemoteStatus()
	waitRefreshed(t, s, 2)
	batch := s.calls()[1]
	sort.Strings(batch)
	if want := []string{old.HexString(), h.HexString()}; !reflect.DeepEqual(batch, want) {
		t.Fatalf("batch after flag on = %v, want %v", batch, want)
	}
}

// Invalidation while a refresh runs: that refresh read the rows before the
// flag was saved, so its late result is dropped and a new refresh starts at
// once; the cache keeps refreshing afterwards.
func TestRemoteStatusInvalidateDuringRefresh(t *testing.T) {
	withTorrDB(t)
	s := withStatusSpy(t, time.Hour)
	old, h := testHash(1), testHash(2)
	flaggedRow(t, old)
	release := s.hold(t, nasURL)
	listed(t, old) // refresh 1 in flight, batch = {old}
	for deadline := time.Now().Add(5 * time.Second); len(s.calls()) < 1; {
		if time.Now().After(deadline) {
			t.Fatal("first refresh never started")
		}
		time.Sleep(time.Millisecond)
	}

	flaggedRow(t, h)
	invalidateRemoteStatus()
	s.set(map[string]remotedl.RemoteStatus{
		old.HexString(): {State: remotedl.StateSeeding, Progress: 100},
		h.HexString():   {State: remotedl.StateDownloading, Progress: 1},
	}, nil)
	listed(t, h) // refresh 2 starts although refresh 1 still runs
	waitRefreshed(t, s, 2)
	release() // refresh 1 (empty reply, stale gen) lands last
	time.Sleep(50 * time.Millisecond)

	if got := remoteKeys(statusFields(t, listed(t, h)))["remote_state"]; got != `"downloading"` {
		t.Fatalf("new torrent state = %s, want downloading (stale result must be dropped)", got)
	}
	batch := s.calls()[1]
	sort.Strings(batch)
	if want := []string{old.HexString(), h.HexString()}; !reflect.DeepEqual(batch, want) {
		t.Fatalf("second batch = %v, want %v", batch, want)
	}
	// not wedged: after the TTL a read refreshes again
	remoteStatuses.mu.Lock()
	remoteStatuses.next = time.Time{}
	remoteStatuses.mu.Unlock()
	listed(t, h)
	waitRefreshed(t, s, 3)
}
