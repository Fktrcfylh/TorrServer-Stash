// Package remotedl talks to a torrent client on the LAN (Transmission on a
// NAS) that predownloads torrents flagged in TorrServer.
package remotedl

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"server/settings"
)

// ErrDisabled is returned by New when no remote downloader is configured
// (empty URL). It is not a failure: callers treat it as "feature off".
var ErrDisabled = errors.New("remote downloader is not configured")

// ErrUnauthorized is wrapped by errors for rejected credentials (HTTP 401).
var ErrUnauthorized = errors.New("remote downloader rejected the credentials")

// SessionInfo is what the client reports about itself.
type SessionInfo struct {
	Version    string `json:"version"`
	RPCVersion int    `json:"rpc_version"`
	PeerPort   int    `json:"peer_port"`
	LPDEnabled bool   `json:"lpd_enabled"`
}

// Downloader is a remote torrent client.
type Downloader interface {
	// Session returns the client's version, RPC version, peer port and LPD flag.
	Session(ctx context.Context) (SessionInfo, error)
}

// New builds the Downloader selected by cfg.Type. An empty URL returns
// (nil, ErrDisabled).
func New(cfg settings.RemoteDownloaderConfig) (Downloader, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	if rawURL == "" {
		return nil, ErrDisabled
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		// The parse error repeats the raw URL, which may carry credentials.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("invalid remote downloader URL: %w", err)
	}
	// Credentials go in Username/Password; in the URL they would end up in logs.
	if u.User != nil {
		return nil, fmt.Errorf("invalid remote downloader URL %q: put the user and password in their own fields, not in the URL", u.Redacted())
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid remote downloader URL %q: want http(s)://host[:port]/path", rawURL)
	}
	switch cfg.Type {
	case settings.DefaultRemoteDownloaderType:
		return newTransmission(u.String(), cfg.Username, cfg.Password), nil
	default:
		return nil, fmt.Errorf("unknown remote downloader type %q", cfg.Type)
	}
}
