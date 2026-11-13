package disk

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestWorsePicksTheFullerVolumeAndTiesGoToOutput: the alerts and the backlog
// gate judge the fuller of the two volumes, and one volume under both
// directories is named as the output directory.
func TestWorsePicksTheFullerVolumeAndTiesGoToOutput(t *testing.T) {
	out := &DiskSpace{UsedPct: 50}
	if got, staging := Worse(out, &DiskSpace{UsedPct: 97}); !staging || got.UsedPct != 97 {
		t.Errorf("fuller staging: got %.0f staging=%v, want the staging reading", got.UsedPct, staging)
	}
	if got, staging := Worse(out, &DiskSpace{UsedPct: 10}); staging || got != out {
		t.Errorf("emptier staging: staging=%v, want the output reading", staging)
	}
	if got, staging := Worse(out, &DiskSpace{UsedPct: 50}); staging || got != out {
		t.Errorf("tie: staging=%v, want the output reading", staging)
	}
	if got, staging := Worse(out, nil); staging || got != out {
		t.Errorf("no second volume: staging=%v, want the output reading", staging)
	}
}

// TestOtherVolumeReadsStagingOnlyOnAnotherVolume: one volume under both
// directories is read once, by the caller's output reading.
func TestOtherVolumeReadsStagingOnlyOnAnotherVolume(t *testing.T) {
	var asked []string
	query := func(dir string) (*DiskSpace, error) {
		asked = append(asked, dir)
		return &DiskSpace{UsedPct: 80}, nil
	}
	same := func(a, b string) bool { return true }
	other := func(a, b string) bool { return false }

	if ds, err := OtherVolume("out", "stg", same, query); ds != nil || err != nil || len(asked) != 0 {
		t.Errorf("same volume: ds=%v err=%v asked=%v, want no second reading", ds, err, asked)
	}
	if ds, err := OtherVolume("out", "", other, query); ds != nil || err != nil || len(asked) != 0 {
		t.Errorf("no staging directory: ds=%v err=%v asked=%v, want no second reading", ds, err, asked)
	}
	ds, err := OtherVolume("out", "stg", other, query)
	if err != nil || ds == nil || ds.UsedPct != 80 || len(asked) != 1 || asked[0] != "stg" {
		t.Errorf("other volume: ds=%v err=%v asked=%v, want the staging directory read once", ds, err, asked)
	}
	boom := errors.New("offline")
	if ds, err := OtherVolume("out", "stg", other, func(string) (*DiskSpace, error) { return nil, boom }); ds != nil || !errors.Is(err, boom) {
		t.Errorf("failed staging read: ds=%v err=%v, want the error", ds, err)
	}
}

// TestSameVolumeForDirectoriesUnderOneParent: two directories under one
// temporary directory — one of them not created yet, as a default
// ./staging is before the first capture — are one volume.
func TestSameVolumeForDirectoriesUnderOneParent(t *testing.T) {
	root := t.TempDir()
	if !SameVolume(filepath.Join(root, "output"), filepath.Join(root, "staging", "not-yet")) {
		t.Error("two directories under one parent reported as different volumes")
	}
}
