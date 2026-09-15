package cookies

// autocookies_process.go — process lifecycle helpers: killing a setup or
// refresh browser, tearing down per-attempt state, and the Job Object
// tracking around it.

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// refreshChromiumCookies is the Chromium browser-refresh step behind a package
// variable, so a test can exercise what RefreshCookiesDetailed DOES WITH the
// step's result without launching a browser — notably the ErrNoCookiesInProfile
// downgrade in autocookies_refresh.go (refreshCookiesDetailed), which is
// unreachable otherwise: the real function has to start a headless Chromium and
// speak CDP to it before it can report an empty profile, and no test in this
// package may launch a browser.
//
// Same seam convention as detectBrowser, setupBrowserGone, killProcessTree and
// writeCookieFile. Nothing in production reassigns it.
var refreshChromiumCookies = (*AutoCookieService).refreshChromium

// killProcessTree kills a process and all its children on Windows (taskkill /T /F),
// or just the process itself on other platforms.
//
// A package variable purely so tests can exercise the kill DECISION without a
// real process — same reason writeCookieFile in cookie_files.go is one. Nothing
// in production reassigns it, and it is always addressed by PID: never by image
// name, which on a developer's machine would take out their own browser.
var killProcessTree = func(proc *os.Process) {
	if proc == nil {
		return
	}
	if isWindows() {
		exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		killProcessTreeUnix(proc)
	}
}

// killProcessTreeUnix is the non-Windows arm, split out of the closure above so
// a test on ANY platform can execute it directly — calling it by name needs no
// GOOS at all. isWindows() itself IS stubbable: it reads the runtimeGOOS var
// (autocookies_detect.go), which TestDpapiFallbackWarnIsGatedOnWindows swaps to
// drive the non-Windows arm of another caller, so the else branch is reachable
// on the Windows machine this project is developed on. What stays reviewed by
// eye is the one-line wiring in the closure above — the same coverage posture
// startChromiumSetup states in prose for its own trackedSetupJob call.
//
// ON LINUX THE TREE IS THE GROUP. configureCmdSysProcAttr sets Setpgid on every
// browser this package launches, so the child leads a group whose id is its own
// pid, and one kill(-pgid) reaches the browser the launcher handed off to.
// proc.Kill() alone never did: it kills the launcher, which on the Firefox
// family exited ~170 ms after start.
//
// IT GOES THROUGH pgroupJob, NOT STRAIGHT TO THE HOOK, so it inherits adopt's
// and killGroup's refusals: the pid must be in the table right now and lead its
// own group, and the group must still have members. killSetupProcess reaches
// here with a REAPED pid whenever no job could vouch for the browser, and
// proc.Kill() was safe there — Go refuses it on a process that has already
// been waited on (os.ErrProcessDone). A bare kill(-pid) has no such memory; it
// fires at whatever group the kernel has since given that number to.
//
// Everywhere the refusals apply — darwin and the fallback build, where the
// table hook is unbound and answers errNoProcessTable; a Linux /proc that
// cannot be read; a pid that is gone or sits in someone else's group — the
// direct kill below is exactly today's behaviour. The pid <= 0 guard is
// adopt's; see killProcessGroup's doc for the three checks on kill(-0, …).
func killProcessTreeUnix(proc *os.Process) {
	if proc == nil {
		return
	}
	group := &pgroupJob{}
	if err := group.adopt(proc.Pid); err == nil {
		if err := group.killGroup(); err == nil {
			return
		}
	}
	killOneProcess(proc)
}

// killOneProcess is (*os.Process).Kill behind a package variable, for the same
// reason killProcessTree itself is one: the fallback above has to be
// exercisable without a real process, and a fabricated PID must never reach a
// real signal on the machine running the tests. Nothing in production
// reassigns it.
var killOneProcess = func(proc *os.Process) error { return proc.Kill() }

