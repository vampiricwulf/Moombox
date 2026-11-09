package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobverbs"
)

// dashboardStatusSets reads every `export const NAME = new Set([...]);` out
// of web/public/modules/utils.js: the dashboard's per-verb status sets.
// Parsed rather than executed — no node in the Go suites — and the shape it
// reads is one line per set, which a reformat that breaks it fails loudly
// below (a set the test expects and does not find).
func dashboardStatusSets(t *testing.T) map[string]map[database.JobStatus]bool {
	t.Helper()
	const utilsFile = "../../web/public/modules/utils.js"
	src, err := os.ReadFile(utilsFile)
	if err != nil {
		t.Fatalf("read %s: %v", utilsFile, err)
	}
	setRe := regexp.MustCompile(`(?m)^export const ([A-Z_]+_STATUSES) = new Set\(\[([^\]]*)\]\);`)
	strRe := regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	out := map[string]map[database.JobStatus]bool{}
	for _, m := range setRe.FindAllStringSubmatch(string(src), -1) {
		set := map[database.JobStatus]bool{}
		for _, lit := range strRe.FindAllString(m[2], -1) {
			s, err := strconv.Unquote(lit)
			if err != nil {
				t.Fatalf("%s: unquote %s: %v", m[1], lit, err)
			}
			set[database.JobStatus(s)] = true
		}
		if rest := strings.TrimSpace(strRe.ReplaceAllString(m[2], "")); strings.Trim(rest, ", \n\t") != "" {
			t.Fatalf("%s holds something other than string literals (%q) — teach this test the new shape", m[1], rest)
		}
		out[m[1]] = set
	}
	return out
}

// TestVerbStatusSetsMatchTheDashboard pins the dashboard's verb sets to the
// Go predicates every other surface calls (internal/jobverbs), for every
// JobStatus internal/database/types.go declares. Before, each was a
// hand-kept copy in REST, the TUI and JS, and only Delete had a pin.
//
// Mutant: add or drop a status on either side — this names the status and
// the verb.
func TestVerbStatusSetsMatchTheDashboard(t *testing.T) {
	all := declaredJobStatuses(t)
	if len(all) < 9 {
		t.Fatalf("parsed %d JobStatus constants, want at least nine", len(all))
	}
	sets := dashboardStatusSets(t)
	verbs := []struct {
		set  string
		goFn func(database.JobStatus) bool
	}{
		{"CANCEL_STATUSES", jobverbs.Cancellable},
		{"REINIT_STATUSES", jobverbs.ReinitializableStatus},
		// The incomplete-tail Finished arm is canResumeJob's, beside the set.
		{"RESUMABLE_STATUSES", func(s database.JobStatus) bool { return jobverbs.ResumableStatus(s, false) }},
		// Finished muxes only with an unmuxed part, a fact job-details.js
		// checks beside the set.
		{"MUX_STATUSES", func(s database.JobStatus) bool {
			return jobverbs.MuxableStatus(s) && !jobverbs.MuxNeedsUnmuxedParts(s)
		}},
		{"DELETE_STATUSES", jobverbs.DeletableStatus},
		// The arms beside the sets: the statuses Resume accepts only with an
		// incomplete tail (canResumeJob), Mux only with an unmuxed part
		// (job-details.js), and chapter import only without an incomplete tail
		// (chapterImportAllowed, which refuses an incomplete tail on every
		// status — pinned below).
		{"RESUME_TAIL_STATUSES", func(s database.JobStatus) bool {
			return jobverbs.ResumableStatus(s, true) && !jobverbs.ResumableStatus(s, false)
		}},
		{"MUX_PARTS_STATUSES", func(s database.JobStatus) bool {
			return jobverbs.MuxableStatus(s) && jobverbs.MuxNeedsUnmuxedParts(s)
		}},
		{"CHAPTER_IMPORT_STATUSES", func(s database.JobStatus) bool { return jobverbs.ChapterImportableStatus(s, false) }},
		{"WATCHED_STATUSES", jobverbs.WatchableStatus},
		// Trim also needs the archive file, a fact job-details.js checks beside
		// the set.
		{"TRIM_STATUSES", jobverbs.TrimmableStatus},
	}
	for _, v := range verbs {
		js, ok := sets[v.set]
		if !ok {
			t.Errorf("utils.js declares no %s (found %d sets)", v.set, len(sets))
			continue
		}
		accepted := 0
		for _, s := range all {
			if g := v.goFn(s); g != js[s] {
				t.Errorf("%s: status %q: Go predicate = %v, utils.js = %v", v.set, s, g, js[s])
			}
			if js[s] {
				accepted++
			}
		}
		// A set naming a status no JobStatus constant declares is a typo the
		// loop above cannot see.
		if accepted != len(js) {
			t.Errorf("%s names %d statuses, %d of them declared JobStatus values: %v", v.set, len(js), accepted, js)
		}
	}
	for _, s := range all {
		if jobverbs.ChapterImportableStatus(s, true) {
			t.Errorf("status %q: ChapterImportableStatus accepts an incomplete tail, which chapterImportAllowed refuses on every status", s)
		}
	}
	// The arms are read through the sets, not spelled beside them again.
	for file, use := range map[string]string{
		"../../web/public/modules/utils.js":          "RESUME_TAIL_STATUSES.has(job.status) && !!job.incompleteTail",
		"../../web/public/modules/job-details.js":    "MUX_PARTS_STATUSES.has(job.status) && job.unmuxedParts === true",
		"../../web/public/modules/chapter-import.js": "CHAPTER_IMPORT_STATUSES.has(job.status) && !job.incompleteTail",
		"../../web/public/app.js":                    "WATCHED_STATUSES.has(j.status) && !j.watched",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), use) {
			t.Errorf("%s does not read its arm as %q", file, use)
		}
	}
}

