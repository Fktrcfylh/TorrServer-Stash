package settings

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDefaultConfigRemoteDownloader(t *testing.T) {
	withTestDB(t)
	SetDefaultConfig()
	want := RemoteDownloaderConfig{Type: "transmission"}
	if BTsets.RemoteDownloader != want {
		t.Fatalf("RemoteDownloader = %+v, want %+v", BTsets.RemoteDownloader, want)
	}
}

func TestLoadBTSetsUpgradesMissingRemoteDownloader(t *testing.T) {
	withTestDB(t)
	tdb.Set("Settings", "BitTorr", []byte(`{"CacheSize":1048576,"ReaderReadAHead":50}`))
	BTsets = nil
	loadBTSets()
	if BTsets.RemoteDownloader.Type != "transmission" {
		t.Fatalf("Type = %q, want transmission", BTsets.RemoteDownloader.Type)
	}
	if BTsets.RemoteDownloader.URL != "" {
		t.Fatalf("URL = %q, want empty (disabled)", BTsets.RemoteDownloader.URL)
	}
	if BTsets.CacheSize != 1048576 {
		t.Fatalf("CacheSize = %d, existing values must be kept", BTsets.CacheSize)
	}
}

func TestLoadBTSetsKeepsRemoteDownloader(t *testing.T) {
	withTestDB(t)
	stored := RemoteDownloaderConfig{
		Type:        "transmission",
		URL:         "http://nas:9091/transmission/rpc",
		Username:    "u",
		Password:    "p",
		DownloadDir: "/mnt/dl",
	}
	buf, err := json.Marshal(map[string]any{"ReaderReadAHead": 50, "RemoteDownloader": stored})
	if err != nil {
		t.Fatal(err)
	}
	tdb.Set("Settings", "BitTorr", buf)
	BTsets = nil
	loadBTSets()
	if BTsets.RemoteDownloader != stored {
		t.Fatalf("RemoteDownloader = %+v, want %+v", BTsets.RemoteDownloader, stored)
	}
}

func TestLoadBTSetsCorruptFallsBackToDefaults(t *testing.T) {
	withTestDB(t)
	tdb.Set("Settings", "BitTorr", []byte(`{not json`))
	BTsets = nil
	loadBTSets()
	if BTsets == nil || BTsets.RemoteDownloader.Type != "transmission" {
		t.Fatalf("corrupt config must fall back to defaults, got %+v", BTsets)
	}
}

func TestSetBTSetsFillsEmptyRemoteType(t *testing.T) {
	withTestDB(t)
	SetBTSets(&BTSets{RemoteDownloader: RemoteDownloaderConfig{URL: "http://nas:9091/transmission/rpc"}})
	if BTsets.RemoteDownloader.Type != "transmission" {
		t.Fatalf("Type = %q, want transmission", BTsets.RemoteDownloader.Type)
	}
	if BTsets.RemoteDownloader.URL != "http://nas:9091/transmission/rpc" {
		t.Fatalf("URL lost: %q", BTsets.RemoteDownloader.URL)
	}
}

func TestSetBTSetsKeepsExplicitRemoteType(t *testing.T) {
	withTestDB(t)
	SetBTSets(&BTSets{RemoteDownloader: RemoteDownloaderConfig{Type: "other"}})
	if BTsets.RemoteDownloader.Type != "other" {
		t.Fatalf("Type = %q, an explicit type must not be overwritten", BTsets.RemoteDownloader.Type)
	}
}

// The settings JSON uses PascalCase keys like every other BTSets field; the
// web UI reads and writes them by these names.
func TestRemoteDownloaderJSONKeys(t *testing.T) {
	buf, err := json.Marshal(&BTSets{RemoteDownloader: RemoteDownloaderConfig{Type: "transmission", URL: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(buf, &raw); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, buf)
	}
	var rd map[string]any
	if err := json.Unmarshal(raw["RemoteDownloader"], &rd); err != nil {
		t.Fatalf("no RemoteDownloader object in %s: %v", buf, err)
	}
	for _, k := range []string{"Type", "URL", "Username", "Password", "DownloadDir"} {
		if _, ok := rd[k]; !ok {
			t.Errorf("RemoteDownloader.%s missing in %s", k, buf)
		}
	}
}

// log.Println("Client config:", settings.BTsets) in torr/btserver.go prints
// BTSets through String on every Connect; the password must not reach the log.
func TestBTSetsStringMasksRemotePassword(t *testing.T) {
	sets := &BTSets{RemoteDownloader: RemoteDownloaderConfig{Type: "transmission", URL: "http://nas:9091/transmission/rpc", Username: "admin", Password: "s3cret"}}
	out := fmt.Sprintln("Client config:", sets)
	if strings.Contains(out, "s3cret") {
		t.Fatalf("logged config leaks the password: %s", out)
	}
	if !strings.Contains(out, `"Password":"***"`) || !strings.Contains(out, `"Username":"admin"`) {
		t.Fatalf("logged config = %s, want masked password and the rest intact", out)
	}
	if sets.RemoteDownloader.Password != "s3cret" {
		t.Fatalf("String must not modify the settings, Password = %q", sets.RemoteDownloader.Password)
	}
	// settings get marshals BTSets directly and still returns the password.
	buf, err := json.Marshal(sets)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf), `"Password":"s3cret"`) {
		t.Fatalf("json.Marshal must keep the password for settings get: %s", buf)
	}
}

func TestBTSetsStringEmptyPassword(t *testing.T) {
	out := (&BTSets{}).String()
	if !strings.Contains(out, `"Password":""`) {
		t.Fatalf("empty password must stay empty, got %s", out)
	}
}
