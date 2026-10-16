package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// OutputCheck inspects a finished output while it is still under its
// temporary name. An archive's own checks belong here — ENGINE-9's comparison
// of the copy's duration with its input's — so a file they reject never takes
// the archive's name. An error rejects the output: the temporary file is
// removed and the error is returned as it stands.
type OutputCheck func(ctx context.Context, path string) error

// writeOutput is how every FFmpeg write of an output file goes: Mux, MuxCopy,
// ConcatCopy and the cross-segment trim's concat. FFmpeg writes a temporary
// file beside final (createMuxTemp), and the temporary file is renamed onto
// final only once FFmpeg has exited 0 and every check has passed. Anything
// else — FFmpeg failing, a cancel killing it, a check refusing it, the rename
// itself — removes the temporary file and leaves final as it was.
//
// FFmpeg used to write final itself, with -y, which is what made a crash
// mid-mux unrecoverable (W28-A01): the crashed child's FFmpeg wrote on, the
// respawned child's boot re-mux wrote the same file again, and two writers
// left an archive that read as Finished and played as garbage. A temporary
// name of this run's own is an inode no stray writer shares, a crash leaves
// only that temporary file — which the next start's sweep removes
// (RemoveStaleMuxTemps) — and final is never a half-written file.
//
// The temporary file is created here, before FFmpeg opens it by name (every
// writer passes -y), and this process holds a lock on it until the file is
// renamed or removed (lockMuxTempFile): the lock, not the name, is what tells
// a sweep the file's writer is alive.
//
// A cancelled run takes its temporary file with it too. It used to be kept,
// under the final name, "for a salvage": FFmpeg killed before its trailer
// leaves an MP4 with no moov, which nothing plays, while the staging it was
// muxed from is kept for the next mux.
func (m *Muxer) writeOutput(ctx context.Context, final string, checks []OutputCheck, write func(tmp string) error) error {
	tmp, held, err := m.createMuxTemp(final)
	if err != nil {
		return fmt.Errorf("create the temporary output: %w", err)
	}
	// Closed last, after the rename or the removal: until then the lock says
	// the file is being written.
	defer held.Close()
	err = write(tmp)
	for _, check := range checks {
		if err != nil {
			break
		}
		err = check(ctx, tmp)
	}
	if err == nil {
		// Flush the finished output to disk before it takes the archive's
		// name, so a power loss after the rename cannot leave that name on a
		// file whose bytes never landed — the staging it was muxed from is
		// deleted once the row reads Finished. Through the handle the lock is
		// on, which can write (Windows flushes only such a handle); best
		// effort, the rename happens all the same.
		_ = held.Sync()
		// utils.ReplaceFile, not os.Rename: on Windows an indexer or a scanner
		// still holding the file FFmpeg just closed refuses the rename for a
		// moment (sweep-2 TOOL-2), and it replaces a final that exists — a
		// re-mux of an archive whose row never reached Finished.
		if rerr := utils.ReplaceFile(tmp, final); rerr != nil {
			err = fmt.Errorf("move the finished output into place: %w", rerr)
		}
	}
	if err != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !os.IsNotExist(rmErr) {
			m.logger.Warn("could not remove a failed FFmpeg run's temporary output; the next start removes it",
				"path", tmp, "err", rmErr)
		}
	}
	return err
}

// errMuxTempHeld is lockMuxTempFile's answer when another open of the file
// holds its lock: the writer is alive.
var errMuxTempHeld = errors.New("the temporary output's lock is held")

// lockMuxTemp is lockMuxTempFile, a variable so a test can stand in for a
// filesystem that cannot lock.
var lockMuxTemp = lockMuxTempFile

// muxTempCreateAttempts bounds createMuxTemp's tries; each draws a new name.
const muxTempCreateAttempts = 4

