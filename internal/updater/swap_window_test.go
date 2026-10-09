package updater

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
)

func swapTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/exe", func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, "fresh moombox binary")
	})
	mux.HandleFunc("/sig", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Write(make([]byte, ed25519.SignatureSize))
	})
	serveManifest(mux, manifestJSON(t, platformManifest(t, "2.0.0", "v2.0.0", []byte("fresh moombox binary"))))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func swapRelease(srv *httptest.Server) *ReleaseInfo {
	return &ReleaseInfo{
		Version: "2.0.0", TagName: "v2.0.0",
		DownloadURL: srv.URL + "/exe", SignatureURL: srv.URL + "/sig",
		ManifestURL: srv.URL + "/manifest", ManifestSignatureURL: srv.URL + "/manifest.sig",
	}
}

// The swap used to rename the running exe to .old and then .new into place,
// so a kill or power loss between the two left no binary at the plain name
// and nothing that could start to repair it. Off Windows the exe path now
// always holds a binary: .old is a hard link, and .new replaces the exe in
// one rename.
//
// Mutant: keepBackupByLink reporting false — the exe is renamed away.
func TestTheSwapNeverLeavesTheExePathEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a running image cannot be renamed over on Windows")
	}
	srv := swapTestServer(t)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)

	orig := renameFile
	t.Cleanup(func() { renameFile = orig })
	renameFile = func(from, to string) error {
		if from == exePath {
			t.Errorf("the exe was renamed away to %s", to)
		}
		if to == exePath {
			if _, err := os.Stat(exePath); err != nil {
				t.Errorf("the exe path was empty when the new binary was placed: %v", err)
			}
		}
		return orig(from, to)
	}

	if err := u.ApplyUpdate(context.Background(), swapRelease(srv)); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if got, _ := os.ReadFile(exePath); string(got) != "fresh moombox binary" {
		t.Errorf("exe holds %q after the update", got)
	}
	if got, _ := os.ReadFile(exePath + ".old"); string(got) != "current binary" {
		t.Errorf(".old holds %q, want the previous binary", got)
	}
}

// A placement that fails leaves the running binary where it was and clears
// both the backup link and the download, so a failed update leaves nothing
// for the next boot to mistake for a rollback artifact.
//
// Mutant: the backup link kept on failure — .old survives.
func TestAFailedPlacementLeavesTheExeAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the link path is not taken on Windows")
	}
	srv := swapTestServer(t)
	u, exePath := newTestUpdater(t, "1.0.0", srv, nil)

	orig := renameFile
	t.Cleanup(func() { renameFile = orig })
	renameFile = func(from, to string) error {
		if to == exePath {
			return errors.New("injected placement failure")
		}
		return orig(from, to)
	}

	if err := u.ApplyUpdate(context.Background(), swapRelease(srv)); err == nil {
		t.Fatal("ApplyUpdate succeeded through a failed placement")
	}
	if got, _ := os.ReadFile(exePath); string(got) != "current binary" {
		t.Errorf("exe holds %q, want the running binary untouched", got)
	}
	for _, suffix := range []string{".old", ".new", PendingVersionSuffix, ".update-broken"} {
		if _, err := os.Stat(exePath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind: %v", suffix, err)
		}
	}
}
