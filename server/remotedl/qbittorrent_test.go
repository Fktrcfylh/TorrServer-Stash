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
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// fakeQB mimics the qBittorrent 4.5 Web API v2: a SID cookie from
// auth/login (unless user is empty: localhost auth bypass), 403 without a
// valid session, "Fails." for a duplicate add.
type fakeQB struct {
	user  string // empty = no login required
	pass  string
	lsd   bool
	ban   bool   // auth/login answers 403
	reply string // forced torrents/add body; empty = Ok./Fails. by presence
	fail  int    // forced status for every non-login API call; 0 = none

	mu        sync.Mutex
	sids      map[string]bool
	nextSID   int
	torrents  map[string]qbTorrent
	logins    int // auth/login requests
	requests  []string
	lastForm  url.Values
	lastFile  []byte
	headerErr string
}

func (f *fakeQB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/qb/api/v2/")
	f.requests = append(f.requests, path)
	if r.Header.Get("Referer") != "" || r.Header.Get("Origin") != "" {
		f.headerErr = fmt.Sprintf("%s carries Referer %q / Origin %q", path, r.Header.Get("Referer"), r.Header.Get("Origin"))
	}

	if path == "auth/login" {
		f.logins++
		r.ParseForm()
		switch {
		case f.ban:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "Your IP address has been banned after too many failed authentication attempts.")
		case r.PostForm.Get("username") == f.user && r.PostForm.Get("password") == f.pass:
			f.nextSID++
			sid := fmt.Sprintf("sid-%d", f.nextSID)
			if f.sids == nil {
				f.sids = map[string]bool{}
			}
			f.sids[sid] = true
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: sid, Path: "/"})
			io.WriteString(w, qbOK)
		default:
			io.WriteString(w, qbFails)
		}
		return
	}
	if f.user != "" {
		c, err := r.Cookie("SID")
		if err != nil || !f.sids[c.Value] {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "Forbidden")
			return
		}
	}
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		io.WriteString(w, "boom")
		return
	}
	if f.torrents == nil {
		f.torrents = map[string]qbTorrent{}
	}

	switch path {
	case "app/version":
		io.WriteString(w, "v4.5.0")
	case "app/preferences":
		fmt.Fprintf(w, `{"listen_port":52255,"lsd":%t,"save_path":"/Users/x/Downloads"}`, f.lsd)
	case "torrents/add":
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.lastForm = url.Values(r.MultipartForm.Value)
		var hash string
		if files := r.MultipartForm.File["torrents"]; len(files) > 0 {
			fh, _ := files[0].Open()
			f.lastFile, _ = io.ReadAll(fh)
			mi, err := metainfo.Load(bytes.NewReader(f.lastFile))
			if err != nil {
				w.WriteHeader(http.StatusUnsupportedMediaType)
				io.WriteString(w, "Torrent file is not valid.")
				return
			}
			hash = mi.HashInfoBytes().HexString()
		} else if m, err := metainfo.ParseMagnetUri(r.MultipartForm.Value["urls"][0]); err == nil {
			hash = m.InfoHash.HexString()
		}
		if f.reply != "" {
			io.WriteString(w, f.reply)
			return
		}
		if _, dup := f.torrents[hash]; dup || hash == "" {
			io.WriteString(w, qbFails)
			return
		}
		f.torrents[hash] = qbTorrent{Hash: hash, State: "metaDL"}
		io.WriteString(w, qbOK)
	case "torrents/delete":
		r.ParseForm()
		f.lastForm = r.PostForm
		for _, h := range strings.Split(r.PostForm.Get("hashes"), "|") {
			delete(f.torrents, h)
		}
	case "torrents/info":
		f.lastForm = r.URL.Query()
		out := []qbTorrent{}
		if q := r.URL.Query().Get("hashes"); q == "" {
			for _, tr := range f.torrents {
				out = append(out, tr)
			}
		} else {
			for _, h := range strings.Split(q, "|") {
				if tr, ok := f.torrents[h]; ok {
					out = append(out, tr)
				}
			}
		}
		json.NewEncoder(w).Encode(out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeQB) snapshot() (requests []string, logins int, form url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), f.logins, f.lastForm
}

