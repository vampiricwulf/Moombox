package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// countingListener counts the bytes the server writes to its connections, so
// a test can see what a frame cost on the wire.
type countingListener struct {
	net.Listener
	written *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: c, written: l.written}, nil
}

type countingConn struct {
	net.Conn
	written *atomic.Int64
}

func (c countingConn) Write(p []byte) (int, error) {
	// Counted before the write: the reader on the other side can have the
	// bytes before this goroutine runs again.
	c.written.Add(int64(len(p)))
	return c.Conn.Write(p)
}

// compressionRows is a jobs payload of about 250 KB whose text varies from
// row to row, standing in for a list of job rows with descriptions.
func compressionRows() []map[string]any {
	rows := make([]map[string]any, 0, 200)
	for i := range 200 {
		var b strings.Builder
		for w := 0; b.Len() < 1200; w++ {
			fmt.Fprintf(&b, "word%d stream%d https://example.com/%d/%d ", (i*7+w*13)%257, (i+w)%91, i, w)
		}
		rows = append(rows, map[string]any{
			"id": fmt.Sprintf("job-%04d", i), "status": "Finished", "title": fmt.Sprintf("Title %d", i),
			"description": b.String(),
		})
	}
	return rows
}

// compressionFixture is a hub behind a server whose written bytes are
// counted, with compressionRows as its snapshot.
func compressionFixture(t *testing.T) (hub *WebSocketHub, url string, written *atomic.Int64, snapshot []byte) {
	t.Helper()
	hub = NewWebSocketHub(testWSLogger{})
	rows := compressionRows()
	hub.InitialState = func() map[string]any { return map[string]any{"jobs": rows} }
	snapshot, err := json.Marshal(WSMessage{Type: "initial_state", Payload: map[string]any{"jobs": rows}})
	if err != nil {
		t.Fatal(err)
	}
	written = &atomic.Int64{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(hub.HandleUpgrade))
	srv.Listener = countingListener{Listener: srv.Listener, written: written}
	srv.Start()
	t.Cleanup(func() { srv.Close(); hub.Close() })
	return hub, "ws" + strings.TrimPrefix(srv.URL, "http"), written, snapshot
}

// onlyClient is the hub's one connected client.
func onlyClient(t *testing.T, hub *WebSocketHub) *wsClient {
	t.Helper()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.clients) != 1 {
		t.Fatalf("%d clients, want 1", len(hub.clients))
	}
	for c := range hub.clients {
		return c
	}
	return nil
}

