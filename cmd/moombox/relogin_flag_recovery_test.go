package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/logger"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestAuthRecoveryLowersTheReloginFlag: the "re-login required" flag raised by
// handleRecoveryNeeded was lowered only by a browser refresh, setup or import,
// never by the RefreshService's own recovery edge — a cookies.txt replaced by
// hand left both dashboards on "Re-login required" until a restart.
//
// Mutant: drop the ClearManualRelogin call in OnAuthRecovered.
func TestAuthRecoveryLowersTheReloginFlag(t *testing.T) {
	dir := t.TempDir()
	log, err := logger.New(filepath.Join(dir, "p.log"), "error", 4096, 1)
	if err != nil {
		t.Fatal(err)
	}
	log.SuppressStdout()
	t.Cleanup(log.Close)
	db, err := database.Open(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	jar := cookies.NewCookieJar()
	rec := notificationtest.New()
	s := &runState{
		log:           log,
		db:            db,
		cookieRefresh: cookies.NewRefreshService(jar, time.Hour, log),
		autoCookieSvc: cookies.NewAutoCookieService(dir, filepath.Join(dir, "cookies.txt"), jar, log),
		notifyMgr:     rec,
	}
	s.wireCredentialRepairCallbacks(func() int { return 0 }, func() int { return 0 }, func(string) bool { return true })

	// handleRecoveryNeeded's disabled / failed-recovery arm.
	s.autoCookieSvc.FlagManualRelogin("youtube")
	// The operator overwrites cookies.txt by hand (the guidance's own
	// alternative); the next RefreshService pass validates and fires:
	s.cookieRefresh.OnAuthRecovered("youtube")

	if s.autoCookieSvc.ReloginStatus()["youtube"] {
		t.Error("auth recovered but youtube still needs a re-login")
	}
}