// newQBFake starts the fake under /qb (a reverse-proxy style prefix) and a
// client with the given credentials.
func newQBFake(t *testing.T, f *fakeQB, user, pass string) *qbittorrent {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(func() {
		srv.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.headerErr != "" {
			t.Errorf("CSRF headers must not be sent: %s", f.headerErr)
		}
	})
	return newQbittorrent(srv.URL+"/qb/", user, pass)
}

// testTorrent is a minimal valid .torrent and its info hash.
func testTorrent(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: 16384, Pieces: make([]byte, 20), Length: 1}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	buf, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: infoBytes})
	if err != nil {
		t.Fatal(err)
	}
	return buf, metainfo.HashBytes(infoBytes).HexString()
}

func TestQBSessionWithoutLogin(t *testing.T) {
	f := &fakeQB{lsd: true}
	c := newQBFake(t, f, "admin", "pw") // localhost bypass: credentials unused

	info, err := c.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (SessionInfo{Version: "v4.5.0", PeerPort: 52255, LPDEnabled: true}); info != want {
		t.Fatalf("info = %+v, want %+v", info, want)
	}
	reqs, logins, _ := f.snapshot()
	if logins != 0 || strings.Join(reqs, ",") != "app/version,app/preferences" {
		t.Fatalf("requests = %v, logins = %d", reqs, logins)
	}
}

