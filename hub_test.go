package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	testID     = "0123456789abcdef"
	testSecret = "0123456789abcdef0123456789abcdef"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.db.Close() })
	srv := httptest.NewServer(routes(newHub(store)))
	t.Cleanup(srv.Close)
	return srv
}

func auth(id, secret string) http.Header {
	return http.Header{"Authorization": {"Board " + id + ":" + secret}}
}

func dial(t *testing.T, srv *httptest.Server, path, secret string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + path
	c, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: auth(testID, secret)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func request(t *testing.T, srv *httptest.Server, method, path, secret, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header = auth(testID, secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func read(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(data, &out)
	return out
}

// readBoard returns the next message to the board that is not an app count.
func readBoard(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	for {
		msg := read(t, c)
		if _, ok := msg["APPS"]; !ok {
			return msg
		}
	}
}

func TestRelay(t *testing.T) {
	srv := newTestServer(t)
	ctx := context.Background()

	// Apps cannot register a board.
	if code, _ := request(t, srv, "GET", "/v1/status", testSecret, ""); code != http.StatusForbidden {
		t.Fatalf("unknown board: got %d", code)
	}

	board := dial(t, srv, "/v1/board", testSecret)
	if got := read(t, board); got["APPS"] != 0.0 {
		t.Fatalf("app count: %v", got)
	}
	app := dial(t, srv, "/v1/app", testSecret)
	if got := read(t, app); got["ONLINE"] != true {
		t.Fatalf("snapshot: %v", got)
	}
	if got := read(t, board); got["APPS"] != 1.0 {
		t.Fatalf("app count: %v", got)
	}

	// Status packets are merged and passed on.
	board.Write(ctx, websocket.MessageText, []byte(`{"STAT":"R","PROG":10}`))
	board.Write(ctx, websocket.MessageText, []byte(`{"PROG":42}`))
	read(t, app)
	if got := read(t, app); got["PROG"] != 42.0 {
		t.Fatalf("app update: %v", got)
	}
	_, status := request(t, srv, "GET", "/v1/status", testSecret, "")
	if status["STAT"] != "R" || status["PROG"] != 42.0 || status["ONLINE"] != true {
		t.Fatalf("status: %v", status)
	}

	// Commands reach the board, over HTTP and over the app's WebSocket.
	if code, _ := request(t, srv, "POST", "/v1/command", testSecret, `{"CMD":"PAUSE"}`); code != http.StatusAccepted {
		t.Fatalf("command: got %d", code)
	}
	if got := readBoard(t, board); got["CMD"] != "PAUSE" {
		t.Fatalf("board got %v", got)
	}
	app.Write(ctx, websocket.MessageText, []byte(`{"SET":{"SPD":80}}`))
	if got := readBoard(t, board); got["SET"] == nil {
		t.Fatalf("board got %v", got)
	}
	if code, _ := request(t, srv, "POST", "/v1/command", testSecret, `{"FOO":1}`); code != http.StatusBadRequest {
		t.Fatalf("invalid command: got %d", code)
	}

	// A wrong secret is rejected, for the board as well.
	if code, _ := request(t, srv, "GET", "/v1/status", "ffffffffffffffffffffffffffffffff", ""); code != http.StatusForbidden {
		t.Fatalf("wrong secret: got %d", code)
	}

	// Once the board is gone, apps see it offline and the last status stays.
	board.Close(websocket.StatusNormalClosure, "")
	if got := read(t, app); got["ONLINE"] != false {
		t.Fatalf("offline: %v", got)
	}
	if code, _ := request(t, srv, "POST", "/v1/command", testSecret, `{"CMD":"PAUSE"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("offline command: got %d", code)
	}
	app.Close(websocket.StatusNormalClosure, "")
	time.Sleep(100 * time.Millisecond)
	_, status = request(t, srv, "GET", "/v1/status", testSecret, "")
	if status["PROG"] != 42.0 || status["ONLINE"] != false {
		t.Fatalf("stored status: %v", status)
	}
}

func TestRekey(t *testing.T) {
	srv := newTestServer(t)
	newSecret := "fedcba9876543210fedcba9876543210"
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/board"
	dialBoard := func(secret string) error {
		c, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: auth(testID, secret)})
		if err == nil {
			c.CloseNow()
		}
		return err
	}

	if err := dialBoard(testSecret); err != nil {
		t.Fatal(err)
	}
	// After a USB flash the board comes back with the same ID and a new secret.
	if dialBoard(newSecret) == nil {
		t.Fatal("board with new secret accepted before rekey")
	}
	if code, _ := request(t, srv, "POST", "/v1/rekey", testSecret, `{"SECRET":"nope"}`); code != http.StatusBadRequest {
		t.Fatalf("invalid secret: got %d", code)
	}
	if code, _ := request(t, srv, "POST", "/v1/rekey", testSecret, `{"SECRET":"`+newSecret+`"}`); code != http.StatusNoContent {
		t.Fatalf("rekey: got %d", code)
	}
	if err := dialBoard(newSecret); err != nil {
		t.Fatalf("board after rekey: %v", err)
	}
	if code, _ := request(t, srv, "GET", "/v1/status", testSecret, ""); code != http.StatusForbidden {
		t.Fatalf("old secret: got %d", code)
	}
}
