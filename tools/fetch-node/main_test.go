package main

import (
	"strings"
	"testing"
)

func TestVersionStampIncludesAllPlatforms(t *testing.T) {
	stamp := versionStamp()
	for _, platform := range []string{"windows-amd64", "linux-amd64", "linux-arm64"} {
		if !strings.Contains(stamp, platform) {
			t.Errorf("versionStamp missing platform %q: %s", platform, stamp)
		}
	}
}

func TestNodeTargetsCoverAllPlatforms(t *testing.T) {
	targets := nodeTargets()
	if len(targets) != 3 {
		t.Fatalf("expected 3 targets, got %d", len(targets))
	}
	gotPlatforms := map[string]bool{}
	for _, tgt := range targets {
		key := tgt.goos + "-" + tgt.goarch
		gotPlatforms[key] = true
		if tgt.expectedSHA == "" {
			t.Errorf("target %s has empty expectedSHA", key)
		}
		if tgt.embedName == "" {
			t.Errorf("target %s has empty embedName", key)
		}
	}
	for _, want := range []string{"windows-amd64", "linux-amd64", "linux-arm64"} {
		if !gotPlatforms[want] {
			t.Errorf("missing target for %s", want)
		}
	}
}
