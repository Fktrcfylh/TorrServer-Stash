package remotedl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const naSession = `{"result":"success","arguments":{"version":"2.82 (14160)","rpc-version":15,"peer-port":51413,"lpd-enabled":false,"download-dir":"/mnt/HD/HD_a2/torrent"}}`

// fakeTransmission mimics the daemon's session-id handshake: a request
// without the current id gets 409 with the id in the header.
type fakeTransmission struct {
	mu        sync.Mutex
	sessionID string
	user      string // empty = no auth required
	pass      string
	reply     func(w http.ResponseWriter, body []byte)
	requests  atomic.Int32
	conflicts atomic.Int32
	lastBody  []byte
}

func (f *fakeTransmission) currentID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionID
}

func (f *fakeTransmission) rotate(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessionID = id
}

func (f *fakeTransmission) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if f.user != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != f.user || p != f.pass {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "<h1>401: Unauthorized</h1>")
			return
		}
	}
	id := f.currentID()
	if r.Header.Get(sessionIDHeader) != id {
		f.conflicts.Add(1)
		w.Header().Set(sessionIDHeader, id)
		w.WriteHeader(http.StatusConflict)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.lastBody = body
	f.mu.Unlock()
	if f.reply != nil {
		f.reply(w, body)
		return
	}
	io.WriteString(w, naSession)
}

func newFake(t *testing.T, f *fakeTransmission) *transmission {
	t.Helper()
	if f.sessionID == "" {
		f.sessionID = "id-1"
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newTransmission(srv.URL+"/transmission/rpc", f.user, f.pass)
}

func TestSessionHandshake(t *testing.T) {
	f := &fakeTransmission{}
	c := newFake(t, f)

	info, err := c.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := SessionInfo{Version: "2.82 (14160)", RPCVersion: 15, PeerPort: 51413, LPDEnabled: false}
	if info != want {
		t.Fatalf("info = %+v, want %+v", info, want)
	}
	if got := f.requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 (409 then retry)", got)
	}
	var req map[string]any
	if err := json.Unmarshal(f.lastBody, &req); err != nil {
		t.Fatalf("request body %q: %v", f.lastBody, err)
	}
	if req["method"] != "session-get" {
		t.Fatalf("method = %v, want session-get", req["method"])
	}
	if _, ok := req["arguments"]; ok {
		t.Fatalf("session-get must not send arguments: %s", f.lastBody)
	}

	// The cached id is reused: no further 409.
	if _, err := c.Session(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3 (cached id reused)", got)
	}
}

func TestSessionRehandshakeOnChangedID(t *testing.T) {
	f := &fakeTransmission{}
	c := newFake(t, f)
	if _, err := c.Session(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.rotate("id-2") // daemon restart / id expiry
	info, err := c.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.PeerPort != 51413 {
		t.Fatalf("PeerPort = %d", info.PeerPort)
	}
	if got := f.conflicts.Load(); got != 2 {
		t.Fatalf("conflicts = %d, want 2", got)
	}
	if got := c.getSessionID(); got != "id-2" {
		t.Fatalf("cached id = %q, want id-2", got)
	}
}

func TestSessionGivesUpWhenIDKeepsChanging(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A fresh id on every request: no resend ever matches.
		w.Header().Set(sessionIDHeader, "id-"+string(rune('a'+n.Add(1))))
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(srv.Close)
	c := newTransmission(srv.URL, "", "")
	_, err := c.Session(context.Background())
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("err = %v, want a 409 error", err)
	}
	if got := n.Load(); got != maxAttempts {
		t.Fatalf("requests = %d, want exactly %d", got, maxAttempts)
	}
}

