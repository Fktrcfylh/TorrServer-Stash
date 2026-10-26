package torr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"server/remotedl"
	"server/settings"
)

const retracker = "udp://retracker.example:80/announce"

// rpcCall is one request the fake Transmission received.
type rpcCall struct {
	Method    string         `json:"method"`
	Arguments map[string]any `json:"arguments"`
}

// fakeRemote is a Transmission RPC endpoint without the session handshake.
// fail, when set, is the result string returned for every call.
type fakeRemote struct {
	mu    sync.Mutex
	calls []rpcCall
	fail  string
}

func (f *fakeRemote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var c rpcCall
	_ = json.Unmarshal(body, &c)
	f.mu.Lock()
	f.calls = append(f.calls, c)
	fail := f.fail
	f.mu.Unlock()
	if fail != "" {
		io.WriteString(w, `{"result":"`+fail+`","arguments":{}}`)
		return
	}
	if c.Method == "torrent-add" {
		io.WriteString(w, `{"result":"success","arguments":{"torrent-added":{"id":1,"name":"n","hashString":"x"}}}`)
		return
	}
	io.WriteString(w, `{"result":"success","arguments":{}}`)
}

func (f *fakeRemote) recorded() []rpcCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rpcCall(nil), f.calls...)
}

// withRemote points the saved remote downloader settings at a fake and
// makes the retracker list exactly {retracker}.
func withRemote(t *testing.T) *fakeRemote {
	t.Helper()
	f := &fakeRemote{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	settings.BTsets.RemoteDownloader = settings.RemoteDownloaderConfig{
		Type: settings.DefaultRemoteDownloaderType,
		URL:  srv.URL + "/transmission/rpc",
	}
	old := retrackers
	retrackers = func() []string { return []string{retracker} }
	t.Cleanup(func() { retrackers = old })
	// keep session-get of the NAS peer cache out of the recorded calls
	lockedPeerSeams(t, func(context.Context, settings.RemoteDownloaderConfig) (torrent.Peer, error) {
		return torrent.Peer{}, errors.New("no NAS peer in this test")
	}, nil)
	// and torrent-get of the status cache too
	lockedStatusSeams(t, func(context.Context, settings.RemoteDownloaderConfig, []string) (map[string]remotedl.RemoteStatus, error) {
		return nil, nil
	}, time.Hour)
	return f
}

func flaggedRow(t *testing.T, hash metainfo.Hash) {
	t.Helper()
	AddTorrentDB(dbTorrent(hash))
	if !settings.SetTorrentRemoteDownload(hash, true) {
		t.Fatal("no row to flag")
	}
}

func rowFlag(t *testing.T, hash metainfo.Hash) bool {
	t.Helper()
	row := GetTorrentDB(hash)
	if row == nil {
		t.Fatal("row missing")
	}
	return row.RemoteDownload
}

func TestSetTorrentRemoteOnSendsMetainfoWithoutRetrackers(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	settings.BTsets.RemoteDownloader.DownloadDir = "/mnt/HD/HD_a2/ts"
	spec := specWithInfo(t, "meta")
	spec.Trackers = [][]string{{"http://orig/announce", retracker}, {retracker}, {"http://orig/announce", "udp://orig2:6969"}}
	tor := dbTorrent(spec.InfoHash)
	tor.TorrentSpec = spec
	AddTorrentDB(tor)

	got, err := SetTorrentRemote(spec.InfoHash.HexString(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Status().RemoteDownload || !rowFlag(t, spec.InfoHash) {
		t.Fatal("flag not saved after a successful add")
	}
	calls := f.recorded()
	if len(calls) != 1 || calls[0].Method != "torrent-add" {
		t.Fatalf("calls = %+v", calls)
	}
	args := calls[0].Arguments
	if args["download-dir"] != "/mnt/HD/HD_a2/ts" {
		t.Fatalf("download-dir = %v", args["download-dir"])
	}
	if _, ok := args["filename"]; ok {
		t.Fatal("filename sent with metainfo")
	}
	raw, err := base64.StdEncoding.DecodeString(args["metainfo"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var mi metainfo.MetaInfo
	if err := bencode.Unmarshal(raw, &mi); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mi.InfoBytes, spec.InfoBytes) {
		t.Fatal("info bytes changed")
	}
	want := metainfo.AnnounceList{{"http://orig/announce"}, {"udp://orig2:6969"}}
	if !reflect.DeepEqual(mi.AnnounceList, want) || mi.Announce != "http://orig/announce" {
		t.Fatalf("announce = %q list = %v, want %v", mi.Announce, mi.AnnounceList, want)
	}
}

func TestSetTorrentRemoteOnFallsBackToMagnet(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	hash := testHash(1)
	tor := dbTorrent(hash)
	tor.TorrentSpec = &torrent.TorrentSpec{InfoHash: hash, DisplayName: "dn", Trackers: [][]string{{"http://orig/announce", retracker}}}
	AddTorrentDB(tor)

	if _, err := SetTorrentRemote(hash.HexString(), true); err != nil {
		t.Fatal(err)
	}
	args := f.recorded()[0].Arguments
	if _, ok := args["metainfo"]; ok {
		t.Fatal("metainfo sent without info bytes")
	}
	if _, ok := args["download-dir"]; ok {
		t.Fatal("empty download-dir sent")
	}
	m, err := metainfo.ParseMagnetUri(args["filename"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if m.InfoHash != hash || m.DisplayName != "dn" || !reflect.DeepEqual(m.Trackers, []string{"http://orig/announce"}) {
		t.Fatalf("magnet = %+v", m)
	}
}

func TestSetTorrentRemoteOnRPCFailureKeepsFlagOff(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	f.fail = "duplicate torrent"
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))

	_, err := SetTorrentRemote(hash.HexString(), true)
	if err == nil || !strings.Contains(err.Error(), "duplicate torrent") || errors.Is(err, ErrRemoteConfig) {
		t.Fatalf("err = %v", err)
	}
	if rowFlag(t, hash) {
		t.Fatal("flag saved after a failed add")
	}
}

func TestSetTorrentRemoteOnNotConfigured(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))

	_, err := SetTorrentRemote(hash.HexString(), true)
	if !errors.Is(err, ErrRemoteConfig) {
		t.Fatalf("err = %v, want ErrRemoteConfig", err)
	}
	if rowFlag(t, hash) {
		t.Fatal("flag saved without a downloader")
	}
}

// A torrent not flagged here is never touched in the client.
func TestSetTorrentRemoteOffUnflaggedSendsNothing(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))

	if _, err := SetTorrentRemote(hash.HexString(), false); err != nil {
		t.Fatal(err)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
}

func TestSetTorrentRemoteOffRemovesWithData(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	hash := testHash(1)
	flaggedRow(t, hash)

	got, err := SetTorrentRemote(strings.ToUpper(hash.HexString()), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status().RemoteDownload || rowFlag(t, hash) {
		t.Fatal("flag still on after a successful remove")
	}
	calls := f.recorded()
	if len(calls) != 1 || calls[0].Method != "torrent-remove" {
		t.Fatalf("calls = %+v", calls)
	}
	if !reflect.DeepEqual(calls[0].Arguments["ids"], []any{hash.HexString()}) || calls[0].Arguments["delete-local-data"] != true {
		t.Fatalf("args = %v", calls[0].Arguments)
	}
}

func TestSetTorrentRemoteOffFailureKeepsFlag(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	f.fail = "boom"
	hash := testHash(1)
	flaggedRow(t, hash)

	if _, err := SetTorrentRemote(hash.HexString(), false); err == nil {
		t.Fatal("want error")
	}
	if !rowFlag(t, hash) {
		t.Fatal("flag cleared after a failed remove")
	}
}

func TestSetTorrentRemoteOffNotConfiguredKeepsFlag(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	flaggedRow(t, hash)

	if _, err := SetTorrentRemote(hash.HexString(), false); !errors.Is(err, ErrRemoteConfig) {
		t.Fatalf("err = %v, want ErrRemoteConfig", err)
	}
	if !rowFlag(t, hash) {
		t.Fatal("flag cleared without a downloader")
	}
}

func TestSetTorrentRemoteUnknownReadOnlyInvalid(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	if _, err := SetTorrentRemote(testHash(9).HexString(), true); !errors.Is(err, ErrTorrentNotFound) {
		t.Fatalf("unknown: err = %v", err)
	}
	if _, err := SetTorrentRemote("zz", true); !errors.Is(err, ErrTorrentNotFound) {
		t.Fatalf("invalid: err = %v", err)
	}
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))
	settings.ReadOnly = true
	if _, err := SetTorrentRemote(hash.HexString(), true); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only: err = %v", err)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
}

