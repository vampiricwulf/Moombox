package cookies

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// The two generations of the same credential, so a verify stub can answer from
// the jar's LIVE value and therefore say something different before and after
// the write. That is the only way to exercise a REGRESSION — "it worked, and
// then this import broke it" — rather than a flat failure, and it is the
// technique the browser path's rollback tests already use (goodTwitchToken /
// staleTwitchToken in autocookies_profile_rollback_test.go).
//
// Obviously fake values throughout, as every fixture in this package is.
const (
	previousTwitchToken = "fake-authtoken-previous"
	pastedTwitchToken   = "fake-authtoken-pasted"
	previousSAPISID     = "fake-sapisid-previous"
	pastedSAPISID       = "fake-sapisid-pasted"
)

// importRollbackSeed is a HEALTHY cookies.txt: both platforms configured, both
// working. Expiry 2000000000 on every row because mergeCookieFiles and
// restorePlatformRows both prune on expiry — an expired "previous" row could
// not be given back even by a correct rollback, so the fixture would be testing
// the prune rather than the rollback.
const importRollbackSeed = netscapeHeader +
	".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\t" + previousSAPISID + "\n" +
	"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tfake-logininfo-previous\n" +
	"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t2000000000\tauth-token\t" + previousTwitchToken + "\n" +
	".twitch.tv\tTRUE\t/\tFALSE\t2000000000\tlogin\tfake-login-previous\n"

// importRollbackPaste carries BOTH platforms, with the same name+domain+path
// keys the seed uses so mergeCookieFiles lets every pasted row win. The Twitch
// half is the dead one; the YouTube half is good.
const importRollbackPaste = netscapeHeader +
	".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\t" + pastedSAPISID + "\n" +
	"#HttpOnly_.twitch.tv\tTRUE\t/\tTRUE\t2000000000\tauth-token\t" + pastedTwitchToken + "\n" +
	".twitch.tv\tTRUE\t/\tFALSE\t2000000000\tlogin\tfake-login-pasted\n"

// twitchLiveFromJar is the verify seam every test in this file drives: it
// answers from the jar rather than from a call counter, so the pre-write check
// and the post-write check differ because the CREDENTIALS differ, which is the
// thing under test.
func twitchLiveFromJar(s *AutoCookieService) func(context.Context) (bool, error) {
	return func(context.Context) (bool, error) {
		return s.jar.GetTwitchAuthToken() == previousTwitchToken, nil
	}
}

// TestImportRollsBackAPlatformThePasteKilled is finding F6 in one sentence: a
// paste whose rows for one platform are dead used to REPLACE that platform's
// working rows, be reported as failed, and leave the operator with a broken
// session and no way back — while the sibling platform's fresh rows were kept
// and made the whole import look partially successful.
//
// The refresh path has protected against exactly this since Arc 8 (a mounted
// profile whose Twitch token is stale overwriting a good one); the operator's
// pasted import, the one gesture a container has, did not.
//
// The mutants: (a) skipping the rollback — the dead pasted token stays on disk
// and in the jar; (c) taking the pre-write snapshot AFTER the write — the
// "previous" credentials are then the dead ones, `before.ok()` is false, and
// nothing is ever given back.
func TestImportRollsBackAPlatformThePasteKilled(t *testing.T) {
	s, path, jar := importService(t, importRollbackSeed, true)
	s.VerifyTwitchAuth = twitchLiveFromJar(s)

	result, err := s.ImportCookies(context.Background(), importRollbackPaste)
	if err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back cookies.txt: %v", err)
	}
	got := string(onDisk)
	if !strings.Contains(got, previousTwitchToken) {
		t.Error("the working Twitch row is gone — the dead paste replaced a credential that authenticates")
	}
	if strings.Contains(got, pastedTwitchToken) {
		t.Error("the rejected Twitch row survived the rollback")
	}
	if !strings.Contains(got, pastedSAPISID) {
		t.Error("the YouTube half was rolled back too — the rollback is per platform, not all-or-nothing")
	}
	if jar.GetTwitchAuthToken() != previousTwitchToken {
		t.Errorf("the live jar holds %q — the restored file was not loaded back", jar.GetTwitchAuthToken())
	}

	if result.TwitchOutcome != ImportRolledBack {
		t.Errorf("TwitchOutcome = %v, want %v — the caller's toast would say the paste was imported",
			result.TwitchOutcome, ImportRolledBack)
	}
	if result.YouTubeOutcome != ImportInstalled {
		t.Errorf("YouTubeOutcome = %v, want %v", result.YouTubeOutcome, ImportInstalled)
	}
	if !result.RollbackProtected {
		t.Error("RollbackProtected is false on an import that actually rolled a platform back")
	}
	// The re-verification of what was KEPT, not of what was rejected: after a
	// rollback the credentials in force are the previous ones, and they work.
	if result.Twitch != RefreshOK || !result.TwitchAccepted {
		t.Errorf("Twitch verdict = %v accepted = %v after the restore — the result describes the "+
			"discarded paste rather than the file that was kept", result.Twitch, result.TwitchAccepted)
	}
}

