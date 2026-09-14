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

// withPinAPI gives a test an empty torrents table and an empty BTServer.
// The previous torr BTServer is not readable from this package; it is nil in
// this test binary, so cleanup resets it to nil.
func withPinAPI(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldReadOnly, oldBTsets := sets.ReadOnly, sets.BTsets
	clearTorrentsDB()
	sets.ReadOnly = false
	sets.BTsets = &sets.BTSets{UseDisk: true, DefaultPinNext: 5}
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

func TestPinRejections(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		readOnly bool
		noDisk   bool
		want     int
	}{
		{"empty hash", `{"action":"pin","pin_mode":"next"}`, false, false, http.StatusBadRequest},
		{"read-only", `{"action":"pin","hash":"%s","pin_mode":"next"}`, true, false, http.StatusForbidden},
		{"empty mode", `{"action":"pin","hash":"%s"}`, false, false, http.StatusBadRequest},
		{"unknown mode", `{"action":"pin","hash":"%s","pin_mode":"bogus"}`, false, false, http.StatusBadRequest},
		{"negative next", `{"action":"pin","hash":"%s","pin_mode":"next","pin_next":-1}`, false, false, http.StatusBadRequest},
		{"next without disk", `{"action":"pin","hash":"%s","pin_mode":"next"}`, false, true, http.StatusBadRequest},
		{"all without disk", `{"action":"pin","hash":"%s","pin_mode":"all"}`, false, true, http.StatusBadRequest},
		{"unknown hash", `{"action":"pin","hash":"0909090909090909090909090909090909090909","pin_mode":"next"}`, false, false, http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withPinAPI(t)
			hash := addDBTorrent(1)
			sets.ReadOnly = tc.readOnly
			sets.BTsets.UseDisk = !tc.noDisk

			w := postTorrents(strings.Replace(tc.body, "%s", hash.HexString(), 1))

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d, body %s", w.Code, tc.want, w.Body)
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
			if tc.noDisk && !strings.Contains(payload.Error, "disk storage") {
				t.Errorf("error %q does not mention disk storage", payload.Error)
			}
			sets.ReadOnly = false
			if db := torr.GetTorrentDB(hash); db.PinMode != "" || db.PinNext != 0 {
				t.Errorf("rejected request changed the pin: %q/%d", db.PinMode, db.PinNext)
			}
		})
	}
}

type pinStatus struct {
	Hash    string  `json:"hash"`
	PinMode *string `json:"pin_mode"`
	PinNext *int    `json:"pin_next"`
}

func decodePinStatus(t *testing.T, w *httptest.ResponseRecorder) pinStatus {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %s", w.Code, w.Body)
	}
	var st pinStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("body is not valid JSON (%v): %q", err, w.Body.String())
	}
	return st
}

func TestPinNextUsesDefaultN(t *testing.T) {
	withPinAPI(t)
	hash := addDBTorrent(1)

	st := decodePinStatus(t, postTorrents(`{"action":"pin","hash":"`+hash.HexString()+`","pin_mode":"next"}`))

	if st.Hash != hash.HexString() {
		t.Fatalf("hash = %q, want %q", st.Hash, hash.HexString())
	}
	if st.PinMode == nil || *st.PinMode != "next" || st.PinNext == nil || *st.PinNext != 5 {
		t.Fatalf("pin = %v/%v, want next/5 (DefaultPinNext)", st.PinMode, st.PinNext)
	}
	if db := torr.GetTorrentDB(hash); db.PinMode != "next" || db.PinNext != 5 {
		t.Fatalf("DB pin = %q/%d, want next/5", db.PinMode, db.PinNext)
	}
}

func TestPinExplicitN(t *testing.T) {
	withPinAPI(t)
	hash := addDBTorrent(1)

	st := decodePinStatus(t, postTorrents(`{"action":"pin","hash":"`+hash.HexString()+`","pin_mode":"all","pin_next":7}`))
	if st.PinMode == nil || *st.PinMode != "all" || st.PinNext == nil || *st.PinNext != 7 {
		t.Fatalf("pin = %v/%v, want all/7", st.PinMode, st.PinNext)
	}

	st = decodePinStatus(t, postTorrents(`{"action":"pin","hash":"`+hash.HexString()+`","pin_mode":"next","pin_next":0}`))
	if st.PinMode == nil || *st.PinMode != "next" {
		t.Fatalf("pin_mode = %v, want next", st.PinMode)
	}
	if st.PinNext != nil {
		t.Fatalf("pin_next = %d, want absent for 0", *st.PinNext)
	}
	if db := torr.GetTorrentDB(hash); db.PinMode != "next" || db.PinNext != 0 {
		t.Fatalf("DB pin = %q/%d, want next/0", db.PinMode, db.PinNext)
	}
}

func TestPinOffWithoutDisk(t *testing.T) {
	withPinAPI(t)
	hash := addDBTorrent(1)
	decodePinStatus(t, postTorrents(`{"action":"pin","hash":"`+hash.HexString()+`","pin_mode":"all","pin_next":2}`))
	sets.BTsets.UseDisk = false

	st := decodePinStatus(t, postTorrents(`{"action":"pin","hash":"`+hash.HexString()+`","pin_mode":"off"}`))

	if st.PinMode != nil || st.PinNext != nil {
		t.Fatalf("pin = %v/%v, want absent", st.PinMode, st.PinNext)
	}
	if db := torr.GetTorrentDB(hash); db.PinMode != "" || db.PinNext != 0 {
		t.Fatalf("DB pin = %q/%d, want off", db.PinMode, db.PinNext)
	}
}

func TestListExposesPin(t *testing.T) {
	withPinAPI(t)
	pinned := addDBTorrent(1)
	unpinned := addDBTorrent(2)
	decodePinStatus(t, postTorrents(`{"action":"pin","hash":"`+pinned.HexString()+`","pin_mode":"next","pin_next":5}`))

	w := postTorrents(`{"action":"list"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, item := range raw {
		switch string(item["hash"]) {
		case `"` + pinned.HexString() + `"`:
			found++
			if string(item["pin_mode"]) != `"next"` || string(item["pin_next"]) != "5" {
				t.Errorf("pinned list item pin = %s/%s, want \"next\"/5", item["pin_mode"], item["pin_next"])
			}
		case `"` + unpinned.HexString() + `"`:
			found++
			if _, ok := item["pin_mode"]; ok {
				t.Errorf("unpinned list item has pin_mode")
			}
			if _, ok := item["pin_next"]; ok {
				t.Errorf("unpinned list item has pin_next")
			}
			for _, key := range []string{"pin_progress", "pin_error"} {
				if _, ok := item[key]; ok {
					t.Errorf("unpinned list item has %s", key)
				}
			}
		}
	}
	if found != 2 {
		t.Fatalf("list returned %d of 2 torrents: %s", found, w.Body)
	}
}