// TestDashboardVerbArmsReadTheirSets: the dashboard's Watched, Trim and
// Resume gates read the sets the parity test compares, not a "Finished" or
// "youtube" literal spelled beside them again, and RESUME_PLATFORMS is
// jobverbs.ResumeSupported over every platform a job can carry.
//
// Mutant: a literal put back in any of those gates; "twitch" added to
// RESUME_PLATFORMS.
func TestDashboardVerbArmsReadTheirSets(t *testing.T) {
	read := func(file string) string {
		t.Helper()
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return string(src)
	}
	app := read("../../web/public/app.js")
	details := read("../../web/public/modules/job-details.js")
	utils := read("../../web/public/modules/utils.js")

	if n := strings.Count(app, "WATCHED_STATUSES.has(j.status) && "); n != 4 {
		t.Errorf("app.js reads WATCHED_STATUSES in %d of the batch bar's four watched gates", n)
	}
	if strings.Contains(app, `j.status === "Finished" && !j.watched`) ||
		strings.Contains(app, `j.status === "Finished" && (j.watched`) {
		t.Error(`app.js spells a watched gate as j.status === "Finished" again`)
	}
	for file, use := range map[string]struct{ src, want string }{
		"job-details.js": {details, "TRIM_STATUSES.has(job.status) && job.filename"},
		"utils.js":       {utils, "RESUME_PLATFORMS.has(job.platform)"},
	} {
		if !strings.Contains(use.src, use.want) {
			t.Errorf("%s does not read its gate as %q", file, use.want)
		}
	}
	if !strings.Contains(details, `setDisplay("details-trim-btn", canTrim)`) {
		t.Error("job-details.js shows the Trim button off something other than canTrim")
	}

	m := regexp.MustCompile(`(?m)^export const RESUME_PLATFORMS = new Set\(\[([^\]]*)\]\);`).FindStringSubmatch(utils)
	if m == nil {
		t.Fatal("utils.js declares no RESUME_PLATFORMS")
	}
	js := map[string]bool{}
	for _, lit := range regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).FindAllString(m[1], -1) {
		p, err := strconv.Unquote(lit)
		if err != nil {
			t.Fatal(err)
		}
		js[p] = true
	}
	for _, p := range []string{"youtube", "twitch", ""} {
		if g := jobverbs.ResumeSupported(p); g != js[p] {
			t.Errorf("RESUME_PLATFORMS: platform %q: Go ResumeSupported = %v, utils.js = %v", p, g, js[p])
		}
	}
	for p := range js {
		if !jobverbs.ResumeSupported(p) {
			t.Errorf("RESUME_PLATFORMS names %q, which jobverbs.ResumeSupported refuses", p)
		}
	}
}