// killSetupProcess terminates the setup browser, waiting briefly for one that
// has been decided on but not yet launched.
//
// The poll is the same fix killRefreshProcess carries for the refresh slot, for
// the identical race in the setup slot. StartSetup claims with `setupClaimed`
// and only assigns setupProcess once the launcher has started the browser, so a
// CancelSetup or Stop landing between the mid-preparation cancel check and that
// assignment used to find nil, kill nothing, and return — and the launcher then
// registered a browser into a slot nobody was watching. The user got a stuck
// wizard and an open browser window.
//
// Deliberately NOT a new flag saying "a launch is in flight": setupClaimed
// already says exactly that, and a second field describing the same window is
// one more thing to get wrong. Capped like the refresh side so a launcher that
// errors before assigning cannot make Stop() hang; past the cap the
// mid-preparation cancel check is what stops the launch, and what is left is a
// sub-second window between that check and cmd.Start() returning.
//
// IT WILL NOT SHELL A TASKKILL AT A PID THAT HAS ALREADY BEEN REAPED. Once the
// spawned process has exited AND a live Job Object is holding whatever it
// handed off to, this PID identifies nothing: Windows recycles PIDs, so the
// kill can only ever land on an unrelated process, and the thing it was meant
// to reach is killed by cleanupLocked closing the job a moment later. That
// last clause is the guard's premise, so it is checked rather than assumed:
// ALL FOUR callers — CancelSetup, Stop, FinishSetup's Chromium branch, and
// startChromiumSetup's CDP-timeout bail — call cleanup() immediately after
// this, so the job always gets its turn. A fifth caller that does not must not
// rely on this.
//
// That state is the NORMAL one on the Firefox family, where the launcher hands
// off and exits in ~170ms, and it is why this guard arrived with the setup Job
// Object rather than before it: until the job existed, killing the stale PID
// was the only thing that even pretended to help. It is the same rule
// runWithTimeout applies with onLauncherReaped, which stops advertising a
// reaped PID for exactly this reason.
//
// It does NOT make killing-by-PID safe in general, and does not try to. Where
// no job can vouch for the browser — a failed assign on either platform, and
// darwin and the fallback build, where queryable() is always false — the kill
// still runs, because there it is the only thing that can work. On Linux that
// kill goes through killProcessTreeUnix, which signals a group only when the
// pid still leads one; a reaped pid falls back to proc.Kill, which Go refuses
// on a process it has already waited on.
func (s *AutoCookieService) killSetupProcess() {
	deadline := time.Now().Add(launchWindowKillBudget)
	for {
		s.mu.Lock()
		proc := s.setupProcess
		claimed := s.setupClaimed
		// Read under the same lock as proc: the question is whether THIS
		// process is still a thing worth killing, and both halves of the answer
		// have to describe the same instant.
		reapedPID := s.browserExited && s.setupJob.queryable()
		s.mu.Unlock()

		if proc != nil {
			if reapedPID {
				return // the job will do it; this PID belongs to whoever Windows gave it to next
			}
			killProcessTree(proc)
			time.Sleep(taskkillDrainDelay)
			return
		}
		if !claimed {
			return // no process, and no launch on the way — nothing to kill
		}
		if !time.Now().Before(deadline) {
			return // still unpublished — give up rather than block the caller
		}
		time.Sleep(killProcessTreePollDelay)
	}
}

func (s *AutoCookieService) killRefreshProcess() {
	// RefreshCookies claims the slot with `s.refreshCmd = &exec.Cmd{}`
	// (a sentinel with nil Process) before refreshFirefox/refreshChromium
	// assigns the real cmd. A naive `Process == nil → bail` lets a Stop()
	// during that window leak the real browser when it lands a moment later
	// (audit reports/cookies.md #22). Poll briefly so the kill catches the
	// real process once the launcher publishes it, but cap the wait so Stop()
	// doesn't block indefinitely if the launcher errors before assignment.
	deadline := time.Now().Add(launchWindowKillBudget)
	for {
		s.mu.Lock()
		cmd := s.refreshCmd
		s.mu.Unlock()

		if cmd == nil {
			return // launcher cleared the slot — nothing to kill
		}
		if cmd.Process != nil {
			killProcessTree(cmd.Process)
			return
		}
		if !time.Now().Before(deadline) {
			return // sentinel still in place — give up rather than block Stop()
		}
		time.Sleep(killProcessTreePollDelay)
	}
}

// cleanup releases the per-attempt setup state so the next StartSetup begins
// from a clean slate. It runs on EVERY setup exit path — success, extraction
// failure, the empty-profile "no login detected" case, each of the mkdir /
// write / jar-load failures, the S9 read-abort that refuses to overwrite an
// unreadable cookies.txt, the Chromium CDP-timeout bail, CancelSetup and Stop
// — and that breadth is precisely why the two decision flags below are NOT in
// the list.
//
// `cancelled` is not reset here. It used to be, and because CancelSetup's own
// last act is to call cleanup(), a complete cancel erased its own flag
// microseconds after raising it: StartSetup's mid-preparation check could
// never observe one. It is cleared at claim time in StartSetup instead, which
// is where a cancel is actually consumed. Do not "fix" a lingering flag by
// putting the reset back here under some condition — that is the same defect
// with extra steps.
//
// `stopped` is not reset here for a stronger reason: Stop() calls cleanup()
// itself, so clearing it would un-stop the service inside the very call that
// stopped it. Nothing may lower that latch.
//
// What does get cleared is only ever state describing a browser that is gone:
// the Job Object handle, the process, the browser record, the exit flag, the
// retention timestamp, the CDP port and the target platform.
func (s *AutoCookieService) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
}

