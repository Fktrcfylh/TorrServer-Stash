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
	"testing"

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
// when set, is the result string of every call.
type fakeRPC struct {
	mu      sync.Mutex
	methods []string
	fail    string
}

func (f *fakeRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.methods = append(f.methods, req.Method)
	fail := f.fail
	f.mu.Unlock()
	switch {
	case fail != "":
		io.WriteString(w, `{"result":"`+fail+`","arguments":{}}`)
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
