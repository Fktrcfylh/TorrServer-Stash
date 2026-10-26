package remotedl

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"server/log"
)

const (
	sessionIDHeader = "X-Transmission-Session-Id"
	// maxResponseBytes caps how much of a response body is read.
	maxResponseBytes = 8 << 20
	// maxErrorExcerpt caps the body excerpt put into error messages.
	maxErrorExcerpt = 200
	// maxAttempts bounds HTTP requests per call: the first one plus resends
	// after 409s that hand out a new session id.
	maxAttempts = 3
)

// requestTimeout bounds every HTTP request to the client. A var so tests can
// shorten it.
var requestTimeout = 10 * time.Second

// transmission is a client for the legacy Transmission RPC dialect
// (rpc-version 15+, kebab-case keys, 409 session-id handshake).
type transmission struct {
	url      string
	username string
	password string
	http     *http.Client

	mu        sync.Mutex
	sessionID string // last X-Transmission-Session-Id the daemon handed out
}

func newTransmission(rpcURL, username, password string) *transmission {
	return &transmission{
		url:      rpcURL,
		username: username,
		password: password,
		http:     &http.Client{},
	}
}

type rpcRequest struct {
	Method    string `json:"method"`
	Arguments any    `json:"arguments,omitempty"`
}

type rpcResponse struct {
	Result    string          `json:"result"`
	Arguments json.RawMessage `json:"arguments"`
}

type sessionGetArgs struct {
	Version    string `json:"version"`
	RPCVersion int    `json:"rpc-version"`
	PeerPort   int    `json:"peer-port"`
	LPDEnabled bool   `json:"lpd-enabled"`
}

// Session runs session-get.
func (t *transmission) Session(ctx context.Context) (SessionInfo, error) {
	var args sessionGetArgs
	if err := t.call(ctx, "session-get", nil, &args); err != nil {
		return SessionInfo{}, err
	}
	info := SessionInfo{
		Version:    args.Version,
		RPCVersion: args.RPCVersion,
		PeerPort:   args.PeerPort,
		LPDEnabled: args.LPDEnabled,
	}
	if !info.LPDEnabled {
		log.TLogln("remotedl: warning: Transmission reports lpd-enabled=false; enable it in settings.json so TorrServer finds the NAS via LPD, url:", t.url)
	}
	return info, nil
}

type torrentAddArgs struct {
	Metainfo    string `json:"metainfo,omitempty"`
	Filename    string `json:"filename,omitempty"`
	DownloadDir string `json:"download-dir,omitempty"`
}

type addedTorrent struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	HashString string `json:"hashString"`
}

type torrentAddReply struct {
	Added     *addedTorrent `json:"torrent-added"`
	Duplicate *addedTorrent `json:"torrent-duplicate"`
}

// Add runs torrent-add with metainfo (base64 .torrent) or filename (magnet).
func (t *transmission) Add(ctx context.Context, req AddRequest) (AddResult, error) {
	if (len(req.Metainfo) == 0) == (req.Magnet == "") {
		return AddResult{}, errors.New("transmission torrent-add: exactly one of metainfo and magnet is required")
	}
	args := torrentAddArgs{Filename: req.Magnet, DownloadDir: req.DownloadDir}
	if len(req.Metainfo) > 0 {
		args.Metainfo = base64.StdEncoding.EncodeToString(req.Metainfo)
	}
	var reply torrentAddReply
	if err := t.call(ctx, "torrent-add", args, &reply); err != nil {
		return AddResult{}, err
	}
	switch {
	case reply.Added != nil:
		return AddResult{Hash: reply.Added.HashString, Name: reply.Added.Name}, nil
	case reply.Duplicate != nil:
		return AddResult{Hash: reply.Duplicate.HashString, Name: reply.Duplicate.Name, Duplicate: true}, nil
	default:
		return AddResult{}, errors.New("transmission torrent-add: response has neither torrent-added nor torrent-duplicate")
	}
}

