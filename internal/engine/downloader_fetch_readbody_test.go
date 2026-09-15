package engine

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// newConnCountingServer starts an HTTP/1.1 test server that counts the TCP
// connections opened to it. Used to prove keep-alive reuse: a response body
// that net/http never saw EOF on makes the client throw the socket away and
// dial again, which shows up here as a second StateNew.
func newConnCountingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

// replayBody is a rewindable io.ReadCloser for the allocation pin: the
// measured closure must not allocate anything of its own, so the reader is
// built once and Reset between runs.
type replayBody struct{ r *bytes.Reader }

func (b *replayBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *replayBody) Close() error               { return nil }

// readBodySink defeats an escape analysis that could stack-allocate the
// returned slice and make the allocation pin measure nothing.
var readBodySink []byte

// TestReadBodyShapes covers the three Content-Length shapes readBody must
// handle identically to today's io.ReadAll(io.LimitReader(...)).
//
// Mutant this kills: pre-sizing from a Content-Length larger than the cap
// (case "oversized") would allocate — and return — more than the caller's
// ceiling, defeating maxSegmentBodyBytes.
func TestReadBodyShapes(t *testing.T) {
	payload := bytes.Repeat([]byte("s"), 4096)
	for _, tc := range []struct {
		name       string
		contentLen int64
		capBytes   int64
		wantLen    int
	}{
		{"declared length", int64(len(payload)), maxSegmentBodyBytes, len(payload)},
		{"undeclared (chunked)", -1, maxSegmentBodyBytes, len(payload)},
		{"declared length equals the cap", int64(len(payload)), int64(len(payload)), len(payload)},
		{"oversized declaration is capped", int64(len(payload)), 100, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				ContentLength: tc.contentLen,
				Body:          io.NopCloser(bytes.NewReader(payload)),
			}
			got, err := readBody(resp, tc.capBytes)
			if err != nil {
				t.Fatalf("readBody: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tc.wantLen)
			}
			if !bytes.Equal(got, payload[:tc.wantLen]) {
				t.Error("bytes differ from the source payload")
			}
		})
	}
}

// TestReadBodySizedPathAllocatesOnce is the point of the change: a declared
// length must produce ONE allocation instead of io.ReadAll's 512-byte start
// and ~1.25x growth ladder (a 4 KB body climbs it several times; a 4 MB
// segment ~20 times, copying ~2x its own size in garbage).
//
// Mutant this kills: deleting the sized branch — the two numbers below then
// collapse onto each other and the strict inequality fails.
func TestReadBodySizedPathAllocatesOnce(t *testing.T) {
	payload := bytes.Repeat([]byte("s"), 8<<10)
	body := &replayBody{r: bytes.NewReader(payload)}

	sized := &http.Response{ContentLength: int64(len(payload)), Body: body}
	sizedAllocs := testing.AllocsPerRun(100, func() {
		body.r.Reset(payload)
		readBodySink, _ = readBody(sized, maxSegmentBodyBytes)
	})
	if sizedAllocs > 1 {
		t.Errorf("sized path allocs/op = %v, want <= 1 (the result buffer) — is the +1 EOF capacity forcing a regrow, or did pre-sizing regress?", sizedAllocs)
	}

	unsized := &http.Response{ContentLength: -1, Body: body}
	unsizedAllocs := testing.AllocsPerRun(100, func() {
		body.r.Reset(payload)
		readBodySink, _ = readBody(unsized, maxSegmentBodyBytes)
	})
	if unsizedAllocs <= sizedAllocs {
		t.Errorf("unsized allocs/op = %v, sized = %v — the sized path must allocate strictly fewer, or Content-Length is being ignored", unsizedAllocs, sizedAllocs)
	}
}

// TestFetchSegmentReusesConnection is the keep-alive pin. net/http returns a
// connection to the idle pool only after the body reaches its OWN io.EOF, so
// the natural io.ReadFull(make([]byte, n)) implementation of readBody — which
// stops exactly at Content-Length — would pay a fresh TCP handshake per
// segment across a whole recording.
//
// Mutant this kills: exactly that implementation (conns becomes 2).
func TestFetchSegmentReusesConnection(t *testing.T) {
	payload := strings.Repeat("m", 4096)
	srv, conns := newConnCountingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Explicit Content-Length: this is the sized path under test.
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		io.WriteString(w, payload)
	})

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: filepath.Join(t.TempDir(), "video.ts"),
	})
	for seq := range 2 {
		data, status, err := d.fetchSegment(t.Context(), d.buildSegmentURL(seq))
		if err != nil || status != http.StatusOK {
			t.Fatalf("fetchSegment(%d) = status %d, err %v", seq, status, err)
		}
		if len(data) != len(payload) {
			t.Fatalf("fetchSegment(%d) returned %d bytes, want %d", seq, len(data), len(payload))
		}
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("server saw %d connections for 2 segment fetches, want 1 — the sized read stopped before net/http observed the body's EOF, so the socket was discarded", got)
	}
}