// TestWebSocketCompressesLargeFramesForAClientThatOffersIt is finding web-9
// under owner ruling R-B4: the snapshot a dashboard is sent on every connect
// and every resync is megabytes of mostly text, and went out raw. A client
// that offers permessage-deflate now gets it; a client that does not, or
// that offers parameters the library cannot honour, is served exactly as
// before. In every case the frames carry the same bytes — compression
// changes what is on the wire, never what a frame contains — and the
// connect, broadcast, ping and resync paths all deliver in order.
//
// Mutants: CompressionMode left out of Accept (the offering client is
// answered with no extension and the snapshot costs its full size); a
// threshold of zero or one (the small frame is deflated too: the library
// then applies its own default, and TestProgressFramesStayUnderThe-
// CompressionThreshold in cmd/moombox pins the constant).
func TestWebSocketCompressesLargeFramesForAClientThatOffersIt(t *testing.T) {
	cases := []struct {
		name       string
		opts       *websocket.DialOptions
		compressed bool
	}{
		{"offers permessage-deflate", &websocket.DialOptions{CompressionMode: websocket.CompressionNoContextTakeover}, true},
		{"offers context takeover", &websocket.DialOptions{CompressionMode: websocket.CompressionContextTakeover}, true},
		{"offers nothing", nil, false},
		// A window the library cannot produce: the offer is declined, not
		// half-honoured, and the client is served uncompressed.
		{"offers a parameter the server cannot honour", &websocket.DialOptions{HTTPHeader: http.Header{
			"Sec-WebSocket-Extensions": {"permessage-deflate; server_max_window_bits=10"},
		}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, url, written, snapshot := compressionFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			conn, resp, err := websocket.Dial(ctx, url, tc.opts)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.CloseNow()
			conn.SetReadLimit(-1)
			ext := resp.Header.Get("Sec-WebSocket-Extensions")
			if got := strings.Contains(ext, "permessage-deflate"); got != tc.compressed {
				t.Fatalf("negotiated extensions %q, want permessage-deflate = %v", ext, tc.compressed)
			}
			if tc.compressed && !strings.Contains(ext, "server_no_context_takeover") {
				t.Errorf("negotiated %q: the server must not keep a compressor per idle dashboard", ext)
			}

			read := func(what string) []byte {
				t.Helper()
				_, b, err := conn.Read(ctx)
				if err != nil {
					t.Fatalf("read %s: %v", what, err)
				}
				return b
			}

			// Connect: the snapshot, byte for byte.
			before := written.Load()
			if got := read("initial_state"); string(got) != string(snapshot) {
				t.Fatalf("initial_state arrived as %d bytes that are not the %d-byte snapshot", len(got), len(snapshot))
			}
			wire := written.Load() - before
			if tc.compressed && wire*2 > int64(len(snapshot)) {
				t.Errorf("the %d-byte snapshot cost %d bytes on the wire, want under half", len(snapshot), wire)
			}
			if !tc.compressed && wire < int64(len(snapshot)) {
				t.Errorf("the %d-byte snapshot cost %d bytes on the wire for a client that negotiated no compression", len(snapshot), wire)
			}

			// A frame under the threshold goes out as it is, whatever was
			// negotiated: the ~60 Hz progress frame must never be deflated.
			small, _ := json.Marshal(WSMessage{Type: "job_progress", Payload: map[string]any{"id": "job-0001", "percent": 12.5}})
			if len(small) >= WSCompressionThreshold {
				t.Fatalf("fixture: the small frame is %d bytes", len(small))
			}
			before = written.Load()
			hub.Broadcast("job_progress", map[string]any{"id": "job-0001", "percent": 12.5})
			if got := read("job_progress"); string(got) != string(small) {
				t.Fatalf("job_progress = %s, want %s", got, small)
			}
			if wire := written.Load() - before; wire < int64(len(small)) {
				t.Errorf("a %d-byte frame under the threshold cost %d bytes: it was compressed", len(small), wire)
			}

			// A whole-row frame, then the next small one: order holds across
			// a compressed and an uncompressed write.
			row := compressionRows()[7]
			big, _ := json.Marshal(WSMessage{Type: "job_update", Payload: row})
			hub.Broadcast("job_update", row)
			hub.Broadcast("job_deleted", map[string]any{"id": "job-0007"})
			if got := read("job_update"); string(got) != string(big) {
				t.Fatalf("job_update arrived changed (%d bytes, want %d)", len(got), len(big))
			}
			if got := read("job_deleted"); !strings.Contains(string(got), `"job_deleted"`) {
				t.Fatalf("frame after job_update = %s, want job_deleted", got)
			}

			// The client's own ping still reads under the 4 KiB read limit,
			// and the pong comes back.
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
				t.Fatalf("write ping: %v", err)
			}
			if got := read("pong"); !strings.Contains(string(got), `"pong"`) {
				t.Fatalf("answer to ping = %s", got)
			}

			// Resync: a client that dropped a frame is sent the snapshot in
			// place of its next frame, and it arrives whole.
			client := onlyClient(t, hub)
			client.lastResync.Store(0)
			client.needsResync.Store(true)
			hub.Broadcast("log", "after the drop")
			if got := read("resync snapshot"); string(got) != string(snapshot) {
				t.Fatalf("the resync frame is %d bytes that are not the %d-byte snapshot", len(got), len(snapshot))
			}

			if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
				t.Errorf("close: %v", err)
			}
		})
	}
}

// TestWebSocketReconnectsCompressed: a dashboard reconnects for as long as it
// is open, and each connection negotiates on its own. Twenty connects in a
// row, alternating clients that offer compression and clients that do not,
// each get the whole snapshot, and the pooled compressors are handed back:
// nothing one connection did is left for the next to trip on.
func TestWebSocketReconnectsCompressed(t *testing.T) {
	_, url, _, snapshot := compressionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := range 20 {
		var opts *websocket.DialOptions
		if i%2 == 0 {
			opts = &websocket.DialOptions{CompressionMode: websocket.CompressionNoContextTakeover}
		}
		conn, _, err := websocket.Dial(ctx, url, opts)
		if err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		conn.SetReadLimit(-1)
		_, b, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("connect %d: read: %v", i, err)
		}
		if string(b) != string(snapshot) {
			t.Fatalf("connect %d: the snapshot arrived changed", i)
		}
		if i%3 == 0 {
			conn.CloseNow() // a tab that vanished
		} else {
			conn.Close(websocket.StatusNormalClosure, "")
		}
	}
}
