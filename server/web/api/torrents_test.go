package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/gin-gonic/gin"

	sets "server/settings"
	"server/torr"
)

// The settings DB is a process-wide singleton that cannot be reopened after
// CloseDB, so it is opened once for the package and emptied per test.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "web-api-test-")
	if err != nil {
		panic(err)
	}
	oldPath, oldReadOnly, oldSearchWA, oldBTsets := sets.Path, sets.ReadOnly, sets.SearchWA, sets.BTsets
	sets.Path = dir
	if err := sets.InitSets(false, false); err != nil {
		panic(err)
	}
	sets.Path, sets.ReadOnly, sets.SearchWA, sets.BTsets = oldPath, oldReadOnly, oldSearchWA, oldBTsets
	code := m.Run()
	sets.CloseDB()
	os.RemoveAll(dir)
	os.Exit(code)
}

// The /torrents handler is documented as `@Produce json`, but its failure
// paths used to abort with an empty body, so any client calling .json() on
// the response got a parse error instead of the reason it failed.
func TestTorrentsRejectionsCarryJSONBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{`},
		{"wrong type on an optional key", `{"action":"add","link":"magnet:?xt=urn:btih:x","title":123}`},
		{"unknown action", `{"action":"bogus"}`},
		{"missing action", `{}`},
		{"add without link", `{"action":"add"}`},
		{"get without hash", `{"action":"get"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.POST("/torrents", torrents)

			req := httptest.NewRequest(http.MethodPost, "/torrents", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
			if w.Body.Len() == 0 {
				t.Fatal("response body is empty; clients parsing it as JSON fail")
			}
			var payload struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body is not valid JSON (%v): %q", err, w.Body.String())
			}
			if payload.Error == "" {
				t.Errorf("no error message in body: %q", w.Body.String())
			}
		})
	}
}

// withTorrentsAPI gives a test an empty torrents table and an empty BTServer.
// The previous torr BTServer is not readable from this package; it is nil in
// this test binary, so cleanup resets it to nil.
func withTorrentsAPI(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldReadOnly, oldBTsets := sets.ReadOnly, sets.BTsets
	clearTorrentsDB()
	sets.ReadOnly = false
	sets.BTsets = &sets.BTSets{UseDisk: true}
	torr.InitApiHelper(&torr.BTServer{})
	t.Cleanup(func() {
		clearTorrentsDB()
		torr.InitApiHelper(nil)
		sets.ReadOnly, sets.BTsets = oldReadOnly, oldBTsets
	})
}

func clearTorrentsDB() {
	for _, db := range sets.ListTorrent() {
		sets.RemTorrent(db.InfoHash)
	}
}

func addDBTorrent(b byte) metainfo.Hash {
	var h metainfo.Hash
	h[0] = b
	h[19] = b
	torr.AddTorrentDB(&torr.Torrent{
		TorrentSpec: &torrent.TorrentSpec{InfoHash: h},
		Title:       "title",
		Data:        `{"TorrServer":{"Files":[]}}`,
	})
	return h
}

