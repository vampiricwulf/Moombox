package cookies

import (
	"testing"
	"time"
)

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