// createMuxTemp creates the temporary output for final and returns it open,
// with this process holding its lock. The name is a new one each try
// (muxTempPath): a leftover under it — an earlier run's whose token matched
// this run's — is never written over, and a file a sweep took between the
// create and the lock (another process's, which found it unlocked for that
// moment and removed it) is let go for a fresh one, so FFmpeg never writes a
// name the lock is not on. On a filesystem where the lock cannot be taken at
// all the file is written unlocked; the sweep spares such a file until it has
// sat untouched for muxTempIdleAge.
func (m *Muxer) createMuxTemp(final string) (string, *os.File, error) {
	var lastErr error
	for range muxTempCreateAttempts {
		tmp := muxTempPath(final)
		f, err := createMuxTempFile(tmp)
		if errors.Is(err, fs.ErrExist) {
			lastErr = err
			continue
		}
		if err != nil {
			return "", nil, err
		}
		lerr := lockMuxTemp(f)
		if lerr != nil && !errors.Is(lerr, errMuxTempHeld) {
			m.logger.Debug("cannot lock the temporary output here; it is written unlocked", "path", tmp, "err", lerr)
			return tmp, f, nil
		}
		if lerr == nil && sameFileAsPath(f, tmp) {
			return tmp, f, nil
		}
		f.Close()
		lastErr = fmt.Errorf("%s was removed before its lock was taken", tmp)
	}
	return "", nil, lastErr
}

// sameFileAsPath reports whether path still names the file f has open.
func sameFileAsPath(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(path)
	return err == nil && os.SameFile(held, named)
}

// muxTempMarker is what names a file as an FFmpeg run's temporary output:
// "<final stem>.moombox-tmp-<run>-<n><ext>". The extension stays last because
// FFmpeg picks the output format from it.
const muxTempMarker = ".moombox-tmp-"

// muxTempNameMax is the longest file name the temporary output may have, in
// bytes: 255 is a name on every filesystem Moombox writes to (UTF-16 code
// units on NTFS, which a UTF-8 byte count never undercounts).
const muxTempNameMax = 255

// muxRun is this process's run token: four random bytes, drawn once at
// start, in the name of every temporary output it writes. It keeps this
// run's own temporary files out of the boot sweep without opening them, and
// two runs' files for one archive apart. It is not what tells the sweep
// another run's file is a leftover — another token is not a gone process: a
// second Moombox writing to the same output tree under its own data
// directory writes under its own — the writer's lock is (lockMuxTempFile). A
// collision with an earlier run's token is a 1 in 2^32 chance per pair of
// runs, and costs the sweep that leftover until the next start.
var muxRun = newMuxRun()

// muxTempSeq numbers this run's temporary outputs, so two writes of one final
// name (an aside retried while the first still unwinds) never share a file.
var muxTempSeq atomic.Uint64

func newMuxRun() string {
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not fail (it aborts the process instead)
	return hex.EncodeToString(b[:])
}

// muxTempPath names a temporary output for final: in final's own directory,
// so the move into place is a rename on one volume, and under final's stem,
// so the claim a finalize holds on that stem (claimOutputStem, internal/worker)
// keeps it off the orphan sweep's list while FFmpeg writes it. A name that
// would pass muxTempNameMax loses the end of its stem instead; the archive
// names config.ResolveTemplate builds leave room for the marker, and only a
// set-aside recording's sibling of a stem at that budget comes near.
func muxTempPath(final string) string {
	dir, base := filepath.Split(final)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	tag := muxTempMarker + muxRun + "-" + strconv.FormatUint(muxTempSeq.Add(1), 10)
	if over := len(stem) + len(tag) + len(ext) - muxTempNameMax; over > 0 {
		stem = cutUTF8(stem, len(stem)-over)
	}
	return filepath.Join(dir, stem+tag+ext)
}

// cutUTF8 cuts s to at most max bytes on a rune boundary.
func cutUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// muxTempRe matches a temporary output's name and captures its run token.
var muxTempRe = regexp.MustCompile(`\.moombox-tmp-([0-9a-f]{8})-[0-9]+(\.[^.]*)?$`)

