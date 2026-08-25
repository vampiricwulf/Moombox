package cookies

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failSidecarCopy replaces the snapshot's copy step so the -wal copy fails
// the way `fault` describes and everything else copies normally. Restores
// the real copy at test end.
func failSidecarCopy(t *testing.T, fault func() error) {
	t.Helper()
	real := copySnapshotFile
	t.Cleanup(func() { copySnapshotFile = real })
	copySnapshotFile = func(src, dst string) error {
		if strings.HasSuffix(src, "-wal") {
			return fault()
		}
		return real(src, dst)
	}
}

// TestSnapshotFallsBackWhenTheTempSideOfTheSidecarCopyFails covers the
// misattribution: ENOSPC (or a transient I/O error) while writing OUR copy
// of the -wal into OUR temp dir says nothing about the user's profile, but
// it was wrapped as ErrCookieDBUnreadable — which querySnapshotOrLive
// deliberately refuses to fall back from. The identical failure on the main
// file did fall back, so a full temp dir hard-failed or degraded gracefully
// depending on which file it happened to trip on, and the message blamed the
// profile either way.
func TestSnapshotFallsBackWhenTheTempSideOfTheSidecarCopyFails(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	failSidecarCopy(t, func() error {
		return &copyFault{dest: true, err: errors.New("no space left on device")}
	})

	_, cleanup, err := snapshotFirefoxCookieDB(profileDir)
	cleanup()
	if err == nil {
		t.Fatal("a failed sidecar copy must still fail the snapshot")
	}
	if errors.Is(err, ErrCookieDBUnreadable) {
		t.Errorf("a full temp dir was reported as an unreadable profile: %v", err)
	}
	if strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("the message blames the source for a destination-side failure: %v", err)
	}

	// The payoff: the read still completes off the live database, which is
	// what the function's own doc comment promises for "no temp space".
	lines, err := querySnapshotOrLive(profileDir, filepath.Join(profileDir, firefoxCookieDBName), false)
	if err != nil {
		t.Fatalf("a temp-side failure must degrade to the live database, got %v", err)
	}
	if len(lines) != len(youtubeAuthRows()) {
		t.Fatalf("live fallback returned %d rows, want %d", len(lines), len(youtubeAuthRows()))
	}
}

// TestSnapshotStillRefusesFallbackOnUnreadableSidecarSource is the half that
// must NOT change. A -wal that cannot be READ is a genuinely unreadable
// profile: falling back to the live database would hit the same sidecar and
// could hand back a stale checkpointed set as if it were current.
func TestSnapshotStillRefusesFallbackOnUnreadableSidecarSource(t *testing.T) {
	profileDir := writeWALCookieProfile(t, youtubeAuthRows())
	failSidecarCopy(t, func() error {
		return &copyFault{err: errors.New("input/output error")}
	})

	_, cleanup, err := snapshotFirefoxCookieDB(profileDir)
	cleanup()
	if !errors.Is(err, ErrCookieDBUnreadable) {
		t.Fatalf("an unreadable sidecar must stay ErrCookieDBUnreadable, got %v", err)
	}

	if _, err := querySnapshotOrLive(profileDir, filepath.Join(profileDir, firefoxCookieDBName), false); !errors.Is(err, ErrCookieDBUnreadable) {
		t.Fatalf("an unreadable sidecar must not degrade to the live database, got %v", err)
	}
}

// TestCopyFileAttributesTheFailingEnd unit-tests the attribution the
// classification above rests on.
func TestCopyFileAttributesTheFailingEnd(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("missing source is a source fault", func(t *testing.T) {
		err := copyFile(filepath.Join(dir, "does-not-exist"), filepath.Join(dir, "dst"))
		if err == nil {
			t.Fatal("copying a missing file must fail")
		}
		if isDestinationCopyFault(err) {
			t.Errorf("an unreadable source was attributed to the destination: %v", err)
		}
	})

	t.Run("uncreatable destination is a destination fault", func(t *testing.T) {
		err := copyFile(src, filepath.Join(dir, "no-such-dir", "dst"))
		if err == nil {
			t.Fatal("copying into a missing directory must fail")
		}
		if !isDestinationCopyFault(err) {
			t.Errorf("a destination-side failure was attributed to the source: %v", err)
		}
	})
}

// TestFingerprintsDifferComparesInstantsNotStructs pins the time.Time
// equality trap. Both stamps describe the SAME instant; only the Location
// pointer differs, which `==` on the surrounding struct reports as a
// difference and the caller then treats as a torn snapshot.
//
// Today both stamps come from os.Stat (no monotonic reading, same
// Location), so the bug is latent — but the whole point of the trap is
// that it fires the moment a stamp reaches this comparison from anywhere
// else, and a "torn" verdict costs a retry and, on the final attempt, a
// fall back to the live database.
func TestFingerprintsDifferComparesInstantsNotStructs(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	elsewhere := at.In(time.FixedZone("elsewhere", 3600))

	a := cookieDBFingerprint{
		main: fileStamp{exists: true, size: 4096, mod: at},
		wal:  fileStamp{exists: true, size: 32768, mod: at},
	}
	b := cookieDBFingerprint{
		main: fileStamp{exists: true, size: 4096, mod: elsewhere},
		wal:  fileStamp{exists: true, size: 32768, mod: elsewhere},
	}

	if fingerprintsDiffer(a, b) {
		t.Error("two stamps of the same instant were reported as a torn snapshot — time.Time must be compared with Equal, not ==")
	}
}

// TestFingerprintsDifferStillDetectsRealChange is the other half: making the
// comparison instant-based must not make it blind.
func TestFingerprintsDifferStillDetectsRealChange(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	base := cookieDBFingerprint{
		main: fileStamp{exists: true, size: 4096, mod: at},
		wal:  fileStamp{exists: true, size: 32768, mod: at},
	}

	cases := map[string]cookieDBFingerprint{
		"main grew": {
			main: fileStamp{exists: true, size: 8192, mod: at},
			wal:  base.wal,
		},
		"main rewritten": {
			main: fileStamp{exists: true, size: 4096, mod: at.Add(time.Second)},
			wal:  base.wal,
		},
		"wal grew": {
			main: base.main,
			wal:  fileStamp{exists: true, size: 65536, mod: at},
		},
		"wal checkpointed away": {
			main: base.main,
			wal:  fileStamp{},
		},
	}
	for name, changed := range cases {
		if !fingerprintsDiffer(base, changed) {
			t.Errorf("%s: a real change to the database must be detected as torn", name)
		}
	}
}
