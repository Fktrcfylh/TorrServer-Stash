package remotedl

import (
	"errors"
	"strings"
	"testing"

	"server/settings"
)

func TestNewDisabled(t *testing.T) {
	for _, u := range []string{"", "   "} {
		d, err := New(settings.RemoteDownloaderConfig{Type: "transmission", URL: u})
		if !errors.Is(err, ErrDisabled) || d != nil {
			t.Fatalf("URL %q: got (%v, %v), want (nil, ErrDisabled)", u, d, err)
		}
	}
}

func TestNewTransmission(t *testing.T) {
	d, err := New(settings.RemoteDownloaderConfig{
		Type:     "transmission",
		URL:      " http://192.168.0.111:9091/transmission/rpc ",
		Username: "u",
		Password: "p",
	})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := d.(*transmission)
	if !ok {
		t.Fatalf("got %T, want *transmission", d)
	}
	if tr.url != "http://192.168.0.111:9091/transmission/rpc" || tr.username != "u" || tr.password != "p" {
		t.Fatalf("client = %+v", tr)
	}
}

func TestNewQbittorrent(t *testing.T) {
	d, err := New(settings.RemoteDownloaderConfig{Type: "qbittorrent", URL: " http://127.0.0.1:8080/ ", Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	qb, ok := d.(*qbittorrent)
	if !ok {
		t.Fatalf("got %T, want *qbittorrent", d)
	}
	if qb.base != "http://127.0.0.1:8080" || qb.username != "u" || qb.password != "p" || qb.http.Jar == nil {
		t.Fatalf("client = %+v", qb)
	}
}

func TestNewUnknownType(t *testing.T) {
	for _, typ := range []string{"deluge", "qBittorrent", ""} {
		_, err := New(settings.RemoteDownloaderConfig{Type: typ, URL: "http://nas:9091/transmission/rpc"})
		if err == nil || !strings.Contains(err.Error(), "unknown remote downloader type") {
			t.Fatalf("type %q: err = %v", typ, err)
		}
	}
}

func TestNewInvalidURL(t *testing.T) {
	for _, u := range []string{"nas:9091", "ftp://nas/rpc", "http://", "://bad", "http//nas"} {
		_, err := New(settings.RemoteDownloaderConfig{Type: "transmission", URL: u})
		if err == nil || !strings.Contains(err.Error(), "invalid remote downloader URL") {
			t.Fatalf("URL %q: err = %v", u, err)
		}
	}
}

func TestNewRejectsURLCredentials(t *testing.T) {
	_, err := New(settings.RemoteDownloaderConfig{Type: "transmission", URL: "http://admin:s3cret@nas:9091/transmission/rpc"})
	if err == nil || !strings.Contains(err.Error(), "own fields") {
		t.Fatalf("err = %v, want userinfo rejected", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestNewParseErrorHidesURL(t *testing.T) {
	_, err := New(settings.RemoteDownloaderConfig{Type: "transmission", URL: "http://admin:s3cret@nas:bad port/rpc"})
	if err == nil {
		t.Fatal("want parse error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestSharedReusesAndRebuilds(t *testing.T) {
	cfg := settings.RemoteDownloaderConfig{Type: "transmission", URL: "http://192.0.2.1:9091/transmission/rpc"}
	a, err := Shared(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Shared(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("same config built a new client")
	}
	cfg.Password = "changed"
	c, err := Shared(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatal("changed config reused the old client")
	}
	if tr := c.(*transmission); tr.password != "changed" {
		t.Fatalf("password = %q", tr.password)
	}
}

func TestSharedDisabledCachesNothing(t *testing.T) {
	cfg := settings.RemoteDownloaderConfig{Type: "transmission", URL: "http://192.0.2.2:9091/transmission/rpc"}
	a, err := Shared(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := Shared(settings.RemoteDownloaderConfig{Type: "transmission"}); !errors.Is(err, ErrDisabled) || d != nil {
		t.Fatalf("got (%v, %v), want (nil, ErrDisabled)", d, err)
	}
	b, err := Shared(cfg)
	if err != nil || b != a {
		t.Fatalf("disabled call replaced the cached client: %v %v", b, err)
	}
}