// TestImportWithNothingToProtectSaysSo is the first-acquisition case, which is
// every fresh container: there is no cookies.txt, so no snapshot can be taken,
// so no rollback is possible — and none is needed, because nothing can be lost.
//
// The claim is that this is REPORTED rather than silently indistinguishable
// from a protected import, and that it produces no warning about protection
// being off: "off" is alarming, and here there is simply nothing to protect.
//
// The mutant: reporting RollbackProtected from "did the pre-write load
// succeed" alone. jar.Load answers nil for a missing file, so a first
// acquisition would then claim it was protected.
func TestImportWithNothingToProtectSaysSo(t *testing.T) {
	s, log := importServiceWithLog(t, "")

	result, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	if err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}
	if result.RollbackProtected {
		t.Error("RollbackProtected is true although there was no cookies.txt to roll back to")
	}
	if result.YouTubeOutcome != ImportInstalled {
		t.Errorf("YouTubeOutcome = %v, want %v", result.YouTubeOutcome, ImportInstalled)
	}
	if result.TwitchOutcome != ImportUnchanged {
		t.Errorf("TwitchOutcome = %v, want %v — the paste carried no Twitch row, so nothing about "+
			"Twitch changed", result.TwitchOutcome, ImportUnchanged)
	}
	lines := log.all()
	if strings.Contains(lines, "rollback protection is off") {
		t.Error("a first acquisition warned that rollback protection is off — there was nothing to protect")
	}
	if strings.Contains(lines, "restoring the previous credentials") {
		t.Error("a first acquisition claims it restored something")
	}
}

