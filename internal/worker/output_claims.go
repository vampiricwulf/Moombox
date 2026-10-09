package worker

import (
	"path/filepath"
	"strings"
	"sync"
)

// outputClaims records the output files a job is writing right now and has
// not yet put on its row.
//
// The orphan sweep's notion of "owned" is the DB: a file under the output
// directory is an orphan unless some row's output_file/filename/chat_file/
// segment columns name it. A finalize writes those columns only once the file
// is complete — muxAndFinalize sets output_file in the same write that marks
// the job Finished, and a part mux records its segment row after FFmpeg exits
// — so for the whole length of a mux the file being written was, to the
// sweep, a stray "output" entry with no job, and DeleteOrphanedFile's
// active-job recheck found no column naming it either. A "Delete All" clicked
// during a long copy-mux unlinked the archive FFmpeg was still writing; on
// Linux FFmpeg wrote on into the unlinked inode, the job finished clean, and
// the staging cleanup removed the only other copy.
//
// The claim is by STEM — the output path minus its extension — because one
// finalize writes a family of files that share it: "<stem>.mp4",
// "<stem>.chat.json", "<stem>.description", the thumbnail, the
// "<stem>.restart-<ts>.mp4" set-aside recoveries, and for a part
// "<stem> - partN.mp4" and the merge temporaries beside it. A prefix match can
// also cover an unrelated file whose name happens to extend the stem; that
// only keeps it off the orphan list until the finalize returns, which errs the
// safe way.
//
// Two claims are not a finalize's: a trim encode claims its trim file's stem
// until the trim row is written, and an aside recovery claims the job's
// staging directory, which it reads under a row whose status still says the
// staging is unowned.
//
// In-process state, deliberately: the sweep and the deletes run in the same
// process as the worker (internal/web/routes/files.go, the TUI's Files
// dialog), and a claim must vanish with the process that holds it — a crash
// mid-mux leaves no stale claim protecting a partial file forever.
var outputClaims = struct {
	mu    sync.Mutex
	stems map[string]*outputClaim
}{stems: map[string]*outputClaim{}}

type outputClaim struct {
	jobID string
	n     int
}

// claimOutputStem marks every output path that starts with stem as owned by
// jobID until the returned release is called (once; further calls are
// no-ops). Claims nest — the same stem claimed twice stays claimed until both
// are released. The stem is recorded in its configured spelling AND its
// canonical one, so a delete naming the file through a symlinked or junctioned
// output directory is refused as well.
func claimOutputStem(jobID, stem string) (release func()) {
	keys := outputClaimKeys(stem)
	outputClaims.mu.Lock()
	for _, k := range keys {
		c := outputClaims.stems[k]
		if c == nil {
			c = &outputClaim{jobID: jobID}
			outputClaims.stems[k] = c
		}
		c.n++
	}
	outputClaims.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			outputClaims.mu.Lock()
			defer outputClaims.mu.Unlock()
			for _, k := range keys {
				if c := outputClaims.stems[k]; c != nil {
					if c.n--; c.n <= 0 {
						delete(outputClaims.stems, k)
					}
				}
			}
		})
	}
}

// outputClaimKeys is the stem's normalized spelling plus, when it differs,
// its canonical one.
func outputClaimKeys(stem string) []string {
	keys := []string{normalizePath(stem)}
	if c := normalizePath(canonicalDir(stem)); c != keys[0] {
		keys = append(keys, c)
	}
	return keys
}

// outputClaimOwner returns the job whose in-flight finalize claims path, or
// "" when none does. path is compared as normalizePath spells it.
func outputClaimOwner(path string) string {
	n := normalizePath(path)
	outputClaims.mu.Lock()
	defer outputClaims.mu.Unlock()
	for stem, c := range outputClaims.stems {
		if strings.HasPrefix(n, stem) {
			return c.jobID
		}
	}
	return ""
}

// outputClaimOwnerUnder returns the job whose in-flight finalize claims a
// path inside dir, or "" when none does. outputClaimOwner answers for a path
// a claim covers; this answers for a directory holding one, which a delete
// would take along with the file being written. dir is matched in its own
// spelling and its canonical one.
func outputClaimOwnerUnder(dir string) string {
	prefixes := []string{normalizePath(dir) + string(filepath.Separator)}
	if c := normalizePath(canonicalDir(dir)) + string(filepath.Separator); c != prefixes[0] {
		prefixes = append(prefixes, c)
	}
	outputClaims.mu.Lock()
	defer outputClaims.mu.Unlock()
	for stem, c := range outputClaims.stems {
		for _, p := range prefixes {
			if strings.HasPrefix(stem, p) {
				return c.jobID
			}
		}
	}
	return ""
}
