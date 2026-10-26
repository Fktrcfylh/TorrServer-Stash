package remotedl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anacrolix/torrent/metainfo"

	"server/log"
)

const (
	// qbittorrentType is the RemoteDownloaderConfig.Type of this client.
	qbittorrentType = "qbittorrent"
	formContentType = "application/x-www-form-urlencoded"
	qbOK            = "Ok."
	qbFails         = "Fails."
)

// qbittorrent is a client for the qBittorrent Web API v2 (4.1+). The WebUI
// session cookie lives in the cookie jar. Requests carry no Referer/Origin:
// qBittorrent's CSRF check passes requests without both headers, and a
// Referer built from the configured URL would fail behind a reverse proxy
// that rewrites Host.
type qbittorrent struct {
	base     string // configured URL without trailing slash
	username string
	password string
	http     *http.Client

	// loginMu serializes logins, so concurrent 403s send one auth/login.
	loginMu sync.Mutex
	// loginRejected (under loginMu): the WebUI answered "Fails." to these
	// credentials. It bans the IP after a few failures, so this client never
	// tries them again; a changed config builds a new client.
	loginRejected bool
	// lpdWarned: the lsd=false warning is logged once per client
	lpdWarned atomic.Bool
}

func newQbittorrent(baseURL, username, password string) *qbittorrent {
	jar, _ := cookiejar.New(nil) // error is always nil without options
	return &qbittorrent{
		base:     strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http:     &http.Client{Jar: jar},
	}
}

type qbPreferences struct {
	ListenPort int  `json:"listen_port"`
	LSD        bool `json:"lsd"`
}

// Session reads app/version and app/preferences. qBittorrent has no RPC
// version number, so RPCVersion stays 0.
func (q *qbittorrent) Session(ctx context.Context) (SessionInfo, error) {
	version, err := q.request(ctx, "app/version", http.MethodGet, "app/version", "", nil)
	if err != nil {
		return SessionInfo{}, err
	}
	body, err := q.request(ctx, "app/preferences", http.MethodGet, "app/preferences", "", nil)
	if err != nil {
		return SessionInfo{}, err
	}
	var prefs qbPreferences
	if err := json.Unmarshal(body, &prefs); err != nil {
		return SessionInfo{}, fmt.Errorf("qbittorrent app/preferences: decode response: %w: %s", err, excerpt(body))
	}
	info := SessionInfo{
		Version:    strings.TrimSpace(string(version)),
		PeerPort:   prefs.ListenPort,
		LPDEnabled: prefs.LSD,
	}
	if !info.LPDEnabled && q.lpdWarned.CompareAndSwap(false, true) {
		log.TLogln("remotedl: warning: qBittorrent reports Local Peer Discovery off; enable it in Options → BitTorrent so TorrServer finds the client via LPD, url:", q.base)
	}
	return info, nil
}