// A loaded torrent without a DB row is saved first and sent as metainfo
// from its info; the loaded torrent mirrors the flag.
func TestSetTorrentRemoteLoadedWithoutRow(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	f := withRemote(t)
	tor := loadedTorrent(t, "loaded-remote")

	got, err := SetTorrentRemote(tor.Hash().HexString(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got != tor || !tor.Status().RemoteDownload {
		t.Fatal("loaded torrent does not mirror the flag")
	}
	if !rowFlag(t, tor.Hash()) {
		t.Fatal("row not created with the flag")
	}
	if _, ok := f.recorded()[0].Arguments["metainfo"]; !ok {
		t.Fatal("loaded torrent with info not sent as metainfo")
	}
}

func TestRemTorrentFlaggedRemovesRemoteFirst(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	hash := testHash(1)
	flaggedRow(t, hash)

	if err := RemTorrent(hash.HexString()); err != nil {
		t.Fatal(err)
	}
	calls := f.recorded()
	if len(calls) != 1 || calls[0].Method != "torrent-remove" || !reflect.DeepEqual(calls[0].Arguments["ids"], []any{hash.HexString()}) {
		t.Fatalf("calls = %+v", calls)
	}
	if GetTorrentDB(hash) != nil {
		t.Fatal("row kept after a successful remove")
	}
}

func TestRemTorrentFlaggedFailureKeepsTorrent(t *testing.T) {
	withTorrDB(t)
	withDiskMode(t)
	f := withRemote(t)
	f.fail = "boom"
	hash := testHash(1)
	flaggedRow(t, hash)
	dir := hashDir(t, hash)

	if err := RemTorrent(hash.HexString()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if GetTorrentDB(hash) == nil || !dirExists(t, dir) {
		t.Fatal("torrent removed although the remote remove failed")
	}
}

func TestRemTorrentFlaggedNotConfiguredKeepsTorrent(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	flaggedRow(t, hash)

	if err := RemTorrent(hash.HexString()); !errors.Is(err, ErrRemoteConfig) {
		t.Fatalf("err = %v, want ErrRemoteConfig", err)
	}
	if GetTorrentDB(hash) == nil {
		t.Fatal("row removed without a downloader")
	}
}

func TestRemTorrentUnflaggedSendsNothing(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))

	if err := RemTorrent(hash.HexString()); err != nil {
		t.Fatal(err)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
	if GetTorrentDB(hash) != nil {
		t.Fatal("row kept")
	}
}

// The flag is copied from the row into every Torrent built from it, a general
// save keeps it, and the status JSON shows it only when on.
func TestRemoteFlagCopiesAndStatus(t *testing.T) {
	withTorrDB(t)
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))
	if st, _ := json.Marshal(GetTorrentDB(hash).Status()); strings.Contains(string(st), "remote_download") {
		t.Fatalf("unflagged status has the key: %s", st)
	}
	settings.SetTorrentRemoteDownload(hash, true)

	if !GetTorrentDB(hash).RemoteDownload {
		t.Fatal("GetTorrentDB lost the flag")
	}
	if !ListTorrentsDB()[hash].RemoteDownload {
		t.Fatal("ListTorrentsDB lost the flag")
	}
	stub := GetTorrentDB(hash)
	stub.RemoteDownload = false // a stale in-memory copy must not clear the row
	stub.Title = "edited"
	AddTorrentDB(stub)
	if !rowFlag(t, hash) {
		t.Fatal("general save cleared the flag")
	}
	st, _ := json.Marshal(GetTorrentDB(hash).Status())
	if !strings.Contains(string(st), `"remote_download":true`) {
		t.Fatalf("status = %s", st)
	}
}

