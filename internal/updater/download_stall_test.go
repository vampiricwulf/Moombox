package updater

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The stall tests run inside a synctest bubble, where time.AfterFunc (the
// stall timer) and the trickle handler's time.After share a fake clock that
// moves only once every goroutine in the bubble is blocked. Both ends of the
// download are in the bubble too: the requests go through inMemoryClient,
// which serves the handler over an io.Pipe instead of a socket, since a
// goroutine waiting on the network would keep the clock from moving. No
// pause of the process — a loaded runner, a scanner, the race detector —
// can then come between a chunk and the timer.
//
// They used to run against an httptest server on the wall clock. Data every
// 50 ms against a 200 ms stall timer was cut by a 160 ms pause of the
// process, and against a 1 s timer it was still cut on a Windows runner.

// shortenStall scales the stall timeout down for a test.
func shortenStall(t *testing.T, d time.Duration) {
	t.Helper()
	orig := downloadStallTimeout
	downloadStallTimeout = d
	t.Cleanup(func() { downloadStallTimeout = orig })
}

// trickleOrigin is the origin trickleMux is served under. Nothing resolves
// it: inMemoryClient answers every request itself.
const trickleOrigin = "http://updates.test"

// trickleMux serves /exe as chunks every gap — chunks of them, then holds
// the connection open for hold — and /sig as a well-formed signature.
func trickleMux(t *testing.T, chunks int, gap, hold time.Duration) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/exe", func(rw http.ResponseWriter, r *http.Request) {
		f := rw.(http.Flusher)
		chunk := make([]byte, 4<<10)
		chunk[0] = 0x7f
		for range chunks {
			if _, err := rw.Write(chunk); err != nil {
				return
			}
			f.Flush()
			chunk[0] = 0
			select {
			case <-time.After(gap):
			case <-r.Context().Done():
				return
			}
		}
		select {
		case <-time.After(hold):
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/sig", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Write(make([]byte, ed25519.SignatureSize))
	})
	// The bytes /exe sends: chunks of 4 KiB, the first opening with 0x7f.
	body := make([]byte, chunks*(4<<10))
	if chunks > 0 {
		body[0] = 0x7f
	}
	serveManifest(mux, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", body)))
	return mux
}

func trickleRelease() *ReleaseInfo {
	return &ReleaseInfo{
		Version: "2.0.0", TagName: "v2.0.0",
		DownloadURL: trickleOrigin + "/exe", SignatureURL: trickleOrigin + "/sig",
		ManifestURL: trickleOrigin + "/manifest", ManifestSignatureURL: trickleOrigin + "/manifest.sig",
	}
}

// serveInMemory points downloadClient at h for the rest of the test.
func serveInMemory(t *testing.T, h http.Handler) {
	t.Helper()
	orig := downloadClient
	downloadClient = inMemoryClient(h)
	t.Cleanup(func() { downloadClient = orig })
}

// inMemoryClient is an http.Client whose every request is answered by h in
// the caller's process, the response body streamed through an io.Pipe: each
// Write the handler makes is read by the client as it is made, as over a
// socket, and the request's cancellation reaches both ends. A pipe blocks on
// channels alone, which a synctest bubble can wait out; a socket it cannot.
func inMemoryClient(h http.Handler) *http.Client {
	return &http.Client{Transport: handlerTransport{h}}
}

type handlerTransport struct{ h http.Handler }

func (ht handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	w := &pipeResponseWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer func() {
			w.WriteHeader(http.StatusOK) // a handler that wrote nothing answers 200, as net/http does
			// A handler that returned because the request was cancelled must
			// not end the body cleanly: over a socket the client's read fails
			// once its request is cancelled, whatever the server does next.
			// A plain Close here could beat the watcher below and hand the
			// client an EOF instead.
			if err := req.Context().Err(); err != nil {
				pw.CloseWithError(err)
			} else {
				pw.Close()
			}
		}()
		ht.h.ServeHTTP(w, req)
	}()
	go func() {
		select {
		case <-req.Context().Done():
			pw.CloseWithError(req.Context().Err())
		case <-served:
		}
	}()
	select {
	case <-w.ready:
	case <-req.Context().Done():
		pr.Close()
		return nil, req.Context().Err()
	}
	return &http.Response{
		Status:        http.StatusText(w.status),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sent,
		Body:          pr,
		ContentLength: -1,
		Request:       req,
	}, nil
}

// pipeResponseWriter is the handler's side of handlerTransport. Only the
// handler's goroutine touches it until ready is closed; RoundTrip reads
// status and sent after that.
type pipeResponseWriter struct {
	header      http.Header
	sent        http.Header
	pw          *io.PipeWriter
	status      int
	wroteHeader bool
	ready       chan struct{}
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }

func (w *pipeResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.sent = w.header.Clone()
	close(w.ready)
}

func (w *pipeResponseWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(b)
}

// Flush is a no-op: every Write already waits for the client to read it.
func (w *pipeResponseWriter) Flush() {}

// A release binary is 78-87 MB, and the download used to carry a 5-minute
// TOTAL deadline: anything slower than about 2.3 Mbit/s was killed however
// steadily it arrived. Only a stall ends a download now. Scaled: data every
// 50 ms against a 1 s stall timeout, 30 chunks, so the transfer runs 1.5 s
// on the bubble's clock — past the timeout, which it can only outlast if
// every chunk reset the timer.
//
// Mutant: the stall timer not reset on progress — the download is cut at
// 1 s.
func TestASteadyDownloadOutlastsTheStallTimeout(t *testing.T) {
	shortenStall(t, time.Second)
	serveInMemory(t, trickleMux(t, 30, 50*time.Millisecond, 0))
	synctest.Test(t, func(t *testing.T) {
		u, exePath := newTestUpdaterAt(t, "1.0.0", trickleOrigin, nil)

		start := time.Now()
		if err := u.ApplyUpdate(context.Background(), trickleRelease()); err != nil {
			t.Fatalf("a steady download failed: %v", err)
		}
		if took := time.Since(start); took < 1500*time.Millisecond {
			t.Errorf("the update took %s on the bubble's clock, want the 30 chunks' 1.5 s: shorter does not outlast the 1 s timeout", took)
		}
		if fileIs(exePath, "current binary") {
			t.Error("the update was not placed")
		}
	})
}

// A download that stops receiving data is abandoned after the stall timeout,
// with an error that says so — not a bare "context canceled". Two chunks
// 10 ms apart, then nothing for 5 s: the timer last reset at 10 ms fires at
// 210 ms on the bubble's clock.
//
// Mutant: no stall timer — the download waits out the server's 5 s hold.
func TestAStalledDownloadIsAbandoned(t *testing.T) {
	shortenStall(t, 200*time.Millisecond)
	serveInMemory(t, trickleMux(t, 2, 10*time.Millisecond, 5*time.Second))
	synctest.Test(t, func(t *testing.T) {
		u, exePath := newTestUpdaterAt(t, "1.0.0", trickleOrigin, nil)

		start := time.Now()
		err := u.ApplyUpdate(context.Background(), trickleRelease())
		if err == nil || !strings.Contains(err.Error(), "download stalled") {
			t.Fatalf("ApplyUpdate = %v, want a stalled-download error", err)
		}
		if took := time.Since(start); took != 210*time.Millisecond {
			t.Errorf("abandoned after %s on the bubble's clock, want 210 ms: the 200 ms timeout after the last chunk", took)
		}
		if !fileIs(exePath, "current binary") {
			t.Error("a stalled download changed the running binary")
		}
	})
}

func fileIs(path, want string) bool {
	got, err := os.ReadFile(path)
	return err == nil && string(got) == want
}