// TestImportKeepsAPasteTheCheckCouldNotEvaluate is the "inconclusive is not a
// failure" rule at the import writer, and it is the rule the whole cookie
// remediation is built on: a 429, a captive portal or a DNS blip is not
// evidence against credentials the operator supplied thirty seconds ago.
//
// It is also the reason this path takes the REGRESSION arm only. credentialAccepted
// already ACCEPTS an inconclusive check for a credential a human just supplied;
// rolling that same credential back off the disk would make the result
// self-contradictory — accepted, and gone.
//
// The mutant (b): using platformsToRestore, the mounted-profile policy, whose
// second arm restores on any inconclusive check over a platform that had
// credentials. The paste is then discarded on every network blip.
func TestImportKeepsAPasteTheCheckCouldNotEvaluate(t *testing.T) {
	s, log := importServiceWithLog(t, importRollbackSeed)
	// Conclusive OK on the previous token, and a NON-ANSWER on the pasted one:
	// the site could not be reached, so nothing at all was learned about it.
	s.VerifyTwitchAuth = func(context.Context) (bool, error) {
		if s.jar.GetTwitchAuthToken() == previousTwitchToken {
			return true, nil
		}
		return false, errors.New("simulated rate limit")
	}

	result, err := s.ImportCookies(context.Background(), importRollbackPaste)
	if err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}

	onDisk, err := os.ReadFile(s.cookiePath)
	if err != nil {
		t.Fatalf("read back cookies.txt: %v", err)
	}
	if !strings.Contains(string(onDisk), pastedTwitchToken) {
		t.Error("the pasted Twitch row was rolled back over a check that concluded nothing")
	}
	if result.TwitchOutcome != ImportInstalled {
		t.Errorf("TwitchOutcome = %v, want %v", result.TwitchOutcome, ImportInstalled)
	}
	if result.Twitch != RefreshUnknown {
		t.Errorf("Twitch verdict = %v, want unknown — the check never answered", result.Twitch)
	}
	if !result.TwitchAccepted {
		t.Error("the pasted Twitch credential was not accepted although it is what is now in use")
	}
	if strings.Contains(log.all(), "restoring the previous credentials") {
		t.Error("an inconclusive check triggered a rollback")
	}
}

// TestImportSaysWhenRollbackProtectionIsOff. The snapshot is taken by loading
// the file about to be replaced and asking both platforms; when that load
// fails, whatever the check then answers is about the jar's previous contents
// rather than about the file, and it is not a basis for handing rows back.
//
// The import must still PROCEED — refusing would throw away credentials the
// operator supplied by hand for the sake of a read that failed — and must say
// that it went in unprotected, in the result and in the log, using the same
// sentence the refresh path uses for the same condition.
//
// The condition is reproduced with a DIRECTORY where cookies.txt belongs: the
// readCookieFile seam answers with the seed (so there is something to protect),
// CookieJar.Load's own os.ReadFile fails on the same path, and the stubbed
// write replaces the directory with a real file so everything past the write —
// including the post-write load — behaves normally. The same technique as
// TestImportFailurePathsCarryNoValue's third subtest.
//
// The mutant: dropping the flag and reporting protection from `existing != ""`
// alone, which is what an import over an unreadable jar would then claim.
func TestImportSaysWhenRollbackProtectionIsOff(t *testing.T) {
	s, log := importServiceWithLog(t, importRollbackSeed)
	s.VerifyTwitchAuth = twitchLiveFromJar(s)

	if err := os.Remove(s.cookiePath); err != nil {
		t.Fatalf("remove the seed file: %v", err)
	}
	if err := os.Mkdir(s.cookiePath, 0o755); err != nil {
		t.Fatalf("put a directory where cookies.txt belongs: %v", err)
	}
	restoreRead := readCookieFile
	readCookieFile = func(string) ([]byte, error) { return []byte(importRollbackSeed), nil }
	t.Cleanup(func() { readCookieFile = restoreRead })
	restoreWrite := writeCookieFile
	writeCookieFile = func(path string, data []byte, perm os.FileMode) error {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		return os.WriteFile(path, data, perm)
	}
	t.Cleanup(func() { writeCookieFile = restoreWrite })

	result, err := s.ImportCookies(context.Background(), importRollbackPaste)
	if err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}
	if !result.Wrote {
		t.Fatal("the import did not proceed — an unreadable jar must not cost the operator their paste")
	}
	if result.RollbackProtected {
		t.Error("RollbackProtected is true although the pre-write load failed")
	}
	if !strings.Contains(log.all(), "rollback protection is off") {
		t.Errorf("the log never says protection was off:\n%s", log.all())
	}
	onDisk, err := os.ReadFile(s.cookiePath)
	if err != nil {
		t.Fatalf("read back cookies.txt: %v", err)
	}
	if !strings.Contains(string(onDisk), pastedTwitchToken) {
		t.Error("something was rolled back off a snapshot that could not be taken")
	}
	// Conclusively rejected, with nothing established to give back. That is a
	// different state from both "imported" and "rolled back", and the caller
	// has to be able to tell them apart.
	if result.TwitchOutcome != ImportRejected {
		t.Errorf("TwitchOutcome = %v, want %v", result.TwitchOutcome, ImportRejected)
	}
}

