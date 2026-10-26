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

func TestNewUnknownType(t *testing.T) {
	for _, typ := range []string{"qbittorrent", ""} {
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