// The id rotates (daemon restart) after the first 409 handed out id-1 but
// before the resend with id-1 arrives. The call must follow the second 409.
func TestSessionRotationBetweenConflictAndResend(t *testing.T) {
	var n atomic.Int32
	var sent []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		got := r.Header.Get(sessionIDHeader)
		mu.Lock()
		sent = append(sent, got)
		mu.Unlock()
		switch n.Add(1) {
		case 1: // handshake
			w.Header().Set(sessionIDHeader, "id-1")
			w.WriteHeader(http.StatusConflict)
		case 2: // resend with id-1, but the daemon restarted meanwhile
			w.Header().Set(sessionIDHeader, "id-2")
			w.WriteHeader(http.StatusConflict)
		default:
			if got != "id-2" {
				w.Header().Set(sessionIDHeader, "id-2")
				w.WriteHeader(http.StatusConflict)
				return
			}
			io.WriteString(w, naSession)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTransmission(srv.URL, "", "")
	info, err := c.Session(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want success after the rotated id", err)
	}
	if info.PeerPort != 51413 {
		t.Fatalf("PeerPort = %d", info.PeerPort)
	}
	if want := []string{"", "id-1", "id-2"}; strings.Join(sent, ",") != strings.Join(want, ",") {
		t.Fatalf("ids sent = %q, want %q", sent, want)
	}
	if got := c.getSessionID(); got != "id-2" {
		t.Fatalf("cached id = %q, want id-2", got)
	}
}

// A 409 that returns the very id just sent means the daemon rejects it for a
// reason a resend cannot fix; the call must stop instead of looping.
func TestSessionConflictWithSameID(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set(sessionIDHeader, "id-1")
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(srv.Close)
	_, err := newTransmission(srv.URL, "", "").Session(context.Background())
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("err = %v, want a 409 error", err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

// Another call refreshed the cache to a newer id while this call's 409 was in
// flight. The late 409 must not put its older id back into the cache.
func TestSessionOlderConflictKeepsNewerCachedID(t *testing.T) {
	var c *transmission
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		got := r.Header.Get(sessionIDHeader)
		if n.Add(1) == 1 {
			// Simulates a concurrent call that already stored the newer id.
			c.mu.Lock()
			c.sessionID = "id-new"
			c.mu.Unlock()
			w.Header().Set(sessionIDHeader, "id-old")
			w.WriteHeader(http.StatusConflict)
			return
		}
		if got != "id-old" && got != "id-new" {
			w.Header().Set(sessionIDHeader, "id-new")
			w.WriteHeader(http.StatusConflict)
			return
		}
		io.WriteString(w, naSession)
	}))
	t.Cleanup(srv.Close)
	c = newTransmission(srv.URL, "", "")
	if _, err := c.Session(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.getSessionID(); got != "id-new" {
		t.Fatalf("cached id = %q, want id-new (an older 409 overwrote it)", got)
	}
}

func TestSessionConflictWithoutHeader(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(srv.Close)
	_, err := newTransmission(srv.URL, "", "").Session(context.Background())
	if err == nil || !strings.Contains(err.Error(), sessionIDHeader) {
		t.Fatalf("err = %v, want missing header error", err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestSessionBasicAuth(t *testing.T) {
	f := &fakeTransmission{user: "admin", pass: "secret"}
	c := newFake(t, f)
	info, err := c.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "2.82 (14160)" {
		t.Fatalf("Version = %q", info.Version)
	}
}

func TestSessionUnauthorizedNoRetry(t *testing.T) {
	f := &fakeTransmission{user: "admin", pass: "secret"}
	c := newFake(t, f)
	c.password = "wrong"
	_, err := c.Session(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if got := f.requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 (bad credentials are never retried)", got)
	}
}

func TestSessionUnauthorizedAfterHandshake(t *testing.T) {
	// Some proxies answer 409 first and check credentials only on the retry.
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set(sessionIDHeader, "id-1")
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	_, err := newTransmission(srv.URL, "u", "bad").Session(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestSessionHTTPErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"ip whitelist", http.StatusForbidden, "<h1>403: Forbidden</h1><p>Unauthorized IP Address.</p><p>Either disable the IP address whitelist or add your address to it.</p>", []string{"HTTP 403", "Unauthorized IP Address"}},
		{"host whitelist", http.StatusMisdirectedRequest, "<h1>421: Misdirected Request</h1><p>Unauthorized Host Address.</p>", []string{"HTTP 421", "Unauthorized Host Address"}},
		{"server error", http.StatusInternalServerError, "boom", []string{"HTTP 500", "boom"}},
		{"long body trimmed", http.StatusForbidden, strings.Repeat("x", 1000), []string{"HTTP 403", strings.Repeat("x", maxErrorExcerpt) + "..."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTransmission{reply: func(w http.ResponseWriter, _ []byte) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}}
			_, err := newFake(t, f).Session(context.Background())
			if err == nil {
				t.Fatal("want error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %q, want it to contain %q", err, w)
				}
			}
			if len(err.Error()) > maxErrorExcerpt+100 {
				t.Errorf("error too long (%d bytes)", len(err.Error()))
			}
		})
	}
}

func TestSessionBadJSON(t *testing.T) {
	f := &fakeTransmission{reply: func(w http.ResponseWriter, _ []byte) {
		io.WriteString(w, `{"result":"success","arguments":`)
	}}
	_, err := newFake(t, f).Session(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v, want decode error", err)
	}
}

func TestSessionBadArgumentsType(t *testing.T) {
	f := &fakeTransmission{reply: func(w http.ResponseWriter, _ []byte) {
		io.WriteString(w, `{"result":"success","arguments":{"peer-port":"not a number"}}`)
	}}
	_, err := newFake(t, f).Session(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode arguments") {
		t.Fatalf("err = %v, want decode arguments error", err)
	}
}

func TestSessionResultError(t *testing.T) {
	f := &fakeTransmission{reply: func(w http.ResponseWriter, _ []byte) {
		io.WriteString(w, `{"result":"method name not recognized","arguments":{}}`)
	}}
	_, err := newFake(t, f).Session(context.Background())
	if err == nil || !strings.Contains(err.Error(), "method name not recognized") {
		t.Fatalf("err = %v, want the result string", err)
	}
}

func TestSessionMissingResult(t *testing.T) {
	f := &fakeTransmission{reply: func(w http.ResponseWriter, _ []byte) {
		io.WriteString(w, `{"arguments":{"peer-port":1}}`)
	}}
	_, err := newFake(t, f).Session(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no result") {
		t.Fatalf("err = %v, want missing result error", err)
	}
}

func TestSessionTimeout(t *testing.T) {
	old := requestTimeout
	requestTimeout = 100 * time.Millisecond
	t.Cleanup(func() { requestTimeout = old })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the client hanging up.
		io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	// The outer deadline only stops the test from hanging if requestTimeout
	// is not applied; the elapsed check is what detects that.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := newTransmission(srv.URL, "", "").Session(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v, requestTimeout not applied", elapsed)
	}
}

func TestSessionCallerCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := newTransmission(srv.URL, "", "").Session(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
}

func TestSessionUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	_, err := newTransmission(addr, "", "").Session(context.Background())
	if err == nil {
		t.Fatal("want connection error")
	}
}

// Stress variant: many concurrent calls on one shared client while the id
// rotates mid-round, over several rounds. The deterministic cases above pin
// down each interleaving; this one runs under -race to check the cache
// locking. One server and a pooled transport keep the connection count flat
// (a new server per round exhausted ephemeral ports under -count=50).
func TestSessionConcurrentRehandshake(t *testing.T) {
	f := &fakeTransmission{}
	c := newFake(t, f)
	tr := &http.Transport{MaxIdleConnsPerHost: 64}
	c.http = &http.Client{Transport: tr}
	t.Cleanup(tr.CloseIdleConnections)

	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		errs := make(chan error, 40)
		for i := 0; i < 40; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i == 20 {
					f.rotate(fmt.Sprintf("id-round-%d", round))
				}
				if _, err := c.Session(context.Background()); err != nil {
					errs <- err
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("round %d: %v", round, err)
		}
	}
}

// The lpd-enabled=false warning is logged once per client, not on every
// session-get (the shared client refreshes the NAS peer port periodically).
func TestSessionWarnsLPDOffOncePerClient(t *testing.T) {
	var buf bytes.Buffer
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(os.Stderr) })

	count := func() int { return strings.Count(buf.String(), "lpd-enabled=false") }
	c := newFake(t, &fakeTransmission{})
	for range 3 {
		if _, err := c.Session(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("warnings = %d, want 1 for one client", n)
	}
	if _, err := newFake(t, &fakeTransmission{}).Session(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("warnings = %d, want 2 after a new client", n)
	}
}

// A client that first reports LPD on still warns when it later reports off.
func TestSessionWarnsLPDOffAfterOn(t *testing.T) {
	var buf bytes.Buffer
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(os.Stderr) })

	var lpd atomic.Bool
	lpd.Store(true)
	f := &fakeTransmission{reply: func(w http.ResponseWriter, _ []byte) {
		fmt.Fprintf(w, `{"result":"success","arguments":{"version":"2.82","rpc-version":15,"peer-port":51413,"lpd-enabled":%t}}`, lpd.Load())
	}}
	c := newFake(t, f)
	if _, err := c.Session(context.Background()); err != nil {
		t.Fatal(err)
	}
	lpd.Store(false)
	if _, err := c.Session(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "lpd-enabled=false"); n != 1 {
		t.Fatalf("warnings = %d, want 1", n)
	}
}