func TestLoadAndAddTorrentCopyRemoteFlag(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spec := specWithInfo(t, "copy-load")
	row := dbTorrent(spec.InfoHash)
	row.TorrentSpec = spec
	AddTorrentDB(row)
	settings.SetTorrentRemoteDownload(spec.InfoHash, true)

	loaded := LoadTorrent(GetTorrentDB(spec.InfoHash))
	if loaded == nil || !loaded.Status().RemoteDownload {
		t.Fatal("LoadTorrent lost the flag")
	}
	bts.RemoveTorrent(spec.InfoHash)
	<-loaded.closed

	added, err := AddTorrent(specWithInfo(t, "copy-load"), "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !added.Status().RemoteDownload {
		t.Fatal("AddTorrent lost the flag of the DB row")
	}
}

// A row saved before the info arrived has no info bytes; the loaded torrent's
// info is used, so it is still sent as metainfo.
func TestSetTorrentRemoteUsesLoadedInfoWhenRowHasNone(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	f := withRemote(t)
	tor := loadedTorrent(t, "row-without-info")
	AddTorrentDB(dbTorrent(tor.Hash()))
	if row := GetTorrentDB(tor.Hash()); len(row.TorrentSpec.InfoBytes) != 0 {
		t.Fatal("fixture row has info bytes")
	}

	if _, err := SetTorrentRemote(tor.Hash().HexString(), true); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(f.recorded()[0].Arguments["metainfo"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var mi metainfo.MetaInfo
	if err := bencode.Unmarshal(raw, &mi); err != nil {
		t.Fatal(err)
	}
	if mi.HashInfoBytes() != tor.Hash() {
		t.Fatal("metainfo does not carry the loaded torrent's info")
	}
}

// GetTorrent of a DB torrent loads it in the background with the row's flag.
func TestGetTorrentAsyncLoadCopiesRemoteFlag(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	spec := specWithInfo(t, "async-load")
	row := dbTorrent(spec.InfoHash)
	row.TorrentSpec = spec
	AddTorrentDB(row)
	settings.SetTorrentRemoteDownload(spec.InfoHash, true)

	GetTorrent(spec.InfoHash.HexString())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if tor := bts.GetTorrent(spec.InfoHash); tor != nil && tor.Status().RemoteDownload {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("loaded torrent has no remote flag")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A loaded torrent without a row and without info cannot be saved yet; the
// request is refused instead of crashing in AddTorrentDB.
func TestSetTorrentRemoteLoadedWithoutInfoRefused(t *testing.T) {
	withTorrDB(t)
	withOfflineClient(t)
	f := withRemote(t)
	hash := testHash(7)
	if _, err := NewTorrent(&torrent.TorrentSpec{InfoHash: hash}, bts); err != nil {
		t.Fatal(err)
	}

	if _, err := SetTorrentRemote(hash.HexString(), true); !errors.Is(err, ErrInfoPending) {
		t.Fatalf("err = %v, want ErrInfoPending", err)
	}
	if GetTorrentDB(hash) != nil {
		t.Fatal("row created")
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Fatalf("calls = %+v, want none", calls)
	}
}

// Info bytes that do not hash to the row's hash are never sent.
func TestSetTorrentRemoteRefusesMismatchedInfo(t *testing.T) {
	withTorrDB(t)
	f := withRemote(t)
	hash := testHash(1)
	tor := dbTorrent(hash)
	tor.TorrentSpec = &torrent.TorrentSpec{InfoHash: hash, InfoBytes: specWithInfo(t, "other").InfoBytes}
	AddTorrentDB(tor)

	if _, err := SetTorrentRemote(hash.HexString(), true); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
	if calls := f.recorded(); len(calls) != 0 || rowFlag(t, hash) {
		t.Fatalf("calls = %+v, flag = %v", calls, rowFlag(t, hash))
	}
}

// A remote write blocked on the client holds only its own torrent's lock:
// removing another, unflagged torrent does not wait for it.
func TestRemTorrentUnflaggedDoesNotWaitForOtherRemoteWrite(t *testing.T) {
	withTorrDB(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		entered <- struct{}{}
		<-release
		io.WriteString(w, `{"result":"success","arguments":{"torrent-added":{"id":1,"name":"n","hashString":"x"}}}`)
	}))
	t.Cleanup(srv.Close)
	settings.BTsets.RemoteDownloader = settings.RemoteDownloaderConfig{Type: settings.DefaultRemoteDownloaderType, URL: srv.URL}
	busy, other := testHash(1), testHash(2)
	AddTorrentDB(dbTorrent(busy))
	AddTorrentDB(dbTorrent(other))

	busyDone := make(chan struct{})
	go func() {
		defer close(busyDone)
		SetTorrentRemote(busy.HexString(), true)
	}()
	// the blocked write finishes before the globals are restored
	defer func() { close(release); <-busyDone }()
	<-entered
	done := make(chan error, 1)
	go func() { done <- RemTorrent(other.HexString()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RemTorrent of an unflagged torrent waited for another torrent's RPC")
	}
}

// RemTorrent of the torrent whose remote write is running waits for it, so it
// sees the flag that write saves and removes the client copy too.
func TestRemTorrentWaitsForSameTorrentRemoteWrite(t *testing.T) {
	withTorrDB(t)
	f := &fakeRemote{}
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(f.recorded()) == 0 {
			entered <- struct{}{}
			<-release
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	settings.BTsets.RemoteDownloader = settings.RemoteDownloaderConfig{Type: settings.DefaultRemoteDownloaderType, URL: srv.URL}
	hash := testHash(1)
	AddTorrentDB(dbTorrent(hash))

	onDone := make(chan error, 1)
	go func() {
		_, err := SetTorrentRemote(hash.HexString(), true)
		onDone <- err
	}()
	<-entered
	remDone := make(chan error, 1)
	go func() { remDone <- RemTorrent(hash.HexString()) }()
	select {
	case <-remDone:
		close(release)
		<-onDone
		t.Fatal("RemTorrent ran while the same torrent's remote write was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-onDone; err != nil {
		t.Fatal(err)
	}
	if err := <-remDone; err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, c := range f.recorded() {
		methods = append(methods, c.Method)
	}
	if !reflect.DeepEqual(methods, []string{"torrent-add", "torrent-remove"}) {
		t.Fatalf("methods = %v, want add then remove", methods)
	}
}
