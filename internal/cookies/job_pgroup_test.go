package cookies

import (
	"errors"
	"testing"
)

// Test group ids. Odd, and far outside any plausible live range, following the
// convention autocookies_setup_reap_test.go states for its PIDs: nothing here
// may reach a real process on the machine running the tests. Every test that
// could signal one installs captureGroupKills first.
const (
	setupGroupPid = 424247
	setupChildPid = 424249
	strangerPid   = 424251
	strangerGroup = 424253
)

// fakeProcessTable binds the package's process-table hook to a fixed pid→pgid
// map for one test and restores the real one afterwards.
//
// This is the "fake process table" the owner's ruling names. It is the whole
// reason the decision logic lives in a file with no build tag: with it, every
// branch of the Linux reap runs in the ordinary Windows test suite, and no
// Linux box is needed to review this arc.
func fakeProcessTable(t *testing.T, table map[int]int) {
	t.Helper()
	prev := listProcessGroups
	listProcessGroups = func() (map[int]int, error) { return table, nil }
	t.Cleanup(func() { listProcessGroups = prev })
}

// unreadableProcessTable binds the hook to a failure — a hardened container
// where /proc cannot be walked — and returns the error it will answer with, so
// a test can assert the error travels rather than being swallowed into a zero.
func unreadableProcessTable(t *testing.T) error {
	t.Helper()
	prev := listProcessGroups
	err := errors.New("read /proc: permission denied")
	listProcessGroups = func() (map[int]int, error) { return nil, err }
	t.Cleanup(func() { listProcessGroups = prev })
	return err
}

// captureGroupKills swaps the group-kill hook for a recorder and restores it
// when the test ends. Same job captureKills does one layer up, and for the same
// reason: no signal may reach a real process group on the developer machine.
func captureGroupKills(t *testing.T) *[]int {
	t.Helper()
	prev := killProcessGroup
	killed := []int{}
	killProcessGroup = func(pgid int) error {
		killed = append(killed, pgid)
		return nil
	}
	t.Cleanup(func() { killProcessGroup = prev })
	return &killed
}

