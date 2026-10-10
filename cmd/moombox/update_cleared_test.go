package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/updater"
	"github.com/vampiricwulf/Moombox/internal/web/routes"
)

// The TUI learns a pending release is withdrawn from an UpdateStatusMsg with
// an empty Version and the withdrawn tag — it drops its badge only when the
// tag names the release it shows. A missing tag would match nothing, and a
// Version would relight the badge instead.
//
// Mutant: announceUpdateCleared sending the message without the tag.
func TestAnnounceUpdateClearedTellsTheTUIWhichTag(t *testing.T) {
	ch := make(chan tui.UpdateStatusMsg, 1)
	announceUpdateCleared(nil, ch, "v9.9.9")
	select {
	case msg := <-ch:
		if msg.TagName != "v9.9.9" || msg.Version != "" {
			t.Errorf("TUI message = %+v, want a clear (no Version) naming v9.9.9", msg)
		}
	default:
		t.Fatal("no message reached the TUI")
	}
}

// An up-to-date answer withdraws the release that was pending BEFORE the
// check, on both of cmd's check paths — the daily periodic check and the
// TUI's R V — and only that one: a release another check stored during the
// round trip survives, with no clear sent. The routes' own clear is tested
// in its package; these are the two call sites.
//
// R V's answer is what the server holds after it: nothing once the pulled
// release is withdrawn, and the release found during the round trip when it
// survives — answered "up to date" there, the TUI dropped the badge for a
// release the server and every dashboard still offered.
//
// Mutants, for either path: drop the ClearPendingUpdate call — the pulled
// release stays pending; load what it may withdraw after the check instead of
// before — the release found during the round trip is withdrawn. For R V:
// answer (nil, nil) on every up-to-date answer — the kept release is not the
// answer.
func TestUpToDateChecksWithdrawOnlyTheReleaseTheySaw(t *testing.T) {
	log, err := logger.New(filepath.Join(t.TempDir(), "update.log"), "error", 4096, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	prev := routes.SharedUpdateInfo.Load()
	t.Cleanup(func() { routes.SharedUpdateInfo.Store(prev) })
	orig := checkForUpdate
	t.Cleanup(func() { checkForUpdate = orig })

	// Each path's answer, as the TUI gets it: the periodic check answers no
	// one (its result is what it sends on the channel), so it reports "".
	paths := map[string]func(t *testing.T, ch chan tui.UpdateStatusMsg) (answer string){
		"periodic": func(_ *testing.T, ch chan tui.UpdateStatusMsg) string {
			var lastTag string
			checkAndBroadcastUpdate(context.Background(), nil, nil, nil, ch, log, nil, &lastTag)
			return ""
		},
		"tui": func(t *testing.T, ch chan tui.UpdateStatusMsg) string {
			s := &runState{log: log, tuiUpdateStatusCh: ch}
			msg, err := s.checkUpdateFromTUI()
			if err != nil {
				t.Fatalf("R V: %v", err)
			}
			if msg == nil {
				return ""
			}
			if msg.Version == "" {
				t.Errorf("R V answered %+v — a release with no version reads as a clear", msg)
			}
			return msg.TagName
		},
	}
	for name, check := range paths {
		t.Run(name+" withdraws the pulled release", func(t *testing.T) {
			pulled := &updater.ReleaseInfo{TagName: "v9.9.1", Version: "9.9.1"}
			routes.SharedUpdateInfo.Store(pulled)
			checkForUpdate = func(*updater.Updater, context.Context) (*updater.ReleaseInfo, error) { return nil, nil }
			ch := make(chan tui.UpdateStatusMsg, 1)
			if answer := check(t, ch); answer != "" {
				t.Errorf("answered %s, want up to date", answer)
			}
			if got := routes.SharedUpdateInfo.Load(); got != nil {
				t.Errorf("pending after an up-to-date answer = %s, want none", got.TagName)
			}
			select {
			case msg := <-ch:
				if msg.TagName != "v9.9.1" {
					t.Errorf("clear names %q, want v9.9.1", msg.TagName)
				}
			default:
				t.Error("no clear reached the TUI")
			}
		})
		t.Run(name+" keeps a release found during it", func(t *testing.T) {
			routes.SharedUpdateInfo.Store(&updater.ReleaseInfo{TagName: "v9.9.1", Version: "9.9.1"})
			found := &updater.ReleaseInfo{TagName: "v9.9.2", Version: "9.9.2"}
			checkForUpdate = func(*updater.Updater, context.Context) (*updater.ReleaseInfo, error) {
				routes.SharedUpdateInfo.Store(found) // another check, over this one's round trip
				return nil, nil                      // a stale "up to date"
			}
			ch := make(chan tui.UpdateStatusMsg, 1)
			answer := check(t, ch)
			if got := routes.SharedUpdateInfo.Load(); got != found {
				t.Errorf("pending = %v, want the release found during the check", got)
			}
			if name == "tui" && answer != "v9.9.2" {
				t.Errorf("R V answered %q, want v9.9.2, the release the server still holds", answer)
			}
			select {
			case msg := <-ch:
				t.Errorf("a clear for %q reached the TUI, want none", msg.TagName)
			default:
			}
		})
	}
}
