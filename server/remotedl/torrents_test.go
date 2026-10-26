package remotedl

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
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

// hashN is a distinct valid info hash per n (0..15).
func hashN(n int) string {
	return strings.Repeat(string("0123456789abcdef"[n]), 40)
}

func TestStatusMapsStatesInOneBatch(t *testing.T) {
	torrent := func(n, status int, done float64, errCode int, errStr string) string {
		return fmt.Sprintf(`{"hashString":"%s","status":%d,"percentDone":%v,"error":%d,"errorString":"%s"}`, hashN(n), status, done, errCode, errStr)
	}
	list := []string{
		torrent(0, 0, 0.5, 0, ""),
		torrent(1, 1, 0, 0, ""),
		torrent(2, 2, 0.1, 0, ""),
		torrent(3, 3, 0, 0, ""),
		torrent(4, 4, 0.12345, 0, ""),
		torrent(5, 5, 1, 0, ""),
		torrent(6, 6, 1, 0, ""),
		torrent(7, 4, 0.3, 1, "tracker warning"),
		torrent(8, 4, 0.3, 2, "tracker gone"),
		torrent(9, 4, 0.3, 3, "No space left on device"),
		torrent(10, 9, 0.2, 0, ""),
		torrent(12, 6, 1, 0, ""), // not asked: ignored
	}
	f := &fakeTransmission{reply: replyWith(`{"result":"success","arguments":{"torrents":[` + strings.Join(list, ",") + `]}}`)}
	c := newFake(t, f)

	asked := []string{strings.ToUpper(hashN(10))}
	for n := 0; n <= 11; n++ {
		if n != 10 {
			asked = append(asked, hashN(n))
		}
	}
	asked = append(asked, hashN(1)) // duplicate: sent once
	got, err := c.Status(context.Background(), asked)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]RemoteStatus{
		hashN(0):  {State: StateStopped, Progress: 50},
		hashN(1):  {State: StateChecking},
		hashN(2):  {State: StateChecking, Progress: 10},
		hashN(3):  {State: StateQueued},
		hashN(4):  {State: StateDownloading, Progress: 12.3},
		hashN(5):  {State: StateQueued, Progress: 100},
		hashN(6):  {State: StateSeeding, Progress: 100},
		hashN(7):  {State: StateDownloading, Progress: 30, Error: "tracker warning"},
		hashN(8):  {State: StateDownloading, Progress: 30, Error: "tracker gone"},
		hashN(9):  {State: StateError, Progress: 30, Error: "No space left on device"},
		hashN(10): {State: StateError, Progress: 20, Error: "unknown transmission status 9"},
		hashN(11): {State: StateMissing},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status =\n%+v\nwant\n%+v", got, want)
	}
	// one handshake 409 plus exactly one torrent-get
	if n := f.requests.Load(); n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
	method, args := lastRequest(t, f)
	if method != "torrent-get" {
		t.Fatalf("method = %q", method)
	}
	ids, _ := args["ids"].([]any)
	if len(ids) != 12 || ids[0] != hashN(10) {
		t.Fatalf("ids = %v, want 12 lowercase unique hashes", args["ids"])
	}
	if !reflect.DeepEqual(args["fields"], []any{"hashString", "status", "percentDone", "error", "errorString"}) {
		t.Fatalf("fields = %v", args["fields"])
	}
}

// Without ids torrent-get would list every torrent of the client.
func TestStatusEmptyOrInvalidHashesSendNoRequest(t *testing.T) {
	f := &fakeTransmission{}
	c := newFake(t, f)

	got, err := c.Status(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: got %v, %v", got, err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 40)} {
		if _, err := c.Status(context.Background(), []string{testHashHex, bad}); err == nil {
			t.Fatalf("hash %q accepted", bad)
		}
	}
	if n := f.requests.Load(); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
}

func TestStatusRPCError(t *testing.T) {
	f := &fakeTransmission{reply: replyWith(`{"result":"no such method","arguments":{}}`)}
	c := newFake(t, f)

	_, err := c.Status(context.Background(), []string{testHashHex})
	if err == nil || !strings.Contains(err.Error(), "torrent-get: no such method") {
		t.Fatalf("err = %v", err)
	}
}
