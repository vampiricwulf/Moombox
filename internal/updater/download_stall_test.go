package updater

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/httpx/httpxtest"
)

// The stall tests run inside a synctest bubble, where time.AfterFunc (the
// stall timer) and the trickle handler's time.After share a fake clock that
// moves only once every goroutine in the bubble is blocked. Both ends of the
// download are in the bubble too: the requests go through
// httpxtest.NewClient, which serves the handler over an io.Pipe instead of a
// socket, since a goroutine waiting on the network would keep the clock from
// moving. No pause of the process — a loaded runner, a scanner, the race
// detector — can then come between a chunk and the timer.
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
// it: httpxtest.NewClient answers every request itself.
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
	downloadClient = httpxtest.NewClient(h)
	t.Cleanup(func() { downloadClient = orig })
}

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
