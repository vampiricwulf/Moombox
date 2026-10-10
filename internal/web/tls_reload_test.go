package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTLSReloadWarnsOncePerBrokenPair: on a mismatched cert/key the reload
// returned before advancing lastModTime, so every handshake re-parsed both
// files and wrote the same Warn — per tab, per reconnect, per plugin call —
// until the key was fixed. It now tries a given pair once, and still picks up
// a key fixed afterwards.
//
// Mutants: drop the failed-pair check (5 Warns); key it on the cert alone
// (the fixed key is never loaded).
func TestTLSReloadWarnsOncePerBrokenPair(t *testing.T) {
	log := &countingWSLogger{}
	dirA, dirB := t.TempDir(), t.TempDir()
	certA, keyA := filepath.Join(dirA, "cert.pem"), filepath.Join(dirA, "key.pem")
	certB, keyB := filepath.Join(dirB, "cert.pem"), filepath.Join(dirB, "key.pem")
	if err := generateSelfSignedCert(certA, keyA, "localhost", log); err != nil {
		t.Fatal(err)
	}
	if err := generateSelfSignedCert(certB, keyB, "localhost", log); err != nil {
		t.Fatal(err)
	}
	w := &certWatcher{certPath: certA, keyPath: keyA, logger: log}
	w.reloadIfChanged() // loads pair A
	if w.cert.Load() == nil {
		t.Fatal("premise lost: pair A did not load")
	}

	copyNewer := func(src, dst string, at time.Time) {
		t.Helper()
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dst, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// A deploy hook copies B's cert first: the pair on disk is mismatched.
	now := time.Now()
	copyNewer(certB, certA, now.Add(time.Minute))
	_, warnsBefore := log.counts()
	for range 5 {
		w.reloadIfChanged() // five handshakes
	}
	if _, warns := log.counts(); warns-warnsBefore != 1 {
		t.Errorf("five handshakes on one broken pair logged %d warnings, want 1", warns-warnsBefore)
	}

	// ...then B's key: the pair is whole again and must load.
	old := w.cert.Load()
	copyNewer(keyB, keyA, now.Add(2*time.Minute))
	w.reloadIfChanged()
	if w.cert.Load() == old {
		t.Error("the fixed key was never loaded — the failed pair was remembered by the cert alone")
	}
}