// TestParseProcStatPGIDReadsFieldFiveAfterTheLastParen pins the one piece of
// /proc parsing that is easy to get wrong and impossible to notice: field 2 of
// /proc/<pid>/stat is `comm`, the executable name in parentheses, and the
// kernel does not escape it. Firefox's content processes really are named
// "(Web Content)" and systemd's helper really is "((sd-pam))".
//
// Mutation that must break this: replace strings.LastIndexByte with
// IndexByte — the "Web Content" row then parses the word "Content)" and
// answers 0, so every Firefox content process is silently missing from the
// table and an abandoned setup reads as empty. Second mutation: read
// fields[1] instead of fields[2] — every row returns the PPID, which for a
// browser started by Moombox is Moombox's own pid. Third mutation: drop the
// state check and the zombie row parses as a member — a browser's orphaned
// child, reparented to a PID-1 Moombox that never reaps it, then holds the
// count above zero for the life of the process (decision 8).
func TestParseProcStatPGIDReadsFieldFiveAfterTheLastParen(t *testing.T) {
	cases := []struct {
		name string
		stat string
		want int
		ok   bool
	}{
		{
			"plain comm",
			"4242 (firefox) S 1 4242 4242 0 -1 4194560 1234 0 0 0 5 3 0 0 20 0 1 0 100 0 0",
			4242, true,
		},
		{
			"comm with a space",
			"4310 (Web Content) S 4242 4242 4242 0 -1 4194560 999 0 0 0 1 0 0 0 20 0 12 0 140 0 0",
			4242, true,
		},
		{
			"comm wrapped in its own parens",
			"4311 ((sd-pam)) S 1 4311 4311 0 -1 1077936384 100 0 0 0 0 0 0 0 20 0 1 0 90 0 0",
			4311, true,
		},
		{
			"comm with both a space and a paren",
			"4312 (Isolated Web Co (x)) S 4242 4242 4242 0 -1 4194560 50 0 0 0 0 0 0 0 20 0 9 0 150 0 0",
			4242, true,
		},
		{
			"zombie keeps its group id but is not a live member",
			"4314 (Web Content) Z 4242 4242 4242 0 -1 4194560 0 0 0 0 1 0 0 0 20 0 1 0 140 0 0",
			0, false,
		},
		{"kernel thread in group zero", "2 (kthreadd) S 0 0 0 0 -1 2129984 0 0 0 0 0 0 0 0 20 0 1 0 1 0 0", 0, false},
		{"truncated after the comm", "4242 (firefox) S 1", 0, false},
		{"no parenthesis at all", "4242 firefox S 1 4242", 0, false},
		{"non-numeric group", "4242 (firefox) S 1 nope 4242", 0, false},
		{"empty", "", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseProcStatPGID(tc.stat)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseProcStatPGID(%q) = (%d, %v), want (%d, %v)",
					tc.stat, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestPGroupJobCannotAnswerWithoutAGroup is the honest-zero rule, the same one
// job_windows.go's queryable() carries: a count of zero from a job that never
// adopted anything is "there is nothing to ask", not "the browser is gone", and
// the reap acts on the second.
//
// Mutation: make queryable() return true unconditionally and the reap releases
// the acquisition slot for every launch where the group could not be adopted.
func TestPGroupJobCannotAnswerWithoutAGroup(t *testing.T) {
	fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})

	for _, tc := range []struct {
		name string
		job  *pgroupJob
	}{
		{"nil job", nil},
		{"never adopted", &pgroupJob{}},
		{"nonsense negative group", &pgroupJob{pgid: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.job.queryable() {
				t.Fatal("queryable() said it could answer with no group adopted")
			}
			n, err := tc.job.activeProcesses()
			if err != nil || n != 0 {
				t.Fatalf("activeProcesses() = (%d, %v), want (0, nil)", n, err)
			}
		})
	}
}

// TestPGroupJobCountsOnlyItsOwnGroup is the count the reap reads. The table
// deliberately contains a stranger process AND a stranger group leader, because
// the failure this guards is a count that answers "still alive" for someone
// else's browser.
//
// Mutation: compare the map's KEY instead of its value and the count collapses
// to one (only the leader itself), so a live browser with three content
// processes reads as gone the moment its launcher exits.
func TestPGroupJobCountsOnlyItsOwnGroup(t *testing.T) {
	fakeProcessTable(t, map[int]int{
		setupGroupPid: setupGroupPid, // the launcher/leader
		setupChildPid: setupGroupPid, // a content process it forked
		strangerPid:   strangerGroup, // someone else's browser
		strangerGroup: strangerGroup,
	})
	job := &pgroupJob{pgid: setupGroupPid}

	n, err := job.activeProcesses()
	if err != nil {
		t.Fatalf("activeProcesses: %v", err)
	}
	if n != 2 {
		t.Fatalf("activeProcesses() = %d, want 2 (the leader and its child, not the stranger)", n)
	}
}

// TestPGroupJobReportsAFailedTableReadAsAnError is invariant 1 at its source: a
// container that cannot walk /proc must not have its acquisition slot released.
// The error has to TRAVEL — a (0, nil) here reads as an empty group upstream.
//
// Mutation: swallow the error and return (0, nil) and a hardened container
// reaps every setup 60 seconds after the launcher exits, with the browser still
// on screen.
func TestPGroupJobReportsAFailedTableReadAsAnError(t *testing.T) {
	want := unreadableProcessTable(t)
	job := &pgroupJob{pgid: setupGroupPid}

	n, err := job.activeProcesses()
	if !errors.Is(err, want) {
		t.Fatalf("activeProcesses() error = %v, want the table's own error", err)
	}
	if n != 0 {
		t.Fatalf("activeProcesses() = %d alongside an error, want 0", n)
	}
}

// TestPGroupJobAdoptsOnlyAProcessThatLeadsItsOwnGroup is the safety argument
// for the whole arc, and the assertion whose absence would be catastrophic
// rather than merely wrong.
//
// configureCmdSysProcAttr sets Setpgid, so a browser Moombox launched leads a
// group whose id is its own pid. A process started WITHOUT that flag inherits
// MOOMBOX'S group — and recording that pid would later point killGroup at
// Moombox's own process group: SIGKILL to Moombox and everything it spawned,
// and in Docker to the container.
//
// Mutation: drop the `pgid != pid` comparison and the "inherited Moombox's
// group" case adopts group 1 — which is exactly the group a containerised
// Moombox leads.
func TestPGroupJobAdoptsOnlyAProcessThatLeadsItsOwnGroup(t *testing.T) {
	t.Run("leads its own group", func(t *testing.T) {
		fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})
		job := &pgroupJob{}
		if err := job.adopt(setupGroupPid); err != nil {
			t.Fatalf("adopt: %v", err)
		}
		if !job.queryable() || job.pgid != setupGroupPid {
			t.Fatalf("adopt left pgid %d, queryable %v", job.pgid, job.queryable())
		}
	})

	t.Run("inherited someone else's group", func(t *testing.T) {
		fakeProcessTable(t, map[int]int{setupGroupPid: 1})
		job := &pgroupJob{}
		if err := job.adopt(setupGroupPid); err == nil {
			t.Fatal("adopt accepted a process that did not lead its own group — " +
				"killGroup would later SIGKILL group 1")
		}
		if job.queryable() {
			t.Fatalf("a refused adopt still left pgid %d", job.pgid)
		}
	})

	t.Run("not in the table at all", func(t *testing.T) {
		fakeProcessTable(t, map[int]int{})
		job := &pgroupJob{}
		if err := job.adopt(setupGroupPid); err == nil {
			t.Fatal("adopt accepted a pid the process table does not contain")
		}
	})

	t.Run("unreadable table", func(t *testing.T) {
		unreadableProcessTable(t)
		job := &pgroupJob{}
		if err := job.adopt(setupGroupPid); err == nil {
			t.Fatal("adopt accepted a group it could not confirm")
		}
	})

	t.Run("nonsense pid", func(t *testing.T) {
		fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})
		job := &pgroupJob{}
		if err := job.adopt(0); err == nil {
			t.Fatal("adopt accepted pid 0 — kill(-0) is Moombox's own group")
		}
	})
}

