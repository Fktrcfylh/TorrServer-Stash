package remotedl

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

const testHashHex = "0123456789abcdef0123456789abcdef01234567"

// rpcBody is the decoded request the fake received last.
type rpcBody struct {
	Method    string          `json:"method"`
	Arguments json.RawMessage `json:"arguments"`
}

func lastRequest(t *testing.T, f *fakeTransmission) (string, map[string]any) {
	t.Helper()
	f.mu.Lock()
	body := f.lastBody
	f.mu.Unlock()
	var req rpcBody
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body %q: %v", body, err)
	}
	args := map[string]any{}
	if len(req.Arguments) > 0 {
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			t.Fatalf("arguments %s: %v", req.Arguments, err)
		}
	}
	return req.Method, args
}

func replyWith(s string) func(w http.ResponseWriter, body []byte) {
	return func(w http.ResponseWriter, _ []byte) { io.WriteString(w, s) }
}

func TestAddMetainfo(t *testing.T) {
	f := &fakeTransmission{reply: replyWith(`{"result":"success","arguments":{"torrent-added":{"id":7,"name":"n","hashString":"` + testHashHex + `"}}}`)}
	c := newFake(t, f)
	meta := []byte("d4:infod4:name1:xee")

	res, err := c.Add(context.Background(), AddRequest{Metainfo: meta, DownloadDir: "/mnt/HD/dl"})
	if err != nil {
		t.Fatal(err)
	}
	if res != (AddResult{Hash: testHashHex, Name: "n"}) {
		t.Fatalf("result = %+v", res)
	}
	method, args := lastRequest(t, f)
	if method != "torrent-add" {
		t.Fatalf("method = %q", method)
	}
	got, err := base64.StdEncoding.DecodeString(args["metainfo"].(string))
	if err != nil || string(got) != string(meta) {
		t.Fatalf("metainfo = %v (%v), want base64 of %q", args["metainfo"], err, meta)
	}
	if args["download-dir"] != "/mnt/HD/dl" {
		t.Fatalf("download-dir = %v", args["download-dir"])
	}
	for _, k := range []string{"filename", "paused"} {
		if _, ok := args[k]; ok {
			t.Fatalf("unexpected %s in %v", k, args)
		}
	}
}

func TestAddMagnetWithoutDownloadDir(t *testing.T) {
	f := &fakeTransmission{reply: replyWith(`{"result":"success","arguments":{"torrent-added":{"id":1,"name":"m","hashString":"` + testHashHex + `"}}}`)}
	c := newFake(t, f)
	magnet := "magnet:?xt=urn:btih:" + testHashHex

	if _, err := c.Add(context.Background(), AddRequest{Magnet: magnet}); err != nil {
		t.Fatal(err)
	}
	_, args := lastRequest(t, f)
	if args["filename"] != magnet {
		t.Fatalf("filename = %v", args["filename"])
	}
	for _, k := range []string{"metainfo", "download-dir"} {
		if _, ok := args[k]; ok {
			t.Fatalf("unexpected %s in %v", k, args)
		}
	}
}

func TestAddDuplicateIsSuccess(t *testing.T) {
	f := &fakeTransmission{reply: replyWith(`{"result":"success","arguments":{"torrent-duplicate":{"id":3,"name":"d","hashString":"` + testHashHex + `"}}}`)}
	c := newFake(t, f)

	res, err := c.Add(context.Background(), AddRequest{Magnet: "magnet:?xt=urn:btih:" + testHashHex})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Duplicate || res.Hash != testHashHex || res.Name != "d" {
		t.Fatalf("result = %+v", res)
	}
}

func TestAddWithoutAddedOrDuplicateFails(t *testing.T) {
	f := &fakeTransmission{reply: replyWith(`{"result":"success","arguments":{}}`)}
	c := newFake(t, f)
	if _, err := c.Add(context.Background(), AddRequest{Magnet: "magnet:?xt=urn:btih:" + testHashHex}); err == nil {
		t.Fatal("want error for a reply without torrent-added/torrent-duplicate")
	}
}

func TestAddRPCErrorResult(t *testing.T) {
	f := &fakeTransmission{reply: replyWith(`{"result":"invalid or corrupt torrent file","arguments":{}}`)}
	c := newFake(t, f)
	_, err := c.Add(context.Background(), AddRequest{Metainfo: []byte("x")})
	if err == nil || !strings.Contains(err.Error(), "invalid or corrupt torrent file") {
		t.Fatalf("err = %v", err)
	}
}

func TestAddNeedsExactlyOnePayload(t *testing.T) {
	f := &fakeTransmission{}
	c := newFake(t, f)
	for _, req := range []AddRequest{{}, {Metainfo: []byte("x"), Magnet: "magnet:?"}} {
		if _, err := c.Add(context.Background(), req); err == nil {
			t.Fatalf("want error for %+v", req)
		}
	}
	if got := f.requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestRemoveSendsOneHashWithData(t *testing.T) {
	f := &fakeTransmission{reply: replyWith(`{"result":"success","arguments":{}}`)}
	c := newFake(t, f)

	if err := c.Remove(context.Background(), strings.ToUpper(testHashHex)); err != nil {
		t.Fatal(err)
	}
	method, args := lastRequest(t, f)
	if method != "torrent-remove" {
		t.Fatalf("method = %q", method)
	}
	ids, ok := args["ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != testHashHex {
		t.Fatalf("ids = %v, want [%s]", args["ids"], testHashHex)
	}
	if args["delete-local-data"] != true {
		t.Fatalf("delete-local-data = %v", args["delete-local-data"])
	}
}

// An empty or missing ids list removes every torrent in Transmission, so no
// request may leave for anything that is not one 40-hex hash.
func TestRemoveRefusesInvalidHashWithoutRequest(t *testing.T) {
	f := &fakeTransmission{reply: replyWith(`{"result":"success","arguments":{}}`)}
	c := newFake(t, f)
	for _, h := range []string{"", " ", testHashHex[:39], testHashHex + "0", "g123456789abcdef0123456789abcdef01234567", testHashHex[:39] + " "} {
		if err := c.Remove(context.Background(), h); err == nil {
			t.Fatalf("hash %q: want error", h)
		}
	}
	if got := f.requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestRemoveRPCError(t *testing.T) {
	f := &fakeTransmission{reply: func(w http.ResponseWriter, _ []byte) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	}}
	c := newFake(t, f)
	err := c.Remove(context.Background(), testHashHex)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v", err)
	}
}
