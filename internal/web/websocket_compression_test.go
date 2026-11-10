package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestWebSocketDoesNotNegotiateCompression holds permessage-deflate off
// (finding web-9, owner ruling R-B4). The initial_state snapshot is large
// and mostly text (TestInitialStateSnapshotSize, cmd/moombox), and
// compression is the one remedy the ruling permits, but only where it is
// clearly safe with the existing clients. It was tried against Chromium and
// Node and never against Safari or Firefox, so it is not enabled: a client
// that offers the extension is answered without it and reads the snapshot
// byte for byte as the hub marshalled it.
//
// Enabling CompressionMode on websocket.Accept fails this test on purpose.
// Change it together with the browser check the ruling asks for: the
// negotiated extension, an intact initial_state at a realistic row count, a
// forced resync and a reconnect, in Safari and Firefox as well.
func TestWebSocketDoesNotNegotiateCompression(t *testing.T) {
	hub := NewWebSocketHub(testWSLogger{})
	rows := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		rows = append(rows, map[string]any{
			"id":          i,
			"title":       "A stream title that repeats and would deflate well",
			"description": strings.Repeat("welcome to the stream, thank you for watching. ", 40),
		})
	}
	hub.InitialState = func() map[string]any { return map[string]any{"jobs": rows} }
	want, err := json.Marshal(WSMessage{Type: "initial_state", Payload: map[string]any{"jobs": rows}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() { srv.Close(); hub.Close() })
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	for _, mode := range []websocket.CompressionMode{websocket.CompressionNoContextTakeover, websocket.CompressionContextTakeover} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{CompressionMode: mode})
		if err != nil {
			cancel()
			t.Fatalf("dial: %v", err)
		}
		if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
			t.Errorf("the hub accepted %q: compression is on without the browser check R-B4 asks for", ext)
		}
		conn.SetReadLimit(-1)
		_, got, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read initial_state: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("the snapshot a client that offers compression reads is not the frame the hub built (%d bytes, want %d)", len(got), len(want))
		}
		conn.CloseNow()
		cancel()
	}
}
