package updater

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// shortenStall scales the stall timeout down for a test.
func shortenStall(t *testing.T, d time.Duration) {
	t.Helper()
	orig := downloadStallTimeout
	downloadStallTimeout = d
	t.Cleanup(func() { downloadStallTimeout = orig })
}

// trickleServer serves /exe as chunks every gap — chunks of them, then holds
// the connection open for hold — and /sig as a well-formed signature.
func trickleServer(t *testing.T, chunks int, gap, hold time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/exe", func(rw http.ResponseWriter, r *http.Request) {
		f := rw.(http.Flusher)
		chunk := make([]byte, 4<<10)
		chunk[0] = 0x7f
		for i := 0; i < chunks; i++ {
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
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func trickleRelease(srv *httptest.Server) *ReleaseInfo {
	return &ReleaseInfo{
		Version: "2.0.0", TagName: "v2.0.0",
		DownloadURL: srv.URL + "/exe", SignatureURL: srv.URL + "/sig",
		ManifestURL: srv.URL + "/manifest", ManifestSignatureURL: srv.URL + "/manifest.sig",
	}
}

// A release binary is 78-87 MB, and the download used to carry a 5-minute
// TOTAL deadline: anything slower than about 2.3 Mbit/s was killed however
// steadily it arrived. Only a stall ends a download now. Scaled: data every
// 50 ms against a 200 ms stall timeout, for well past the timeout overall.
//
// Mutant: the stall timer not reset on progress — the download is cut at
// 200 ms.
func TestASteadyDownloadOutlastsTheStallTimeout(t *testing.T) {
	shortenStall(t, 200*time.Millisecond)
	srv := trickleServer(t, 16, 50*time.Millisecond, 0)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)

	if err := u.ApplyUpdate(context.Background(), trickleRelease(srv)); err != nil {
		t.Fatalf("a steady download failed: %v", err)
	}
	if fileIs(exePath, "current binary") {
		t.Error("the update was not placed")
	}
}

// A download that stops receiving data is abandoned after the stall timeout,
// with an error that says so — not a bare "context canceled".
//
// Mutant: no stall timer — the download waits out the server's 5 s hold.
func TestAStalledDownloadIsAbandoned(t *testing.T) {
	shortenStall(t, 200*time.Millisecond)
	srv := trickleServer(t, 2, 10*time.Millisecond, 5*time.Second)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)

	start := time.Now()
	err := u.ApplyUpdate(context.Background(), trickleRelease(srv))
	if err == nil || !strings.Contains(err.Error(), "download stalled") {
		t.Fatalf("ApplyUpdate = %v, want a stalled-download error", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("abandoned after %s, want about the 200 ms stall timeout", took)
	}
	if !fileIs(exePath, "current binary") {
		t.Error("a stalled download changed the running binary")
	}
}

func fileIs(path, want string) bool {
	got, err := os.ReadFile(path)
	return err == nil && string(got) == want
}
