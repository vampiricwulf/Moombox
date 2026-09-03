package cookies

import (
	"errors"
	"os"
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

// TestPGroupJobSecondAdoptReplacesTheGroup pins adopt's last-wins behaviour: a
// second adopt call on the same job overwrites the first, rather than being
// refused or merged. Nothing in production relies on this — killProcessTreeUnix
// builds a fresh *pgroupJob per call, and processJob.assign adopts once — but
// the type itself has no guard against a second call, and this is the
// assertion that would catch it silently changing.
//
// Mutation: make the second adopt a no-op (skip the assignment when a group is
// already tracked) and the job is left holding group 100 instead of 200.
func TestPGroupJobSecondAdoptReplacesTheGroup(t *testing.T) {
	fakeProcessTable(t, map[int]int{100: 100, 200: 200})
	job := &pgroupJob{}
	if err := job.adopt(100); err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	if err := job.adopt(200); err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	if job.pgid != 200 {
		t.Fatalf("pgid = %d, want 200 (last adopt wins)", job.pgid)
	}
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

// captureOneProcessKills swaps the single-process fallback for a recorder. The
// package's rule is that a fabricated PID must never reach a real signal on the
// machine running the tests; killProcessTree has been a variable for that
// reason since it was written, and its fallback needs the same treatment.
func captureOneProcessKills(t *testing.T) *[]int {
	t.Helper()
	prev := killOneProcess
	killed := []int{}
	killOneProcess = func(p *os.Process) error {
		if p != nil {
			killed = append(killed, p.Pid)
		}
		return nil
	}
	t.Cleanup(func() { killOneProcess = prev })
	return &killed
}

// TestKillProcessTreeUnixKillsTheGroupNotJustTheLauncher is the reason the
// group exists at all. proc.Kill() kills the process Moombox spawned — which on
// the Firefox family is a launcher that handed off and exited ~170 ms after
// start (Arc 0's measurement). On Linux the tree IS the group, so one
// kill(-pgid) reaches the browser the launcher left behind.
//
// Mutation: put killOneProcess(proc) back as the primary and the recorder shows
// a single PID, which is the bug: the browser survives every cancel, every
// Stop, and every refresh timeout.
func TestKillProcessTreeUnixKillsTheGroupNotJustTheLauncher(t *testing.T) {
	groups := captureGroupKills(t)
	single := captureOneProcessKills(t)
	fakeProcessTable(t, map[int]int{
		setupGroupPid: setupGroupPid, // the leader, still alive
		setupChildPid: setupGroupPid, // what it handed off to
	})

	killProcessTreeUnix(&os.Process{Pid: setupGroupPid})

	if len(*groups) != 1 || (*groups)[0] != setupGroupPid {
		t.Fatalf("group kills = %v, want exactly [%d]", *groups, setupGroupPid)
	}
	if len(*single) != 0 {
		t.Fatalf("it also killed %v directly; the group kill already covers the leader", *single)
	}
}

// TestKillProcessTreeUnixFallsBackWhereThereAreNoProcessGroups keeps darwin and
// every other non-Linux, non-Windows target on exactly today's behaviour: the
// table hook is unbound there, it answers errNoProcessTable, adopt refuses,
// and the direct kill is all there ever was. A Linux /proc that cannot be read
// takes the same arm.
//
// Mutation: drop the fallback and a darwin build stops killing browsers
// entirely.
func TestKillProcessTreeUnixFallsBackWhereThereAreNoProcessGroups(t *testing.T) {
	groups := captureGroupKills(t)
	single := captureOneProcessKills(t)
	unreadableProcessTable(t)

	killProcessTreeUnix(&os.Process{Pid: setupGroupPid})

	if len(*groups) != 0 {
		t.Fatalf("group kills = %v; nothing may be signalled through a table that cannot be read", *groups)
	}
	if len(*single) != 1 || (*single)[0] != setupGroupPid {
		t.Fatalf("fallback kills = %v, want exactly [%d]", *single, setupGroupPid)
	}
}

// TestKillProcessTreeUnixWillNotSignalAGroupThePidNoLongerLeads is why the arm
// goes through adopt rather than handing the pid to the hook. killSetupProcess
// reaches here with a REAPED pid whenever no job could vouch for the browser
// (a failed assign), and proc.Kill() was safe there — Go refuses it on a
// process that has been waited on. A bare kill(-pid) has no such memory: it
// fires at whatever group the kernel has since given that number to. So the
// arm signals only a pid that is in the table right now and leads its own
// group, and otherwise falls back to the direct kill, which is harmless on a
// reaped process.
//
// Mutations, one per subtest: replace adopt with a bare killProcessGroup(pid)
// and the first case signals a number the table does not contain; drop the
// `pgid != pid` refusal in adopt and the second case does NOT signal a
// stranger's group — adopt still records the INPUT pid, not the table's
// looked-up value, so killGroup's own empty-group refusal (activeProcesses
// finds no member whose pgid equals that pid) blocks the signal. The observed
// failure is nothing killed, an orphaned browser left running, not a wrong
// group getting SIGKILLed.
func TestKillProcessTreeUnixWillNotSignalAGroupThePidNoLongerLeads(t *testing.T) {
	t.Run("pid is not in the table", func(t *testing.T) {
		groups := captureGroupKills(t)
		single := captureOneProcessKills(t)
		fakeProcessTable(t, map[int]int{strangerPid: strangerGroup})

		killProcessTreeUnix(&os.Process{Pid: setupGroupPid})

		if len(*groups) != 0 {
			t.Fatalf("group kills = %v for a pid the table does not contain", *groups)
		}
		if len(*single) != 1 || (*single)[0] != setupGroupPid {
			t.Fatalf("fallback kills = %v, want exactly [%d]", *single, setupGroupPid)
		}
	})

	t.Run("pid sits in someone else's group", func(t *testing.T) {
		groups := captureGroupKills(t)
		single := captureOneProcessKills(t)
		fakeProcessTable(t, map[int]int{setupGroupPid: strangerGroup, strangerGroup: strangerGroup})

		killProcessTreeUnix(&os.Process{Pid: setupGroupPid})

		if len(*groups) != 0 {
			t.Fatalf("group kills = %v; the pid does not lead that group and it is not ours to signal", *groups)
		}
		if len(*single) != 1 || (*single)[0] != setupGroupPid {
			t.Fatalf("fallback kills = %v, want exactly [%d]", *single, setupGroupPid)
		}
	})
}

// TestKillProcessTreeUnixRefusesANonPositivePid is the pid <= 0 guard as seen
// from this arm. A zero-valued os.Process is not hypothetical: the refresh
// slot is claimed with `&exec.Cmd{}` whose Process is nil until the launcher
// publishes one, and a future caller reaching this with a zero pid must fall
// back rather than signal. The refusal is adopt's; this pins that the arm
// cannot route around it. The table deliberately contains a 0 → 0 row so
// that a dropped guard would "succeed" and the recorder would show it.
//
// Mutation: drop adopt's `pid <= 0` check — the group is adopted as 0,
// killGroup refuses it silently, and the fallback never runs, so the second
// assertion fails; kill(-0, SIGKILL) is Moombox's own process group.
func TestKillProcessTreeUnixRefusesANonPositivePid(t *testing.T) {
	groups := captureGroupKills(t)
	single := captureOneProcessKills(t)
	fakeProcessTable(t, map[int]int{0: 0})

	killProcessTreeUnix(&os.Process{Pid: 0})
	killProcessTreeUnix(nil)

	if len(*groups) != 0 {
		t.Fatalf("group kills = %v; a non-positive pid must never reach the hook", *groups)
	}
	if len(*single) != 1 || (*single)[0] != 0 {
		t.Fatalf("fallback kills = %v, want exactly [0] (and nothing for the nil process)", *single)
	}
}
