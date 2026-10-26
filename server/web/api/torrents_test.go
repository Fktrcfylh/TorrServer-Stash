package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