// muxTempRunOf returns the run token in a temporary output's file name, or ""
// when name is not one.
func muxTempRunOf(name string) string {
	if m := muxTempRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// IsMuxTempPath reports whether path names an FFmpeg run's temporary output
// (writeOutput), this run's or another's.
func IsMuxTempPath(path string) bool { return muxTempRunOf(filepath.Base(path)) != "" }

// muxTempIdleAge is how long a temporary output whose lock cannot be tried
// must have sat untouched before the sweep takes it: a live FFmpeg rewrites
// its output as it goes, and the checks and the rename after it take
// seconds, so a day is no run's. The trim sweep trusts the same age.
const muxTempIdleAge = 24 * time.Hour

// RemoveStaleMuxTemps is the start-up sweep for the temporary outputs a run
// that is gone left behind — a crash or a kill mid-mux leaves exactly one,
// beside the archive it was writing. It walks each of trees and reads each of
// dirs (one level), and removes a regular file named as a temporary output
// (muxTempMarker) only when its writer is gone: its token is not this run's,
// and its writer's lock is free (removeStaleMuxTemp). A file whose lock is
// held is a live run's — another Moombox writing to the same tree — and is
// returned in live. A root that does not exist is skipped, an entry that
// cannot be read or removed is skipped, and the first error that stopped a
// root being read is returned with what was removed.
func RemoveStaleMuxTemps(trees, dirs []string) (removed, live []string, err error) {
	sweep := func(path string) {
		if run := muxTempRunOf(filepath.Base(path)); run == "" || run == muxRun {
			return
		}
		switch removeStaleMuxTemp(path) {
		case muxTempRemoved:
			removed = append(removed, path)
		case muxTempLive:
			live = append(live, path)
		}
	}
	note := func(e error) {
		if err == nil && e != nil && !os.IsNotExist(e) {
			err = e
		}
	}
	for _, root := range trees {
		// The trailing separator makes the walk resolve an output directory
		// that is itself a symlink (or a junction), as the orphan sweep's
		// does; links below it are not followed.
		walkRoot := root
		if !strings.HasSuffix(walkRoot, string(filepath.Separator)) {
			walkRoot += string(filepath.Separator)
		}
		note(filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				if path == walkRoot {
					return werr
				}
				return nil
			}
			if d.Type().IsRegular() {
				sweep(path)
			}
			return nil
		}))
	}
	for _, dir := range dirs {
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			note(rerr)
			continue
		}
		for _, e := range entries {
			if e.Type().IsRegular() {
				sweep(filepath.Join(dir, e.Name()))
			}
		}
	}
	return removed, live, err
}

// muxTempVerdict is what removeStaleMuxTemp did with one temporary output.
type muxTempVerdict int

const (
	muxTempKept    muxTempVerdict = iota // unreadable, too fresh to judge unlocked, or the remove failed
	muxTempRemoved                       // its writer was gone, and it is
	muxTempLive                          // its writer holds its lock
)

// removeStaleMuxTemp removes one temporary output of another run's if its
// writer is gone. Taking the writer's lock is the proof: the lock goes with
// the process that held it, however that process ended, and the file is
// removed while the sweep holds it, so no writer can be on it in between
// (createMuxTemp lets go of a name it finds gone after its own lock). Where
// the lock cannot be tried at all, a file untouched for muxTempIdleAge is
// taken instead.
func removeStaleMuxTemp(path string) muxTempVerdict {
	f, err := openMuxTempForSweep(path)
	if err != nil {
		return muxTempKept
	}
	defer f.Close()
	switch lerr := lockMuxTemp(f); {
	case lerr == nil:
	case errors.Is(lerr, errMuxTempHeld):
		return muxTempLive
	default:
		st, serr := f.Stat()
		if serr != nil || time.Since(st.ModTime()) < muxTempIdleAge {
			return muxTempKept
		}
	}
	if os.Remove(path) != nil {
		return muxTempKept
	}
	return muxTempRemoved
}