type torrentRemoveArgs struct {
	IDs             []string `json:"ids"`
	DeleteLocalData bool     `json:"delete-local-data"`
}

// Remove runs torrent-remove for one hash with delete-local-data. An empty or
// missing ids list makes Transmission remove every torrent, so anything but a
// 40-hex hash is refused before a request is built.
func (t *transmission) Remove(ctx context.Context, hash string) error {
	if !isInfoHash(hash) {
		return fmt.Errorf("transmission torrent-remove: refusing invalid info hash %q", hash)
	}
	args := torrentRemoveArgs{IDs: []string{strings.ToLower(hash)}, DeleteLocalData: true}
	return t.call(ctx, "torrent-remove", args, nil)
}

// isInfoHash reports whether s is a v1 info hash: exactly 40 hex chars.
func isInfoHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func (t *transmission) getSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// replaceSessionID installs next only while the cache still holds sent, the id
// the caller's request carried. If another call refreshed the id in the
// meantime, its newer value is kept.
func (t *transmission) replaceSessionID(sent, next string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessionID == sent {
		t.sessionID = next
	}
}

// call sends one RPC and decodes its arguments into out. Each 409 carries the
// daemon's current session id; the request is resent with it while the id
// keeps changing, up to maxAttempts HTTP requests per call. A 409 that hands
// back the id just sent, or no id, ends the call.
func (t *transmission) call(ctx context.Context, method string, args any, out any) error {
	body, err := json.Marshal(rpcRequest{Method: method, Arguments: args})
	if err != nil {
		return fmt.Errorf("transmission %s: encode request: %w", method, err)
	}

	var (
		status   int
		respBody []byte
	)
	sentID := t.getSessionID()
	for attempt := 1; ; attempt++ {
		var newID string
		status, respBody, newID, err = t.post(ctx, body, sentID)
		if err != nil {
			return fmt.Errorf("transmission %s: %w", method, err)
		}
		if status != http.StatusConflict {
			break
		}
		if newID == "" {
			return fmt.Errorf("transmission %s: HTTP 409 without %s header", method, sessionIDHeader)
		}
		if newID == sentID {
			return fmt.Errorf("transmission %s: HTTP 409 rejected the session id it issued", method)
		}
		if attempt >= maxAttempts {
			return fmt.Errorf("transmission %s: HTTP 409, session id still changing after %d attempts", method, attempt)
		}
		t.replaceSessionID(sentID, newID)
		log.TLogln("remotedl: transmission session id refreshed, url:", t.url, "attempt:", attempt)
		sentID = newID
	}

	switch {
	case status == http.StatusUnauthorized:
		return fmt.Errorf("transmission %s: %w (HTTP 401)", method, ErrUnauthorized)
	case status != http.StatusOK:
		return fmt.Errorf("transmission %s: HTTP %d: %s", method, status, excerpt(respBody))
	}

	var resp rpcResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return fmt.Errorf("transmission %s: decode response: %w: %s", method, err, excerpt(respBody))
	}
	if resp.Result != "success" {
		if resp.Result == "" {
			return fmt.Errorf("transmission %s: response has no result: %s", method, excerpt(respBody))
		}
		return fmt.Errorf("transmission %s: %s", method, resp.Result)
	}
	if out != nil && len(resp.Arguments) > 0 {
		if err := json.Unmarshal(resp.Arguments, out); err != nil {
			return fmt.Errorf("transmission %s: decode arguments: %w", method, err)
		}
	}
	return nil
}

// post sends body once under requestTimeout and returns the status, the body
// and the session id header of the response.
func (t *transmission) post(ctx context.Context, body []byte, sessionID string) (int, []byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set(sessionIDHeader, sessionID)
	}
	if t.username != "" || t.password != "" {
		req.SetBasicAuth(t.username, t.password)
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, "", fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, respBody, resp.Header.Get(sessionIDHeader), nil
}

// excerpt returns the start of an error body, trimmed, for error messages.
func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > maxErrorExcerpt {
		s = s[:maxErrorExcerpt] + "..."
	}
	return s
}
