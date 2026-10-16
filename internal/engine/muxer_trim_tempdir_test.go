package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMultiSegmentTrimIntermediatesStayBesideTheOutput: a multi-segment trim
// re-encodes every part it touches into an intermediate before concatenating
// them, and together those are about the size of the clip. They were written
// under os.MkdirTemp("", ...) — in Docker the container's /tmp, the writable
// layer on the host's /var/lib/docker that no disk alert or disk gate
// watches, and RAM on a host whose /tmp is a tmpfs, where a long trim failed
// with ENOSPC however much room the output disk had. They now go in a
// "<stem>.moombox-trim-<n>" directory beside the output, which is removed
// when the trim is done; the output's directory need not exist beforehand.
//
// Mutants: MkdirTemp under "" again (the intermediates land in the stand-in
// /tmp); a pattern without the output's stem, "moombox-trim-*" (the
// directory's name no longer starts with the stem the trim service claims);
// drop the MkdirAll ahead of MkdirTemp (the trim fails: the output's
// directory does not exist yet); drop the deferred RemoveAll (the directory
// outlives the trim).
func TestMultiSegmentTrimIntermediatesStayBesideTheOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in for FFmpeg")
	}
	bin := t.TempDir()
	tmpRoot := t.TempDir() // stands in for the container's /tmp
	volume := t.TempDir()  // stands in for /data
	t.Setenv("TMPDIR", tmpRoot)

	ffmpeg := filepath.Join(bin, "ffmpeg")
	script := "#!/bin/sh\n" +
		"for a; do out=\"$a\"; done\n" +
		"printf '%s\\n' \"$out\" >> '" + bin + "/outputs'\n" +
		"printf media > \"$out\"\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	in1 := filepath.Join(volume, "a - part1.mp4")
	in2 := filepath.Join(volume, "a - part2.mp4")
	for _, p := range []string{in1, in2} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	trimDir := filepath.Join(volume, "chan", "trim") // not created: the trim makes it
	out := filepath.Join(trimDir, "vid [10s-7200s].partial.mp4")

	m := NewMuxer(ffmpeg, &testLogger{})
	err := m.TrimAndConcatWithProgress(context.Background(), []TrimSegmentInput{
		{InputPath: in1, StartTime: 10, Duration: 3590},
		{InputPath: in2, StartTime: 0, Duration: 3600, NeedScale: true},
	}, out, 1920, 1080, 60, 23, 128, func(float64) {})
	if err != nil {
		t.Fatalf("TrimAndConcatWithProgress: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(bin, "outputs"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("FFmpeg ran %d times, want two segment encodes and a concat: %q", len(lines), lines)
	}
	// The concat writes the clip under its temporary name beside it, which
	// writeOutput renames into place (TestEveryFFmpegWriteIsHandedATemporaryName).
	if filepath.Dir(lines[2]) != trimDir ||
		!strings.HasPrefix(filepath.Base(lines[2]), "vid [10s-7200s].partial"+muxTempMarker) ||
		!muxTempRe.MatchString(lines[2]) {
		t.Errorf("the concat wrote %q, want the output's temporary name beside it (%s)", lines[2], out)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("the clip is not at its own name after the trim: %v", err)
	}
	for _, seg := range lines[:2] {
		if strings.HasPrefix(seg, tmpRoot) {
			t.Errorf("intermediate %q was written under os.TempDir(), off the output's volume", seg)
		}
		holder := filepath.Dir(seg)
		if filepath.Dir(holder) != trimDir {
			t.Errorf("intermediate %q is not in a directory beside the output %q", seg, out)
		}
		name := filepath.Base(holder)
		if !strings.HasPrefix(name, "vid [10s-7200s].partial.moombox-trim-") || !trimTempDirRe.MatchString(name) {
			t.Errorf("intermediates directory %q is not named after the output's stem (<stem>.moombox-trim-<n>)", name)
		}
	}
	entries, err := os.ReadDir(trimDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("the intermediates directory %q outlived the trim", e.Name())
		}
	}
}

// TestRemoveStaleTrimTempDirs: the startup sweep of a trim directory removes
// the intermediates directories a hard abort left there once they are over a
// day old — and nothing else that directory holds: a clip, a fresh
// intermediates directory a running trim owns, a file or a directory that
// only resembles the name. A directory that does not exist has nothing to
// sweep and is no error.
//
// Mutants: drop the IsDir check (the stale-looking file goes); drop the age
// check (the fresh directory goes); match on the marker alone, without the
// decimal tail (the "-abc" directory goes); return ReadDir's error for a
// missing directory (the last call fails).
func TestRemoveStaleTrimTempDirs(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	mk := func(name string, isDir bool, mtime time.Time) {
		t.Helper()
		p := filepath.Join(dir, name)
		var err error
		if isDir {
			err = os.Mkdir(p, 0o755)
			if err == nil {
				err = os.WriteFile(filepath.Join(p, "seg_0.mp4"), []byte("x"), 0o644)
			}
		} else {
			err = os.WriteFile(p, []byte("x"), 0o644)
		}
		if err == nil {
			err = os.Chtimes(p, mtime, mtime)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	mk("vid [0s-60s].partial.moombox-trim-123", true, old)
	mk("vid [0s-90s].partial.moombox-trim-456", true, time.Now())
	mk("vid [0s-30s].mp4", false, old)
	mk("vid [0s-40s].partial.moombox-trim-789", false, old)
	mk("vid [0s-50s].partial.moombox-trim-abc", true, old)

	removed, err := RemoveStaleTrimTempDirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed %d, want 1", removed)
	}
	for name, want := range map[string]bool{
		"vid [0s-60s].partial.moombox-trim-123": false,
		"vid [0s-90s].partial.moombox-trim-456": true,
		"vid [0s-30s].mp4":                      true,
		"vid [0s-40s].partial.moombox-trim-789": true,
		"vid [0s-50s].partial.moombox-trim-abc": true,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		if exists := err == nil; exists != want {
			t.Errorf("%s exists = %v, want %v", name, exists, want)
		}
	}

	if n, err := RemoveStaleTrimTempDirs(filepath.Join(dir, "no-such-trim-dir")); n != 0 || err != nil {
		t.Errorf("a missing directory: removed %d, err %v; want 0, nil", n, err)
	}
}
