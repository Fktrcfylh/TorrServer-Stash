package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	sets "server/settings"
)

// fakeRemote answers session-get like Transmission 2.82 on the NAS, after the
// 409 handshake.
func fakeRemote(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("X-Transmission-Session-Id") != "sid" {
			w.Header().Set("X-Transmission-Session-Id", "sid")
			w.WriteHeader(http.StatusConflict)
			return
		}
		io.WriteString(w, `{"result":"success","arguments":{"version":"2.82 (14160)","rpc-version":15,"peer-port":51413,"lpd-enabled":false}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withSavedRemote installs a BTsets with the given remote downloader config
// and restores the previous one.
func withSavedRemote(t *testing.T, cfg sets.RemoteDownloaderConfig) {
	t.Helper()
	old := sets.BTsets
	sets.BTsets = &sets.BTSets{RemoteDownloader: cfg}
	t.Cleanup(func() { sets.BTsets = old })
}

func postSettings(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/settings", settings)
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func errorBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON (%v): %q", err, w.Body.String())
	}
	return payload.Error
}

func TestTestRemoteSavedConfig(t *testing.T) {
	var n atomic.Int32
	srv := fakeRemote(t, &n)
	saved := sets.RemoteDownloaderConfig{Type: "transmission", URL: srv.URL + "/transmission/rpc"}
	withSavedRemote(t, saved)

	w := postSettings(t, `{"action":"test_remote"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["version"] != "2.82 (14160)" || got["peer_port"] != float64(51413) || got["rpc_version"] != float64(15) || got["lpd_enabled"] != false {
		t.Fatalf("body = %s", w.Body.String())
	}
	if n.Load() != 2 {
		t.Fatalf("requests = %d, want 2", n.Load())
	}
	if sets.BTsets.RemoteDownloader != saved {
		t.Fatalf("saved config changed: %+v", sets.BTsets.RemoteDownloader)
	}
}

func TestTestRemoteBodyConfigOverridesSaved(t *testing.T) {
	var n atomic.Int32
	srv := fakeRemote(t, &n)
	saved := sets.RemoteDownloaderConfig{Type: "transmission"} // disabled
	withSavedRemote(t, saved)

	body := `{"action":"test_remote","remote_downloader":{"Type":"transmission","URL":"` + srv.URL + `/transmission/rpc"}}`
	w := postSettings(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"peer_port":51413`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	if sets.BTsets.RemoteDownloader != saved {
		t.Fatalf("test_remote must not save the body config: %+v", sets.BTsets.RemoteDownloader)
	}
}

// test_remote with the qBittorrent type reads app/version and app/preferences.
func TestTestRemoteQbittorrent(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v2/app/version":
			io.WriteString(w, "v4.5.0")
		case "/api/v2/app/preferences":
			io.WriteString(w, `{"listen_port":52255,"lsd":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	withSavedRemote(t, sets.RemoteDownloaderConfig{Type: "transmission"})

	w := postSettings(t, `{"action":"test_remote","remote_downloader":{"Type":"qbittorrent","URL":"`+srv.URL+`"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if want := `{"version":"v4.5.0","rpc_version":0,"peer_port":52255,"lpd_enabled":true}`; w.Body.String() != want {
		t.Fatalf("body = %s, want %s", w.Body.String(), want)
	}
	if strings.Join(paths, ",") != "/api/v2/app/version,/api/v2/app/preferences" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestTestRemoteDisabled(t *testing.T) {
	withSavedRemote(t, sets.RemoteDownloaderConfig{Type: "transmission"})
	w := postSettings(t, `{"action":"test_remote"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if msg := errorBody(t, w); !strings.Contains(msg, "not configured") {
		t.Fatalf("error = %q", msg)
	}
}

func TestTestRemoteBadBodyConfig(t *testing.T) {
	var n atomic.Int32
	srv := fakeRemote(t, &n)
	// A valid saved config must not be used when the body carries a bad one.
	withSavedRemote(t, sets.RemoteDownloaderConfig{Type: "transmission", URL: srv.URL})
	w := postSettings(t, `{"action":"test_remote","remote_downloader":{"Type":"deluge","URL":"http://nas:8112"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if msg := errorBody(t, w); !strings.Contains(msg, "deluge") {
		t.Fatalf("error = %q", msg)
	}
	if n.Load() != 0 {
		t.Fatalf("saved client was called %d times", n.Load())
	}
}

func TestTestRemoteRPCFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "<h1>403: Forbidden</h1><p>Unauthorized IP Address.</p>")
	}))
	t.Cleanup(srv.Close)
	withSavedRemote(t, sets.RemoteDownloaderConfig{Type: "transmission", URL: srv.URL})

	w := postSettings(t, `{"action":"test_remote"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if msg := errorBody(t, w); !strings.Contains(msg, "HTTP 403") || !strings.Contains(msg, "Unauthorized IP Address") {
		t.Fatalf("error = %q", msg)
	}
}

func TestTestRemoteUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	body := `{"action":"test_remote","remote_downloader":{"Type":"transmission","URL":"` + srv.URL + `","Username":"u","Password":"bad"}}`
	withSavedRemote(t, sets.RemoteDownloaderConfig{Type: "transmission"})

	w := postSettings(t, body)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if msg := errorBody(t, w); !strings.Contains(msg, "401") || strings.Contains(msg, "bad") {
		t.Fatalf("error = %q (must name 401, never echo the password)", msg)
	}
}

func TestTestRemoteURLCredentialsRejected(t *testing.T) {
	withSavedRemote(t, sets.RemoteDownloaderConfig{Type: "transmission"})
	w := postSettings(t, `{"action":"test_remote","remote_downloader":{"Type":"transmission","URL":"http://admin:s3cret@nas:9091/transmission/rpc"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("response leaks the password: %s", w.Body.String())
	}
}
