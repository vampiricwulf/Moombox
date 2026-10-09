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

	"github.com/vampiricwulf/Moombox/internal/logger"
)

// TestAConnectShowsEachLogLineOnce is W24-14, over a real socket with the
// production pieces: a real logger at DEBUG feeding the hub's own lines back
// through a SubscribeLines -> BroadcastLog forwarder (cmd/moombox's
// wireLogForwarding), and an initial_state that carries the ring and the
// number of its newest line (ws_wiring.go).
//
// HandleUpgrade registers the client before the snapshot reads the ring — the
// order that leaves no gap — and logs "websocket connected" in between, so
// that line is in the snapshot AND reaches the client as a log frame. The
// dashboard set its buffer to the snapshot and appended every frame, so the
// line showed twice on every connect. Each frame now carries the ring's number
// for its line and the snapshot the number of its newest; the dashboard skips
// a frame at or below it (web/tests/log-panel.test.mjs pins that half).
//
// Mutants this kills:
//   - BroadcastLog leaving Seq off the frame: no frame can be told apart.
//   - a frame numbered with anything but the ring's number (the forwarder
//     sending 0, or the logger's broadcast handing out another count): the
//     replayed line no longer sits at or below logSeq, or the new one above.
func TestAConnectShowsEachLogLineOnce(t *testing.T) {
	log, err := logger.New("", "DEBUG", 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	log.SuppressStdout()
	hub := NewWebSocketHub(log)
	hub.InitialState = func() map[string]any {
		logs, logSeq := log.RecentLines()
		return map[string]any{"jobs": []any{}, "logs": logs, "logSeq": logSeq}
	}
	sub := log.SubscribeLines()
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("forwarder panicked: %v", r)
			}
		}()
		for {
			select {
			case <-done:
				return
			case line, ok := <-sub:
				if !ok {
					return
				}
				hub.BroadcastLog(line.Text, line.Seq)
			}
		}
	}()
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(func() {
		srv.Close()
		hub.Close()
		close(done)
		log.UnsubscribeLines(sub)
		log.Close()
	})

	log.Info("before the connect")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	type frame struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
		Seq     *uint64         `json:"seq"`
	}
	read := func() frame {
		t.Helper()
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
		return f
	}

	first := read()
	if first.Type != "initial_state" {
		t.Fatalf("first frame is %q, want initial_state", first.Type)
	}
	var snap struct {
		Logs   []string `json:"logs"`
		LogSeq *uint64  `json:"logSeq"`
	}
	if err := json.Unmarshal(first.Payload, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.LogSeq == nil {
		t.Fatal("initial_state carries no logSeq")
	}
	inSnapshot := func(line string) bool {
		for _, l := range snap.Logs {
			if l == line {
				return true
			}
		}
		return false
	}
	if !strings.Contains(strings.Join(snap.Logs, "\n"), "websocket connected") {
		t.Fatalf("fixture: the hub's own connect line is not in the snapshot: %q", snap.Logs)
	}

	// A line the snapshot cannot hold: logged after it was delivered.
	log.Info("after the connect")

	// The dashboard's rule, applied to every frame that follows.
	view := append([]string(nil), snap.Logs...)
	var replayed bool
	for {
		f := read()
		if f.Type != "log" {
			continue
		}
		var text string
		if err := json.Unmarshal(f.Payload, &text); err != nil {
			t.Fatalf("a log frame's payload is not a string (a tab still on the previous app.js reads it as one): %s", f.Payload)
		}
		if f.Seq == nil {
			t.Fatalf("log frame %q carries no seq", text)
		}
		if inSnapshot(text) {
			replayed = true
			if *f.Seq > *snap.LogSeq {
				t.Errorf("%q is in the snapshot but arrived numbered %d, above its logSeq %d — the dashboard shows it twice",
					text, *f.Seq, *snap.LogSeq)
			}
		}
		if *f.Seq > *snap.LogSeq {
			view = append(view, text)
		}
		if strings.Contains(text, "after the connect") {
			if *f.Seq <= *snap.LogSeq {
				t.Errorf("a line logged after the snapshot arrived numbered %d, at or below its logSeq %d — the dashboard drops it",
					*f.Seq, *snap.LogSeq)
			}
			break
		}
	}
	if !replayed {
		t.Error("fixture: the connect line never came back as a frame, so nothing here was at risk of showing twice")
	}

	seen := map[string]int{}
	for _, l := range view {
		seen[l]++
		if seen[l] == 2 {
			t.Errorf("the dashboard's view shows %q twice: %q", l, view)
		}
	}
	if !strings.Contains(strings.Join(view, "\n"), "after the connect") {
		t.Errorf("the line logged after the connect is missing from the view: %q", view)
	}
}
