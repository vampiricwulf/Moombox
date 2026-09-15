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
		// Fix round 1, finding 3: the spec's contract is 0 < ContentLength <=
		// capBytes, not the brief's stricter < capBytes — declared == cap
		// (the common 206-chunk case, ContentLength == end-start+1 == the
		// requested range) must take the sized path, not fall back to
		// io.ReadAll. This value-only case can't distinguish the two paths
		// by itself (both correctly return len(payload) bytes here); see
		// TestReadBodySizedPathIncludesTheBoundary for the allocation pin
		// that actually kills the `n >= capBytes` mutant.
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

// TestReadBodySizedPathIncludesTheBoundary pins fix round 1's finding 3: the
// contract is 0 < ContentLength <= capBytes (the design spec), not the
// brief's stricter < capBytes — a declared length exactly AT the cap (the
// common 206-chunk case, where ContentLength == end-start+1 == the
// requested range size) must still take the sized, pre-allocating path.
//
// A byte-length assertion can't tell the sized and unsized paths apart at
// this exact boundary — both correctly return every byte when the body
// doesn't exceed the cap — so, like TestReadBodySizedPathAllocatesOnce, the
// pin is an allocation count.
//
// Mutant this kills: `n >= capBytes` in place of `n > capBytes` pushes a
// declared-length-equals-cap body onto the unsized io.ReadAll fallback —
// same bytes returned, but the 512-byte-start growth ladder instead of one
// allocation.
func TestReadBodySizedPathIncludesTheBoundary(t *testing.T) {
	payload := bytes.Repeat([]byte("b"), 8<<10)
	body := &replayBody{r: bytes.NewReader(payload)}

	atBoundary := &http.Response{ContentLength: int64(len(payload)), Body: body}
	boundaryAllocs := testing.AllocsPerRun(100, func() {
		body.r.Reset(payload)
		readBodySink, _ = readBody(atBoundary, int64(len(payload))) // capBytes == ContentLength
	})
	if boundaryAllocs > 1 {
		t.Errorf("declared == cap allocs/op = %v, want <= 1 — did the boundary fall back to the unsized path?", boundaryAllocs)
	}
}

// TestReadBodyContentLengthBoundaryAndUnderstatement covers fix round 1's
// findings 1 and 3(b): cases where the real body doesn't match what
// Content-Length declared.
func TestReadBodyContentLengthBoundaryAndUnderstatement(t *testing.T) {
	for _, tc := range []struct {
		name       string
		contentLen int64
		actualLen  int
		capBytes   int64
		wantLen    int
	}{
		// Finding 1(a). Mutant this kills: the post-loop fallback replaced
		// by `return buf, nil` (or anything that stops at the declared
		// length) truncates this to 10 bytes instead of the real 100-byte
		// body — every mutant that deletes or short-circuits the "body
		// outran its declared Content-Length" branch fails this.
		{"declared understates a small body", 10, 100, 4096, 100},
		// Finding 1(b). Mutant this kills: same as above, but proves the
		// cap still binds even though the declared length was wrong — a
		// fallback that reads unboundedly instead of capBytes-len(buf)
		// would return 4990 bytes instead of 4096.
		{"declared understates a body larger than the cap", 10, 5000, 4096, 4096},
		// Finding 3(b). Mutant this kills: probing n+1 bytes without
		// clamping to capBytes when n == capBytes reads 101 bytes into the
		// buffer, and the old `capBytes - len(buf)` fallback math then goes
		// negative — io.LimitReader treats N<=0 as an immediate EOF, so the
		// bug silently RETURNS 101 bytes for a 100-byte cap instead of
		// erroring, and only a length assertion here catches it.
		{"declared equals cap, body has more", 100, 5000, 100, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("u"), tc.actualLen)
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

// TestFetchSegmentReusesConnection is the keep-alive regression pin for
// readBody's sized path.
//
// Fix round 1 tested the hypothesis (from the original brief) that a read
// stopping exactly at Content-Length defeats connection reuse. Experiment:
// swap the sized path for a bare `buf := make([]byte, n);
// io.ReadFull(resp.Body, buf)` and run this test — it still passes (conns ==
// 1). net/http's body wrapper surfaces io.EOF on the same Read call that
// drains a body to its own declared Content-Length (transfer.go's
// readLocked piggybacks EOF once its internal io.LimitedReader hits N==0),
// so an exact-length read reuses the connection too; the result buffer's
// extra byte of capacity was never load-bearing for keep-alive (see
// readBody's doc comment for what it's actually for).
//
// This test still earns its place as a regression pin: it fails on any
// change that reads past the connection without draining the declared
// Content-Length (e.g. sizing the read/buffer to capBytes instead of the
// declared length, so extra unread bytes are left on the socket).
//
// Mutant this kills: a sized-path implementation that leaves bytes of the
// declared Content-Length unread on the connection when it returns (conns
// becomes 2).
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