// TestPGroupJobNeverKillsWhatItCannotSee covers all three refusals in killGroup.
// Each is an invariant, not a nicety.
//
// Mutations, one per subtest: drop the queryable() guard and pgid 0 reaches the
// hook, i.e. kill(-0) — Moombox's own group; drop the error check and an
// unreadable /proc fires a blind signal at a number; drop the `active == 0`
// check and every reap fires a signal at a group id the kernel may since have
// recycled onto a stranger.
func TestPGroupJobNeverKillsWhatItCannotSee(t *testing.T) {
	t.Run("no group adopted", func(t *testing.T) {
		killed := captureGroupKills(t)
		fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})
		if err := (&pgroupJob{}).killGroup(); err != nil {
			t.Fatalf("killGroup: %v", err)
		}
		if len(*killed) != 0 {
			t.Fatalf("killGroup signalled %v with no group adopted", *killed)
		}
	})

	t.Run("table unreadable", func(t *testing.T) {
		killed := captureGroupKills(t)
		want := unreadableProcessTable(t)
		if err := (&pgroupJob{pgid: setupGroupPid}).killGroup(); !errors.Is(err, want) {
			t.Fatalf("killGroup error = %v, want the table's own error", err)
		}
		if len(*killed) != 0 {
			t.Fatalf("killGroup signalled %v with an unreadable process table", *killed)
		}
	})

	t.Run("group already empty", func(t *testing.T) {
		killed := captureGroupKills(t)
		fakeProcessTable(t, map[int]int{strangerPid: strangerGroup})
		if err := (&pgroupJob{pgid: setupGroupPid}).killGroup(); err != nil {
			t.Fatalf("killGroup: %v", err)
		}
		if len(*killed) != 0 {
			t.Fatalf("killGroup signalled %v at an empty group", *killed)
		}
	})
}

// TestPGroupJobKillsALiveGroupOnce is the positive case the three refusals
// above exist to protect: when the table says the group still has members, one
// signal goes to that group and to no other.
//
// Mutation: signal j.pgid+1, or signal every key in the table, and the test
// names the group it did not mean to kill.
func TestPGroupJobKillsALiveGroupOnce(t *testing.T) {
	killed := captureGroupKills(t)
	fakeProcessTable(t, map[int]int{
		setupGroupPid: setupGroupPid,
		setupChildPid: setupGroupPid,
		strangerPid:   strangerGroup,
	})

	if err := (&pgroupJob{pgid: setupGroupPid}).killGroup(); err != nil {
		t.Fatalf("killGroup: %v", err)
	}
	if len(*killed) != 1 || (*killed)[0] != setupGroupPid {
		t.Fatalf("killGroup signalled %v, want exactly [%d]", *killed, setupGroupPid)
	}
}

// TestPGroupJobForgetsWithoutKilling is R4 at the type level: close() on Linux
// drops a number, it does not signal. The sites that need a kill ask for one.
//
// Mutation: make forget() call killGroup and trackedSetupJob's failed-assign
// close becomes a SIGKILL of the browser window the user is signed into.
func TestPGroupJobForgetsWithoutKilling(t *testing.T) {
	killed := captureGroupKills(t)
	fakeProcessTable(t, map[int]int{setupGroupPid: setupGroupPid})
	job := &pgroupJob{pgid: setupGroupPid}

	job.forget()

	if job.queryable() {
		t.Fatal("forget() left a group behind")
	}
	if len(*killed) != 0 {
		t.Fatalf("forget() signalled %v; closing a Linux job must kill nothing", *killed)
	}
}