func postTorrents(body string) *httptest.ResponseRecorder {
	router := gin.New()
	router.POST("/torrents", torrents)
	req := httptest.NewRequest(http.MethodPost, "/torrents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// The disk pin was removed: "pin" is an unknown action again.
func TestPinActionIsUnknown(t *testing.T) {
	withTorrentsAPI(t)
	hash := addDBTorrent(1)

	w := postTorrents(`{"action":"pin","hash":"` + hash.HexString() + `","pin_mode":"all"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body %s", w.Code, http.StatusBadRequest, w.Body)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not valid JSON (%v): %q", err, w.Body.String())
	}
	if !strings.Contains(payload.Error, `unknown action: "pin"`) {
		t.Fatalf("error = %q, want unknown action", payload.Error)
	}
}

func TestListHasNoPinKeys(t *testing.T) {
	withTorrentsAPI(t)
	hash := addDBTorrent(1)

	w := postTorrents(`{"action":"list"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || string(raw[0]["hash"]) != `"`+hash.HexString()+`"` {
		t.Fatalf("list = %s, want the one DB torrent", w.Body)
	}
	for key := range raw[0] {
		if strings.HasPrefix(key, "pin_") {
			t.Errorf("list item has %s", key)
		}
	}
}

// fakeRPC is a Transmission endpoint without the session handshake; fail,
// when set, is the result string of every call. Writes are recorded in
// methods; torrent-get (status cache, background) only counted in gets and
// answered with torrents (a JSON array, default empty).
type fakeRPC struct {
	mu       sync.Mutex
	methods  []string
	gets     int
	fail     string
	torrents string
}

func (f *fakeRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	if req.Method == "torrent-get" {
		f.gets++
	} else {
		f.methods = append(f.methods, req.Method)
	}
	fail, torrents := f.fail, f.torrents
	f.mu.Unlock()
	if torrents == "" {
		torrents = "[]"
	}
	switch {
	case fail != "":
		io.WriteString(w, `{"result":"`+fail+`","arguments":{}}`)
	case req.Method == "torrent-get":
		io.WriteString(w, `{"result":"success","arguments":{"torrents":`+torrents+`}}`)
	case req.Method == "torrent-add":
		io.WriteString(w, `{"result":"success","arguments":{"torrent-duplicate":{"id":1,"name":"n","hashString":"x"}}}`)
	default:
		io.WriteString(w, `{"result":"success","arguments":{}}`)
	}
}

func (f *fakeRPC) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func withFakeRPC(t *testing.T) *fakeRPC {
	t.Helper()
	f := &fakeRPC{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	sets.BTsets.RemoteDownloader = sets.RemoteDownloaderConfig{Type: sets.DefaultRemoteDownloaderType, URL: srv.URL + "/transmission/rpc"}
	return f
}

func rowFlag(t *testing.T, hash metainfo.Hash) bool {
	t.Helper()
	for _, db := range sets.ListTorrent() {
		if db.InfoHash == hash {
			return db.RemoteDownload
		}
	}
	t.Fatal("row missing")
	return false
}

func TestRemoteActionOnAndOff(t *testing.T) {
	withTorrentsAPI(t)
	f := withFakeRPC(t)
	hash := addDBTorrent(1)

	w := postTorrents(`{"action":"remote","hash":"` + hash.HexString() + `","remote_download":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("on: status = %d, body %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"remote_download":true`) || !rowFlag(t, hash) {
		t.Fatalf("on: flag not set, body %s", w.Body)
	}
	w = postTorrents(`{"action":"remote","hash":"` + hash.HexString() + `","remote_download":false}`)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "remote_download") || rowFlag(t, hash) {
		t.Fatalf("off: status = %d, body %s", w.Code, w.Body)
	}
	if got := f.calls(); !reflect.DeepEqual(got, []string{"torrent-add", "torrent-remove"}) {
		t.Fatalf("calls = %v", got)
	}
}

func TestRemoteActionErrors(t *testing.T) {
	withTorrentsAPI(t)
	hash := addDBTorrent(1)
	h := hash.HexString()

	cases := []struct {
		name string
		body string
		want int
	}{
		{"no hash", `{"action":"remote","remote_download":true}`, http.StatusBadRequest},
		{"no remote_download", `{"action":"remote","hash":"` + h + `"}`, http.StatusBadRequest},
		{"not configured", `{"action":"remote","hash":"` + h + `","remote_download":true}`, http.StatusBadRequest},
		{"unknown hash", `{"action":"remote","hash":"` + strings.Repeat("ab", 20) + `","remote_download":true}`, http.StatusNotFound},
		{"invalid hash", `{"action":"remote","hash":"xyz","remote_download":true}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postTorrents(tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d, body %s", w.Code, tc.want, w.Body)
			}
			if errorBody(t, w) == "" {
				t.Fatalf("no error message in %s", w.Body)
			}
		})
	}

	f := withFakeRPC(t)
	f.fail = "disk full"
	w := postTorrents(`{"action":"remote","hash":"` + h + `","remote_download":true}`)
	if w.Code != http.StatusBadGateway || !strings.Contains(errorBody(t, w), "disk full") {
		t.Fatalf("rpc failure: status = %d, body %s", w.Code, w.Body)
	}
	if rowFlag(t, hash) {
		t.Fatal("flag saved after RPC failure")
	}

	sets.ReadOnly = true
	if w := postTorrents(`{"action":"remote","hash":"` + h + `","remote_download":true}`); w.Code != http.StatusForbidden {
		t.Fatalf("read-only: status = %d, body %s", w.Code, w.Body)
	}
}

func TestRemFlaggedFailureKeepsTorrent(t *testing.T) {
	withTorrentsAPI(t)
	f := withFakeRPC(t)
	hash := addDBTorrent(1)
	sets.SetTorrentRemoteDownload(hash, true)
	f.fail = "boom"

	w := postTorrents(`{"action":"rem","hash":"` + hash.HexString() + `"}`)
	if w.Code != http.StatusBadGateway || !strings.Contains(errorBody(t, w), "boom") {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	if !rowFlag(t, hash) {
		t.Fatal("flag lost")
	}

	f.fail = ""
	if w := postTorrents(`{"action":"rem","hash":"` + hash.HexString() + `"}`); w.Code != http.StatusOK {
		t.Fatalf("retry: status = %d, body %s", w.Code, w.Body)
	}
	if len(sets.ListTorrent()) != 0 {
		t.Fatal("row kept after a successful rem")
	}
}

func TestWipeReportsFlaggedFailures(t *testing.T) {
	withTorrentsAPI(t)
	f := withFakeRPC(t)
	flagged := addDBTorrent(1)
	addDBTorrent(2)
	sets.SetTorrentRemoteDownload(flagged, true)
	f.fail = "boom"

	w := postTorrents(`{"action":"wipe"}`)
	if w.Code != http.StatusBadGateway || !strings.Contains(errorBody(t, w), "1 torrent(s) not removed") {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	rows := sets.ListTorrent()
	if len(rows) != 1 || rows[0].InfoHash != flagged {
		t.Fatalf("rows = %v, want only the flagged one", rows)
	}
	if got := f.calls(); !reflect.DeepEqual(got, []string{"torrent-remove"}) {
		t.Fatalf("calls = %v, want one remove for the flagged torrent", got)
	}
}

// listItems posts list and returns the items keyed by hash.
func listItems(t *testing.T) map[string]map[string]json.RawMessage {
	t.Helper()
	w := postTorrents(`{"action":"list"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d, body %s", w.Code, w.Body)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, item := range raw {
		var h string
		_ = json.Unmarshal(item["hash"], &h)
		out[h] = item
	}
	return out
}

func remoteFields(item map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for k, v := range item {
		if strings.HasPrefix(k, "remote_") {
			out[k] = string(v)
		}
	}
	return out
}

// waitList polls list (as the web does) until cond holds for the item.
func waitList(t *testing.T, hash string, cond func(map[string]string) bool) map[string]string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := remoteFields(listItems(t)[hash])
		if cond(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("list item %s never matched, last %v", hash[:4], got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func flagRow(t *testing.T, hash metainfo.Hash) {
	t.Helper()
	if !sets.SetTorrentRemoteDownload(hash, true) {
		t.Fatal("no row to flag")
	}
}

func TestListShowsRemoteStatusOfFlaggedOnly(t *testing.T) {
	withTorrentsAPI(t)
	f := withFakeRPC(t)
	flagged, plain := addDBTorrent(1), addDBTorrent(2)
	flagRow(t, flagged)
	f.mu.Lock()
	f.torrents = `[{"hashString":"` + flagged.HexString() + `","status":4,"percentDone":0.256,"error":0,"errorString":""}]`
	f.mu.Unlock()

	got := waitList(t, flagged.HexString(), func(m map[string]string) bool { return m["remote_state"] != "" })
	want := map[string]string{"remote_download": "true", "remote_state": `"downloading"`, "remote_progress": "25.6"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("flagged = %v, want %v", got, want)
	}
	if got := remoteFields(listItems(t)[plain.HexString()]); len(got) != 0 {
		t.Fatalf("unflagged item has %v", got)
	}
	f.mu.Lock()
	gets, writes := f.gets, len(f.methods)
	f.mu.Unlock()
	if gets != 1 || writes != 0 {
		t.Fatalf("torrent-get %d times, writes %d; want one batch read and no writes", gets, writes)
	}
}

// A list without flagged torrents never asks the client.
func TestListWithoutFlaggedSendsNoRPC(t *testing.T) {
	withTorrentsAPI(t)
	f := withFakeRPC(t)
	hash := addDBTorrent(1)
	for i := 0; i < 5; i++ {
		if got := remoteFields(listItems(t)[hash.HexString()]); len(got) != 0 {
			t.Fatalf("unflagged item has %v", got)
		}
	}
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	gets := f.gets
	f.mu.Unlock()
	if gets != 0 {
		t.Fatalf("torrent-get sent %d times", gets)
	}
}

// hangingRPC holds every request until released (it reads the body first,
// so closing the server does not hang).
type hangingRPC struct {
	release chan struct{}
	hits    atomic.Int32
}

func (h *hangingRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	h.hits.Add(1)
	select {
	case <-h.release:
	case <-r.Context().Done():
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

// list never waits on RPC: with the client hanging, every list answers at
// once and shows only the flag.
func TestListDoesNotWaitOnHangingRemote(t *testing.T) {
	withTorrentsAPI(t)
	h := &hangingRPC{release: make(chan struct{})}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var once sync.Once
	release := func() { once.Do(func() { close(h.release) }) }
	t.Cleanup(release) // runs before srv.Close
	sets.BTsets.RemoteDownloader = sets.RemoteDownloaderConfig{Type: sets.DefaultRemoteDownloaderType, URL: srv.URL + "/transmission/rpc"}
	hash := addDBTorrent(1)
	flagRow(t, hash)

	for i := 0; i < 20; i++ {
		if i == 10 { // let the refresh reach the client before the rest
			for deadline := time.Now().Add(5 * time.Second); h.hits.Load() == 0 && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
		}
		start := time.Now()
		got := remoteFields(listItems(t)[hash.HexString()])
		if d := time.Since(start); d > 500*time.Millisecond {
			release()
			t.Fatalf("list %d took %v while the client hangs", i, d)
		}
		if !reflect.DeepEqual(got, map[string]string{"remote_download": "true"}) {
			release()
			t.Fatalf("list %d = %v", i, got)
		}
	}
	if n := h.hits.Load(); n != 1 {
		release()
		t.Fatalf("client got %d requests, want the one refresh in flight", n)
	}
}

// An unreachable client shows up as remote_state error with the reason.
func TestListShowsUnreachableRemoteAsError(t *testing.T) {
	withTorrentsAPI(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/transmission/rpc"
	srv.Close() // nothing listens there now
	sets.BTsets.RemoteDownloader = sets.RemoteDownloaderConfig{Type: sets.DefaultRemoteDownloaderType, URL: url}
	hash := addDBTorrent(1)
	flagRow(t, hash)

	got := waitList(t, hash.HexString(), func(m map[string]string) bool { return m["remote_state"] != "" })
	if got["remote_state"] != `"error"` || !strings.Contains(got["remote_error"], "torrent-get") || got["remote_progress"] != "" {
		t.Fatalf("unreachable = %v", got)
	}
}