// cleanupLocked is cleanup's body for callers that already hold s.mu — the
// inline reap in reapAbandonedSetupLocked, which must decide and clean up
// inside ONE critical section. See cleanup for what is cleared and, more
// importantly, what is not.
//
// ON WINDOWS, CLOSING THE JOB OBJECT KILLS BROWSERS. That is the point of it —
// KILL_ON_JOB_CLOSE finishes off a setup browser killSetupProcess missed, on
// BOTH families now that startFirefoxSetup creates a job too — but it is also
// why every caller must have established, under the lock it still holds, that
// the slot it is clearing is the slot it looked at. A caller that sampled the
// state, released the lock, and came back later can be looking at a different
// attempt's job, and closing that one kills a browser the user is actively
// using.
//
// IT KILLS ON LINUX TOO NOW, but by a different route: the close only forgets a
// process-group id there, so the killTrackedProcesses call in the body below is
// what reaches the browser. Same consequence, same requirement on callers. On darwin and the
// fallback build job_other.go is still a no-op stub, nothing is tracked and
// nothing is killed; a browser left behind there keeps running (pdeathsig ties
// it to Moombox's death, not to this call).
//
// Caller must hold s.mu.
func (s *AutoCookieService) cleanupLocked() {
	if s.setupJob != nil {
		// BEFORE the close, and before the field is nilled. On Windows the
		// close is itself the kill and this is a no-op; on Linux the close
		// forgets a number, so this is the only thing that reaches the browser.
		// See killTrackedProcesses.
		if err := killTrackedProcesses(s.setupJob); err != nil && s.logger != nil {
			s.logger.Warn("could not kill the setup browser's process group; it may still be running",
				"err", err)
		}
		s.setupJob.close()
		s.setupJob = nil
	}
	s.setupProcess = nil
	s.setupBrowser = nil
	s.browserExited = false
	s.setupRetainedSince = time.Time{}
	s.cdpPort = 0
	s.targetPlatform = ""
}

// assignProcessToJob is processJob.assign behind a package variable, so a test
// can induce the failure trackedSetupJob exists to handle. That failure needs a
// hostile process state — a handle that cannot be opened for
// PROCESS_SET_QUOTA|PROCESS_TERMINATE, or an AssignProcessToJobObject refusal —
// which no test in this package can arrange without launching something. Same
// seam convention as setupBrowserGone, killProcessTree and writeCookieFile;
// nothing in production reassigns it.
var assignProcessToJob = func(job *processJob, p *os.Process) error { return job.assign(p) }

// trackedSetupJob puts a launched setup browser under its Job Object and
// returns the job the setup slot should own — nil when nothing is actually
// being tracked. Both launchers call it, so the decision below is made once.
//
// A JOB THAT TRACKS NOTHING IS WORSE THAN NO JOB. Its handle is live, so
// queryable() is true and activeProcesses() answers 0 — and setupBrowserGone
// reads that pair as a POSITIVE "the browser is gone". The reap would then
// release a setup whose browser is still on screen. It cannot kill that
// browser (nothing is in the job for KILL_ON_JOB_CLOSE to take), so the
// consequence is a premature release: the user's next "I'm logged in" answers
// ErrNoSetupInProgress. Dropping the job instead makes the probe say "no idea",
// which is the honest reading of a failed assign, and leaves the slot alone.
//
// Dropping loses nothing. A job with a failed assign provides neither the
// crash-time cleanup nor the count it was created for, because the launcher's
// children join the launcher's OWN job, not this one.
//
// NOTE THE ASYMMETRY WITH runWithTimeout, which keeps a failed-assign job on
// purpose. Correct there, and for the same underlying rule: drainJob reads its
// zero as "nothing was waited on", never as "the browser finished". The two
// paths differ because their readers differ.
func (s *AutoCookieService) trackedSetupJob(job *processJob, proc *os.Process, family string) *processJob {
	if job == nil {
		return nil
	}
	if err := assignProcessToJob(job, proc); err != nil {
		if s.logger != nil {
			s.logger.Warn("failed to assign setup process to job object — dropping the job "+
				"rather than let an empty one look like a closed browser",
				"family", family, "pid", proc.Pid, "err", err)
		}
		job.close()
		return nil
	}
	return job
}

// adoptSetupJobLocked hands ownership of a freshly created Job Object to the
// setup slot, closing whatever handle an earlier attempt left there. Both
// launchers go through it — startChromiumSetup and startFirefoxSetup — so the
// guard below exists once instead of in two copies free to drift.
//
// NEVER OVERWRITE A LIVE JOB OBJECT HANDLE. Dropping one leaks the handle AND
// the browser it holds: nothing else has a reference, so KILL_ON_JOB_CLOSE
// never fires and the orphan runs until Moombox exits. StartSetup's reap should
// have cleared this already — if it did not, the invariant broke somewhere and
// closing is still the right answer, because the only process such a job can
// hold is a setup browser from an attempt the gate has already declared over.
//
// Closing it cannot touch the browser the caller just launched: that browser
// was assigned to `job`, and a process is only ever in the job it was assigned
// to. Callers must therefore assign BEFORE calling, which both do.
//
// Caller must hold s.mu.
func (s *AutoCookieService) adoptSetupJobLocked(job *processJob) {
	if s.setupJob != nil {
		if s.logger != nil {
			s.logger.Warn("closing a setup Job Object left behind by an earlier attempt")
		}
		if err := killTrackedProcesses(s.setupJob); err != nil && s.logger != nil {
			s.logger.Warn("could not kill the stale setup browser's process group", "err", err)
		}
		s.setupJob.close()
	}
	s.setupJob = job
}

func (s *AutoCookieService) setError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = &msg
}

// isWindows returns true when running on Windows.
func isWindows() bool {
	return runtimeGOOS() == "windows"
}
