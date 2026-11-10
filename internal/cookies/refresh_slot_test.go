package cookies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestTheRefreshSlotNamesItsHolder (K43): the refresh slot records WHO holds
// it — a refresh pass or a cookie import — so a refusal says which. An import
// held by another import, or a setup refused behind an import, used to answer
// "cookie refresh in progress"; it now answers ErrImportInProgress, which
// still matches ErrRefreshInProgress, so every errors.Is mapping (the routes'
// 409s) is unchanged. A pass holding the slot still answers
// ErrRefreshInProgress alone.
//
// Mutants: claim the import as a refresh pass — the import refusals read
// "cookie refresh in progress"; ErrImportInProgress not unwrapping to
// ErrRefreshInProgress — the routes' errors.Is arms miss it (500).
func TestTheRefreshSlotNamesItsHolder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		holder     refreshHolder
		wantImport bool
	}{
		{"an import holds it", refreshHolderImport, true},
		{"a refresh pass holds it", refreshHolderRefresh, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "cookies.txt")
			s := NewAutoCookieService(dir, path, NewCookieJar(), nopAutoCookieLogger{})
			s.mu.Lock()
			s.refresh.claim(tc.holder)
			s.mu.Unlock()

			_, importErr := s.ImportCookies(context.Background(), netscapeHeader+fakeYouTubeRows)
			setupErr := s.StartSetup("youtube")
			for what, err := range map[string]error{"ImportCookies": importErr, "StartSetup": setupErr} {
				if !errors.Is(err, ErrRefreshInProgress) {
					t.Errorf("%s = %v, want it to match ErrRefreshInProgress (the routes' 409)", what, err)
				}
				if got := errors.Is(err, ErrImportInProgress); got != tc.wantImport {
					t.Errorf("%s = %v: errors.Is(ErrImportInProgress) = %v, want %v", what, err, got, tc.wantImport)
				}
				wantText := "cookie refresh in progress"
				if tc.wantImport {
					wantText = "cookie import in progress"
				}
				if err == nil || !strings.Contains(err.Error(), "please try again shortly: "+wantText) {
					t.Errorf("%s = %v, want \"please try again shortly: %s\"", what, err, wantText)
				}
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Errorf("a refused import wrote cookies.txt (stat err = %v)", statErr)
			}
			s.mu.Lock()
			holder := s.refresh.holder
			s.mu.Unlock()
			if holder != tc.holder {
				t.Errorf("the refusals changed the holder to %v, want %v", holder, tc.holder)
			}
		})
	}
}

// TestStopDoesNotWaitOnAnImport (K43): an import holds the refresh slot
// without ever launching a browser, so Stop's killRefreshProcess has nothing
// to wait for and returns at once — it used to poll the launch window for
// launchWindowKillBudget (2 s) on the import's sentinel. A refresh pass in its
// launch window still gets the poll (it may publish a browser a moment later).
//
// Mutant: killRefreshProcess without its import arm — the import case polls.
func TestStopDoesNotWaitOnAnImport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		holder    refreshHolder
		wantPolls bool
	}{
		{"import", refreshHolderImport, false},
		{"refresh pass in its launch window", refreshHolderRefresh, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var polls atomic.Int32
			prev := launchWindowPollSleep
			launchWindowPollSleep = func(time.Duration) { polls.Add(1) }
			t.Cleanup(func() { launchWindowPollSleep = prev })

			s := NewAutoCookieService(t.TempDir(), "", NewCookieJar(), nopAutoCookieLogger{})
			s.mu.Lock()
			s.refresh.claim(tc.holder)
			s.mu.Unlock()
			s.killRefreshProcess()
			if got := polls.Load() > 0; got != tc.wantPolls {
				t.Errorf("killRefreshProcess polled the launch window = %v (%d polls), want %v", got, polls.Load(), tc.wantPolls)
			}
		})
	}
}
