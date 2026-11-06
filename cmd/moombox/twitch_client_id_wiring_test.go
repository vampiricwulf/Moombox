package main

import (
	"os"
	"strings"
	"testing"
)

// TestTwitchClientIDIsInstalledAtBoot pins, by source, that initServices
// installs `[twitch] client_id` (G70) before it builds the Twitch service —
// ahead of the first GQL request — since the setting is restart-required and
// nothing else applies it.
//
// Mutant: the SetClientID call removed, or moved below twitch.NewService.
func TestTwitchClientIDIsInstalledAtBoot(t *testing.T) {
	src, err := os.ReadFile("services.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	set := strings.Index(text, "twitch.SetClientID(cfg.Twitch.ClientID)")
	build := strings.Index(text, "twService := twitch.NewService(jar, log)")
	if set < 0 || build < 0 || set > build {
		t.Errorf("initServices must call twitch.SetClientID(cfg.Twitch.ClientID) before twitch.NewService (set at %d, build at %d)", set, build)
	}
}
