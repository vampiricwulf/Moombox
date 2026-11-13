package cookies

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lastErrorOf reads the published "Last cookie error", "" when there is none.
func lastErrorOf(s *AutoCookieService) string {
	if e := s.GetStatus().LastError; e != nil {
		return *e
	}
	return ""
}

// A container's only repair is the paste. An import that failed on the file
// and a later one that wrote, loaded and verified must not leave the first
// one's sentence standing beside an Authenticated badge until restart (R2-C3).
func TestAVerifiedImportClearsTheImportsOwnEarlierFileError(t *testing.T) {
	s, _, _ := importService(t, "", true)

	orig := writeCookieFile
	writeCookieFile = func(string, []byte, os.FileMode) error { return errors.New("read-only file system") }
	_, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
	writeCookieFile = orig
	if err == nil {
		t.Fatal("the import with a failing write returned no error")
	}
	if lastErrorOf(s) == "" {
		t.Fatal("a failed write recorded no last cookie error; the fixture no longer reaches that exit")
	}

	if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}
	if got := lastErrorOf(s); got != "" {
		t.Errorf("a verified import left the earlier file error standing: %q", got)
	}
}

// The kind split is the whole safety of the clear: a pasted file says nothing
// about whether the browser refresh works, so a mechanism message stays.
func TestAVerifiedImportKeepsAMechanismError(t *testing.T) {
	s, _, _ := importService(t, "", true)
	const msg = "the browser profile contained no cookies to refresh from"
	s.setError(msg)

	if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
		t.Fatalf("ImportCookies: %v", err)
	}
	if got := lastErrorOf(s); got != msg {
		t.Errorf("a verified import changed a report about the refresh mechanism: got %q, want %q", got, msg)
	}
}

func TestAVerifiedImportClearsACredentialErrorOnlyForThePlatformsItVerified(t *testing.T) {
	const both = "YouTube + Twitch auth verification failed — manual re-login required"

	t.Run("one of two platforms verified keeps the line", func(t *testing.T) {
		s, _, _ := importService(t, "", true)
		s.setCredentialError(both, "YouTube", "Twitch")
		if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
			t.Fatalf("ImportCookies: %v", err)
		}
		if got := lastErrorOf(s); got != both {
			t.Errorf("a YouTube-only paste retracted a line that also reports Twitch: got %q", got)
		}
		// Twitch then recovers through the background re-check.
		if !s.ClearCredentialError("twitch") {
			t.Error("the recovery of the last named platform did not clear the line")
		}
		if got := lastErrorOf(s); got != "" {
			t.Errorf("the line is still up after both platforms verified: %q", got)
		}
	})

	t.Run("the named platform verified clears it", func(t *testing.T) {
		s, _, _ := importService(t, "", true)
		s.setCredentialError("YouTube auth verification failed — manual re-login required", "YouTube")
		if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
			t.Fatalf("ImportCookies: %v", err)
		}
		if got := lastErrorOf(s); got != "" {
			t.Errorf("a verified YouTube import left YouTube's credential error standing: %q", got)
		}
	})

	t.Run("an accepted but unverified paste clears nothing", func(t *testing.T) {
		s, _, _ := importService(t, "", true)
		s.VerifyYouTubeAuth = func(context.Context) (bool, error) { return false, errors.New("rate limited") }
		const msg = "YouTube auth verification failed — manual re-login required"
		s.setCredentialError(msg, "YouTube")
		if _, err := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows); err != nil {
			t.Fatalf("ImportCookies: %v", err)
		}
		if got := lastErrorOf(s); got != msg {
			t.Errorf("an import whose check was inconclusive retracted the line: got %q", got)
		}
	})
}

// The auth-recovered edge clears a credential message for its platform and
// nothing else: not a file message, not a mechanism one, not another platform's.
func TestClearCredentialErrorIsLimitedToItsKindAndPlatform(t *testing.T) {
	s, _, _ := importService(t, "", true)

	s.setError("browser profile in use by host")
	if s.ClearCredentialError("youtube") || lastErrorOf(s) == "" {
		t.Error("an auth recovery cleared a mechanism-kind message")
	}
	s.setFileError("could not write cookies.txt")
	if s.ClearCredentialError("youtube") || lastErrorOf(s) == "" {
		t.Error("an auth recovery cleared a file-kind message")
	}
	s.setCredentialError(cookiesLostMessage([]string{"Twitch"}), "Twitch")
	if s.ClearCredentialError("youtube") || !strings.Contains(lastErrorOf(s), "Twitch") {
		t.Error("YouTube's recovery cleared a message about Twitch")
	}
	if !s.ClearCredentialError("twitch") || lastErrorOf(s) != "" {
		t.Error("Twitch's recovery did not clear the message about Twitch")
	}
	// A later mechanism message must not inherit the cleared one's kind.
	s.setCredentialError("YouTube auth verification failed — manual re-login required", "YouTube")
	s.setError("no browser found for refresh, and no browser profile to import from")
	if s.ClearCredentialError("youtube") || lastErrorOf(s) == "" {
		t.Error("a mechanism message recorded over a credential one was cleared as if it were one")
	}
}

// The refresh's "manual re-login required" sentence is the credential message
// an operator on a container most often has standing. This drives the real
// refresh to that exit and checks the recovery edge can retract what it wrote.
func TestTheRefreshReloginErrorIsRetractedByThatPlatformsRecovery(t *testing.T) {
	pointTwitchValidateAt(t, statusServer(t, http.StatusUnauthorized))

	profileDir := writeWALCookieProfile(t, twitchOnlyRows(staleTwitchToken))
	cookiePath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiePath, []byte(twitchOnlyCookieFile), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewAutoCookieService(profileDir, cookiePath, NewCookieJar(), nopAutoCookieLogger{})
	s.detectBrowser = func() *DetectedBrowser { return nil }
	s.VerifyTwitchAuth = NewRefreshService(s.jar, 0, nopLogger{}).CheckTwitchAuth

	if _, err := s.RefreshCookies(context.Background()); err != nil {
		t.Fatalf("RefreshCookies: %v", err)
	}
	if !strings.Contains(lastErrorOf(s), "manual re-login required") {
		t.Fatalf("the fixture no longer reaches the re-login exit: %q", lastErrorOf(s))
	}
	if s.ClearCredentialError("youtube") {
		t.Error("YouTube's recovery retracted a sentence about Twitch")
	}
	if !s.ClearCredentialError("twitch") || lastErrorOf(s) != "" {
		t.Errorf("Twitch's recovery left the re-login sentence standing: %q", lastErrorOf(s))
	}
}