// Add posts torrents/add with the .torrent file or the magnet. qBittorrent
// answers "Fails." for a duplicate and for an invalid torrent alike; the
// torrent being present afterwards makes it a duplicate.
func (q *qbittorrent) Add(ctx context.Context, req AddRequest) (AddResult, error) {
	if (len(req.Metainfo) == 0) == (req.Magnet == "") {
		return AddResult{}, errors.New("qbittorrent torrents/add: exactly one of metainfo and magnet is required")
	}
	res, err := addTarget(req)
	if err != nil {
		return AddResult{}, err
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if len(req.Metainfo) > 0 {
		part, err := mw.CreateFormFile("torrents", res.Hash+".torrent")
		if err == nil {
			_, err = part.Write(req.Metainfo)
		}
		if err != nil {
			return AddResult{}, fmt.Errorf("qbittorrent torrents/add: encode request: %w", err)
		}
	} else if err := mw.WriteField("urls", req.Magnet); err != nil {
		return AddResult{}, fmt.Errorf("qbittorrent torrents/add: encode request: %w", err)
	}
	if req.DownloadDir != "" {
		if err := mw.WriteField("savepath", req.DownloadDir); err != nil {
			return AddResult{}, fmt.Errorf("qbittorrent torrents/add: encode request: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return AddResult{}, fmt.Errorf("qbittorrent torrents/add: encode request: %w", err)
	}

	body, err := q.request(ctx, "torrents/add", http.MethodPost, "torrents/add", mw.FormDataContentType(), buf.Bytes())
	if err != nil {
		return AddResult{}, err
	}
	switch strings.TrimSpace(string(body)) {
	case qbOK:
		return res, nil
	case qbFails:
		st, err := q.Status(ctx, []string{res.Hash})
		if err != nil {
			return AddResult{}, fmt.Errorf("qbittorrent torrents/add: Fails., duplicate check: %w", err)
		}
		if st[res.Hash].State == StateMissing {
			return AddResult{}, errors.New("qbittorrent torrents/add: Fails. and the torrent is not in the client (invalid torrent or magnet?)")
		}
		res.Duplicate = true
		return res, nil
	default:
		return AddResult{}, fmt.Errorf("qbittorrent torrents/add: unexpected response: %s", excerpt(body))
	}
}

// addTarget reads the info hash and name of the torrent being added; the
// Web API does not report them.
func addTarget(req AddRequest) (AddResult, error) {
	if req.Magnet != "" {
		m, err := metainfo.ParseMagnetUri(req.Magnet)
		if err != nil {
			return AddResult{}, fmt.Errorf("qbittorrent torrents/add: parse magnet: %w", err)
		}
		return AddResult{Hash: m.InfoHash.HexString(), Name: m.DisplayName}, nil
	}
	mi, err := metainfo.Load(bytes.NewReader(req.Metainfo))
	if err != nil {
		return AddResult{}, fmt.Errorf("qbittorrent torrents/add: parse .torrent: %w", err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return AddResult{}, fmt.Errorf("qbittorrent torrents/add: parse .torrent info: %w", err)
	}
	return AddResult{Hash: mi.HashInfoBytes().HexString(), Name: info.Name}, nil
}

// Remove posts torrents/delete for one hash with deleteFiles. "all" deletes
// every torrent, so anything but a 40-hex hash is refused before a request.
// qBittorrent answers 200 for an unknown hash too.
func (q *qbittorrent) Remove(ctx context.Context, hash string) error {
	if !isInfoHash(hash) {
		return fmt.Errorf("qbittorrent torrents/delete: refusing invalid info hash %q", hash)
	}
	form := url.Values{"hashes": {strings.ToLower(hash)}, "deleteFiles": {"true"}}
	_, err := q.request(ctx, "torrents/delete", http.MethodPost, "torrents/delete", formContentType, []byte(form.Encode()))
	return err
}

type qbTorrent struct {
	Hash     string  `json:"hash"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
}

// Status reads torrents/info for all hashes in one request. Without hashes
// torrents/info lists every torrent, so an empty list sends no request.
func (q *qbittorrent) Status(ctx context.Context, hashes []string) (map[string]RemoteStatus, error) {
	out := make(map[string]RemoteStatus, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if !isInfoHash(h) {
			return nil, fmt.Errorf("qbittorrent torrents/info: refusing invalid info hash %q", h)
		}
		h = strings.ToLower(h)
		if _, dup := out[h]; !dup {
			ids = append(ids, h)
			out[h] = RemoteStatus{State: StateMissing}
		}
	}
	query := url.Values{"hashes": {strings.Join(ids, "|")}}.Encode()
	body, err := q.request(ctx, "torrents/info", http.MethodGet, "torrents/info?"+query, "", nil)
	if err != nil {
		return nil, err
	}
	var torrents []qbTorrent
	if err := json.Unmarshal(body, &torrents); err != nil {
		return nil, fmt.Errorf("qbittorrent torrents/info: decode response: %w: %s", err, excerpt(body))
	}
	for _, tr := range torrents {
		h := strings.ToLower(tr.Hash)
		if _, asked := out[h]; !asked {
			continue
		}
		st := RemoteStatus{State: qbittorrentState(tr.State), Progress: math.Round(tr.Progress*1000) / 10}
		if st.State == StateError {
			st.Error = fmt.Sprintf("qBittorrent state %q", tr.State)
		}
		out[h] = st
	}
	return out, nil
}

// qbittorrentState maps a torrents/info state (4.1+, plus the 5.0 stopped*
// names); an unknown one is an error.
func qbittorrentState(state string) string {
	switch state {
	case "uploading", "stalledUP", "forcedUP":
		return StateSeeding
	case "downloading", "stalledDL", "metaDL", "forcedMetaDL", "forcedDL", "allocating":
		return StateDownloading
	case "pausedUP", "pausedDL", "stoppedUP", "stoppedDL":
		return StateStopped
	case "queuedUP", "queuedDL":
		return StateQueued
	case "checkingUP", "checkingDL", "checkingResumeData", "moving":
		return StateChecking
	default: // error, missingFiles, unknown
		return StateError
	}
}

// request sends one Web API call and returns the body of a 200. A 403 means
// no valid session: with credentials configured it logs in once and resends.
func (q *qbittorrent) request(ctx context.Context, op, method, path, contentType string, body []byte) ([]byte, error) {
	status, respBody, err := q.send(ctx, method, path, contentType, body)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent %s: %w", op, err)
	}
	if status == http.StatusForbidden && (q.username != "" || q.password != "") {
		log.TLogln("remotedl: qbittorrent session missing or expired, logging in, url:", q.base)
		if err := q.login(ctx); err != nil {
			return nil, fmt.Errorf("qbittorrent %s: %w", op, err)
		}
		status, respBody, err = q.send(ctx, method, path, contentType, body)
		if err != nil {
			return nil, fmt.Errorf("qbittorrent %s: %w", op, err)
		}
	}
	switch {
	case status == http.StatusForbidden:
		return nil, fmt.Errorf("qbittorrent %s: %w (HTTP 403: set the user and password, or allow this host without login in qBittorrent)", op, ErrUnauthorized)
	case status != http.StatusOK:
		return nil, fmt.Errorf("qbittorrent %s: HTTP %d: %s", op, status, excerpt(respBody))
	}
	return respBody, nil
}

// login posts auth/login; the jar keeps the session cookie. Rejected
// credentials are remembered and never sent again by this client. A caller
// that waited for another login resends with that login's cookie or gets
// its rejection.
func (q *qbittorrent) login(ctx context.Context) error {
	q.loginMu.Lock()
	defer q.loginMu.Unlock()
	if q.loginRejected {
		return fmt.Errorf("login: %w (rejected before, not retried)", ErrUnauthorized)
	}
	form := url.Values{"username": {q.username}, "password": {q.password}}
	status, body, err := q.send(ctx, http.MethodPost, "auth/login", formContentType, []byte(form.Encode()))
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	reply := strings.TrimSpace(string(body))
	switch {
	case status == http.StatusOK && reply == qbOK:
		log.TLogln("remotedl: qbittorrent logged in, url:", q.base)
		return nil
	case status == http.StatusOK && reply == qbFails:
		q.loginRejected = true
		log.TLogln("remotedl: qbittorrent rejected the credentials, not retrying until the config changes, url:", q.base)
		return fmt.Errorf("login: %w", ErrUnauthorized)
	case status == http.StatusForbidden:
		return fmt.Errorf("login: HTTP 403 (IP banned after too many failed logins?): %s", excerpt(body))
	default:
		return fmt.Errorf("login: HTTP %d: %s", status, excerpt(body))
	}
}

// send makes one HTTP request to base/api/v2/path under requestTimeout and
// returns the status and the body.
func (q *qbittorrent) send(ctx context.Context, method, path, contentType string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.base+"/api/v2/"+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := q.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}
