package worker

import (
	"errors"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// cookies.cookie_file is restart-required: saved without the restart, the
// setting names a file the running services never touch. The failed-refresh
// advice named the setting anyway, so the operator replaced the wrong file.
// It names the file in use (CookieFileInUse) now, the setting only as a
// fallback.
//
// Mutant: the advice reading the setting first again.
func TestFailedRefreshAdviceNamesTheCookieFileInUse(t *testing.T) {
	w, _ := testWorkerSetup(t)
	log := &fieldCaptureLogger{}
	w.logger = log
	cfg := config.Defaults()
	cfg.Cookies.CookieFile = "/new/cookies.txt" // saved, restart declined
	w.configStore = config.NewStore(cfg, "")
	w.OnCookieRefreshNeeded = func(string) bool { return false }
	w.CookieFileInUse = func() string { return "/boot/cookies.txt" }

	w.attemptCookieRefresh(&database.Job{ID: "j", VideoID: "v", Platform: "youtube"}, errors.New("sign in to confirm"))

	got, logged := log.field("auto cookie refresh failed — the cookie file has to be replaced by hand", "cookieFile")
	if !logged || got != "/boot/cookies.txt" {
		t.Errorf("advice named %v (logged %v), want the file in use, /boot/cookies.txt", got, logged)
	}
}