func TestQBLoginCookieReused(t *testing.T) {
	f := &fakeQB{user: "admin", pass: "pw"}
	c := newQBFake(t, f, "admin", "pw")
	for range 2 {
		if _, err := c.Session(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	reqs, logins, _ := f.snapshot()
	if logins != 1 {
		t.Fatalf("logins = %d, want 1 (cookie reused)", logins)
	}
	// 403 → login → resend, then 3 requests on the cookie
	want := "app/version,auth/login,app/version,app/preferences,app/version,app/preferences"
	if got := strings.Join(reqs, ","); got != want {
		t.Fatalf("requests = %s, want %s", got, want)
	}
}

func TestQBReloginOnceAfterExpiry(t *testing.T) {
	f := &fakeQB{user: "admin", pass: "pw"}
	c := newQBFake(t, f, "admin", "pw")
	if _, err := c.Status(context.Background(), []string{hashN(1)}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.sids = nil // WebUI session timeout / restart
	f.mu.Unlock()
	if _, err := c.Status(context.Background(), []string{hashN(1)}); err != nil {
		t.Fatal(err)
	}
	if _, logins, _ := f.snapshot(); logins != 2 {
		t.Fatalf("logins = %d, want 2", logins)
	}
}

// A session that is rejected again right after a fresh login fails the call
// instead of logging in again.
func TestQBForbiddenAfterLoginIsNotLooped(t *testing.T) {
	f := &fakeQB{user: "admin", pass: "pw"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth/login") {
			f.ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := newQbittorrent(srv.URL+"/qb", "admin", "pw")
	_, err := c.Session(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if _, logins, _ := f.snapshot(); logins != 1 {
		t.Fatalf("logins = %d, want 1", logins)
	}
}

// Rejected credentials are tried once per client: the WebUI bans the IP
// after a few failures.
func TestQBBadCredentialsNotRetried(t *testing.T) {
	f := &fakeQB{user: "admin", pass: "pw"}
	c := newQBFake(t, f, "admin", "wrong")
	for range 3 {
		_, err := c.Status(context.Background(), []string{hashN(1)})
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("err = %v, want ErrUnauthorized", err)
		}
	}
	reqs, logins, _ := f.snapshot()
	if logins != 1 {
		t.Fatalf("logins = %d, want 1", logins)
	}
	if len(reqs) != 4 { // info 403, login, then one 403 per later call
		t.Fatalf("requests = %v", reqs)
	}
	// a new client (changed config) tries again
	if _, err := newQBFake(t, f, "admin", "pw").Status(context.Background(), []string{hashN(1)}); err != nil {
		t.Fatal(err)
	}
}

// Concurrent calls that all hit 403 with bad credentials send one login.
func TestQBConcurrentBadCredentialsOneLogin(t *testing.T) {
	f := &fakeQB{user: "admin", pass: "pw"}
	release := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "auth/login") {
			arrived.Done() // every caller has its 403 before any login answers
			<-release
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newQbittorrent(srv.URL+"/qb", "admin", "wrong")

	errs := make(chan error, 5)
	for range 5 {
		go func() {
			_, err := c.Status(context.Background(), []string{hashN(1)})
			errs <- err
		}()
	}
	arrived.Wait()
	close(release)
	for range 5 {
		if err := <-errs; !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("err = %v, want ErrUnauthorized", err)
		}
	}
	if _, logins, _ := f.snapshot(); logins != 1 {
		t.Fatalf("logins = %d, want 1", logins)
	}
}

func TestQBForbiddenWithoutCredentials(t *testing.T) {
	f := &fakeQB{user: "admin", pass: "pw"}
	c := newQBFake(t, f, "", "")
	if _, err := c.Session(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if _, logins, _ := f.snapshot(); logins != 0 {
		t.Fatalf("logins = %d, want 0", logins)
	}
}

func TestQBBannedLogin(t *testing.T) {
	f := &fakeQB{user: "admin", pass: "pw", ban: true}
	c := newQBFake(t, f, "admin", "pw")
	_, err := c.Session(context.Background())
	if err == nil || !strings.Contains(err.Error(), "banned") {
		t.Fatalf("err = %v, want banned", err)
	}
}

func TestQBHTTPError(t *testing.T) {
	f := &fakeQB{fail: http.StatusInternalServerError}
	c := newQBFake(t, f, "", "")
	_, err := c.Status(context.Background(), []string{hashN(1)})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500: boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestQBAddMetainfo(t *testing.T) {
	f := &fakeQB{user: "admin", pass: "pw"}
	c := newQBFake(t, f, "admin", "pw")
	meta, hash := testTorrent(t, "movie")

	res, err := c.Add(context.Background(), AddRequest{Metainfo: meta, DownloadDir: "/mnt/dl"})
	if err != nil {
		t.Fatal(err)
	}
	if res != (AddResult{Hash: hash, Name: "movie"}) {
		t.Fatalf("result = %+v", res)
	}
	_, _, form := f.snapshot()
	f.mu.Lock()
	file := f.lastFile
	f.mu.Unlock()
	if !bytes.Equal(file, meta) {
		t.Fatalf("torrents part = %q, want the .torrent", file)
	}
	if got := form["savepath"]; len(got) != 1 || got[0] != "/mnt/dl" {
		t.Fatalf("savepath = %v", got)
	}
	for _, k := range []string{"urls", "paused", "tags", "category", "sequentialDownload"} {
		if _, ok := form[k]; ok {
			t.Fatalf("unexpected %s in %v", k, form)
		}
	}
}

func TestQBAddMagnetWithoutSavepath(t *testing.T) {
	f := &fakeQB{}
	c := newQBFake(t, f, "", "")
	magnet := "magnet:?xt=urn:btih:" + testHashHex + "&dn=Big+Buck+Bunny"

	res, err := c.Add(context.Background(), AddRequest{Magnet: magnet})
	if err != nil {
		t.Fatal(err)
	}
	if res != (AddResult{Hash: testHashHex, Name: "Big Buck Bunny"}) {
		t.Fatalf("result = %+v", res)
	}
	_, _, form := f.snapshot()
	if got := form["urls"]; len(got) != 1 || got[0] != magnet {
		t.Fatalf("urls = %v", got)
	}
	if _, ok := form["savepath"]; ok {
		t.Fatalf("savepath sent: %v", form)
	}
}

// "Fails." for a torrent the client already has is a duplicate: ours.
func TestQBAddDuplicateIsSuccess(t *testing.T) {
	meta, hash := testTorrent(t, "movie")
	f := &fakeQB{torrents: map[string]qbTorrent{hash: {Hash: hash, State: "uploading", Progress: 1}}}
	c := newQBFake(t, f, "", "")

	res, err := c.Add(context.Background(), AddRequest{Metainfo: meta})
	if err != nil {
		t.Fatal(err)
	}
	if res != (AddResult{Hash: hash, Name: "movie", Duplicate: true}) {
		t.Fatalf("result = %+v", res)
	}
	reqs, _, form := f.snapshot()
	if strings.Join(reqs, ",") != "torrents/add,torrents/info" || form["hashes"][0] != hash {
		t.Fatalf("requests = %v, last form = %v", reqs, form)
	}
}

func TestQBAddFailsWhenAbsent(t *testing.T) {
	f := &fakeQB{reply: qbFails}
	c := newQBFake(t, f, "", "")
	meta, _ := testTorrent(t, "movie")
	_, err := c.Add(context.Background(), AddRequest{Metainfo: meta})
	if err == nil || !strings.Contains(err.Error(), "not in the client") {
		t.Fatalf("err = %v", err)
	}
}

func TestQBAddErrors(t *testing.T) {
	meta, _ := testTorrent(t, "movie")
	for _, tc := range []struct {
		reply string
		want  string
	}{
		{"weird", "unexpected response: weird"},
	} {
		c := newQBFake(t, &fakeQB{reply: tc.reply}, "", "")
		if _, err := c.Add(context.Background(), AddRequest{Metainfo: meta}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("reply %q: err = %v, want %q", tc.reply, err, tc.want)
		}
	}

	// the fake answers 415 to a file it cannot parse; the client refuses it first
	f := &fakeQB{}
	c := newQBFake(t, f, "", "")
	for _, req := range []AddRequest{{}, {Metainfo: meta, Magnet: "magnet:?xt=urn:btih:" + testHashHex}, {Metainfo: []byte("junk")}, {Magnet: "http://x"}} {
		if _, err := c.Add(context.Background(), req); err == nil {
			t.Fatalf("req %+v: no error", req)
		}
	}
	if reqs, _, _ := f.snapshot(); len(reqs) != 0 {
		t.Fatalf("requests = %v, want none", reqs)
	}
}

func TestQBRemoveSendsOneHashWithFiles(t *testing.T) {
	f := &fakeQB{user: "admin", pass: "pw"}
	c := newQBFake(t, f, "admin", "pw")
	if err := c.Remove(context.Background(), strings.ToUpper(testHashHex)); err != nil {
		t.Fatal(err)
	}
	reqs, _, form := f.snapshot()
	if reqs[len(reqs)-1] != "torrents/delete" {
		t.Fatalf("requests = %v", reqs)
	}
	if form.Get("hashes") != testHashHex || form.Get("deleteFiles") != "true" || len(form) != 2 {
		t.Fatalf("form = %v", form)
	}
}

// hashes=all deletes every torrent; nothing but one 40-hex hash may be sent.
func TestQBRemoveRefusesInvalidHashWithoutRequest(t *testing.T) {
	f := &fakeQB{}
	c := newQBFake(t, f, "", "")
	for _, h := range []string{"", "all", "ALL", testHashHex[:39], testHashHex + "0", strings.Repeat("g", 40), testHashHex[:19] + "|" + testHashHex[:20]} {
		if err := c.Remove(context.Background(), h); err == nil {
			t.Fatalf("hash %q: no error", h)
		}
	}
	if reqs, _, _ := f.snapshot(); len(reqs) != 0 {
		t.Fatalf("requests = %v, want none", reqs)
	}
}

func TestQBStatusMapsStatesInOneRequest(t *testing.T) {
	cases := []struct {
		state    string
		progress float64
		want     RemoteStatus
	}{
		{"uploading", 1, RemoteStatus{State: StateSeeding, Progress: 100}},
		{"stalledUP", 1, RemoteStatus{State: StateSeeding, Progress: 100}},
		{"forcedUP", 1, RemoteStatus{State: StateSeeding, Progress: 100}},
		{"downloading", 0.21349, RemoteStatus{State: StateDownloading, Progress: 21.3}},
		{"stalledDL", 0.5, RemoteStatus{State: StateDownloading, Progress: 50}},
		{"metaDL", 0, RemoteStatus{State: StateDownloading}},
		{"forcedDL", 0.1, RemoteStatus{State: StateDownloading, Progress: 10}},
		{"allocating", 0, RemoteStatus{State: StateDownloading}},
		{"pausedUP", 1, RemoteStatus{State: StateStopped, Progress: 100}},
		{"pausedDL", 0.3, RemoteStatus{State: StateStopped, Progress: 30}},
		{"queuedUP", 1, RemoteStatus{State: StateQueued, Progress: 100}},
		{"queuedDL", 0, RemoteStatus{State: StateQueued}},
		{"checkingUP", 0.9, RemoteStatus{State: StateChecking, Progress: 90}},
		{"checkingDL", 0.2, RemoteStatus{State: StateChecking, Progress: 20}},
		{"checkingResumeData", 0, RemoteStatus{State: StateChecking}},
		{"moving", 1, RemoteStatus{State: StateChecking, Progress: 100}},
	}
	errStates := []string{"error", "missingFiles", "unknown", "somethingNew"}
	f := &fakeQB{torrents: map[string]qbTorrent{}}
	var hashes []string
	for i, tc := range cases {
		h := fmt.Sprintf("%040x", i+1)
		f.torrents[h] = qbTorrent{Hash: h, State: tc.state, Progress: tc.progress}
		hashes = append(hashes, strings.ToUpper(h))
	}
	for i, s := range errStates {
		h := fmt.Sprintf("%040x", 100+i)
		f.torrents[h] = qbTorrent{Hash: h, State: s, Progress: 0.5}
		hashes = append(hashes, h)
	}
	f.torrents[hashN(15)] = qbTorrent{Hash: hashN(15), State: "uploading"} // not asked
	missing := hashN(14)
	hashes = append(hashes, missing, missing)

	c := newQBFake(t, f, "", "")
	got, err := c.Status(context.Background(), hashes)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cases)+len(errStates)+1 {
		t.Fatalf("got %d statuses: %v", len(got), got)
	}
	for i, tc := range cases {
		if st := got[fmt.Sprintf("%040x", i+1)]; st != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.state, st, tc.want)
		}
	}
	for i, s := range errStates {
		want := RemoteStatus{State: StateError, Progress: 50, Error: fmt.Sprintf("qBittorrent state %q", s)}
		if st := got[fmt.Sprintf("%040x", 100+i)]; st != want {
			t.Errorf("%s: got %+v, want %+v", s, st, want)
		}
	}
	if st := got[missing]; st != (RemoteStatus{State: StateMissing}) {
		t.Errorf("missing: got %+v", st)
	}
	reqs, _, form := f.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("requests = %v, want one", reqs)
	}
	// lowercased, deduplicated, in the asked order
	want := strings.ToLower(strings.Join(hashes[:len(hashes)-1], "|"))
	if got := form.Get("hashes"); got != want {
		t.Fatalf("hashes = %s, want %s", got, want)
	}
}

// torrents/info without hashes lists every torrent: no request for an empty
// list, none for an invalid hash.
func TestQBStatusEmptyOrInvalidHashesSendNoRequest(t *testing.T) {
	f := &fakeQB{}
	c := newQBFake(t, f, "", "")
	got, err := c.Status(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, h := range []string{"", "all", testHashHex[:39]} {
		if _, err := c.Status(context.Background(), []string{testHashHex, h}); err == nil {
			t.Fatalf("hash %q: no error", h)
		}
	}
	if reqs, _, _ := f.snapshot(); len(reqs) != 0 {
		t.Fatalf("requests = %v, want none", reqs)
	}
}

func TestQBSessionWarnsLPDOffOncePerClient(t *testing.T) {
	var buf bytes.Buffer
	stdlog.SetOutput(&buf)
	t.Cleanup(func() { stdlog.SetOutput(os.Stderr) })

	c := newQBFake(t, &fakeQB{}, "", "")
	for range 3 {
		if _, err := c.Session(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "Local Peer Discovery off"); n != 1 {
		t.Fatalf("warnings = %d, want 1", n)
	}
}