// TestImportRollbackThatDoesNotLandIsNotReportedAsOne is Arc 9's rule at this
// writer, and the reason both exits carry a sentinel: reaching either means
// cookies.txt and the running jar disagree about which generation of
// credentials is in force, and the two sentences below are the only place that
// is said.
//
// Both are the SAME wrapped error value, returned AND recorded, so whoever
// renders the failure renders the truth. Before this the exits returned a
// terse "restore previous cookies: %w" and kept the sentence in lastError
// alone — where the dialog that caused the import never looks — and the route,
// finding nothing it recognised, fell through to its `result.Wrote` arm and
// answered "the cookies were imported and written". False on both.
//
// The mutants: returning the bare cause without the sentinel (the route's
// truthful arm never fires); recording the sentence in lastError but returning
// something else (the dialog and Settings disagree about what happened).
func TestImportRollbackThatDoesNotLandIsNotReportedAsOne(t *testing.T) {
	t.Run("the restore write fails", func(t *testing.T) {
		s, _ := importServiceWithLog(t, importRollbackSeed)
		s.VerifyTwitchAuth = twitchLiveFromJar(s)

		real := writeCookieFile
		writes := 0
		writeCookieFile = func(path string, data []byte, perm os.FileMode) error {
			writes++
			if writes == 1 {
				return real(path, data, perm)
			}
			return errors.New("rename temp cookie file: device or resource busy")
		}
		t.Cleanup(func() { writeCookieFile = real })

		result, err := s.ImportCookies(context.Background(), importRollbackPaste)
		if !errors.Is(err, ErrImportRollbackIncomplete) {
			t.Fatalf("error = %v, want it to wrap ErrImportRollbackIncomplete", err)
		}
		if !result.Wrote {
			t.Error("Wrote is false although cookies.txt was replaced — the caller's re-check is gated on it")
		}
		if !strings.Contains(err.Error(), "still holds the rejected new credentials") {
			t.Errorf("error = %q — it does not say what is on disk, which is the only actionable half", err)
		}
		if got := lastErrorSnapshot(s); got != err.Error() {
			t.Errorf("lastError = %q but the caller was handed %q — Settings and the dialog that "+
				"caused this would describe different events", got, err)
		}
	})

	t.Run("the restore lands and the jar cannot read it back", func(t *testing.T) {
		s, _ := importServiceWithLog(t, importRollbackSeed)
		s.VerifyTwitchAuth = twitchLiveFromJar(s)

		// A DIRECTORY where the restored file belongs: the write reports
		// success and CookieJar.Load's own os.ReadFile then fails on that same
		// path. The one exit where the FILE is correct and the process is not.
		real := writeCookieFile
		writes := 0
		writeCookieFile = func(path string, data []byte, perm os.FileMode) error {
			writes++
			if writes == 1 {
				return real(path, data, perm)
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Mkdir(path, 0o755)
		}
		t.Cleanup(func() { writeCookieFile = real })

		_, err := s.ImportCookies(context.Background(), importRollbackPaste)
		if !errors.Is(err, ErrImportRollbackIncomplete) {
			t.Fatalf("error = %v, want it to wrap ErrImportRollbackIncomplete", err)
		}
		if !strings.Contains(err.Error(), "this process is still using the rejected credentials") {
			t.Errorf("error = %q — the two exits are different states and this one says the process "+
				"is stale, not that the file is wrong", err)
		}
		if got := lastErrorSnapshot(s); got != err.Error() {
			t.Errorf("lastError = %q, want the same sentence the caller was handed (%q)", got, err)
		}
	})
}
