package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The Docker image's config seed and its guidance live in a shell heredoc
// (docker/entrypoint.sh), which nothing in CI runs: ci.yml never builds or
// starts the image, so a typo in the seed — `network_acess = "lan"` leaves
// every new install's dashboard bound to 127.0.0.1 and unreachable through
// the published port, while the healthcheck, which connects from inside,
// stays green — passed every suite. These tests read the heredocs out of the
// file the image copies.

// entrypointPath is docker/entrypoint.sh, from this package's directory.
var entrypointPath = filepath.Join("..", "..", "docker", "entrypoint.sh")

// entrypointHeredoc returns the text docker/entrypoint.sh writes from its
// heredoc delimited by delim (<<'delim' ... delim), as cat writes it.
func entrypointHeredoc(t *testing.T, delim string) string {
	t.Helper()
	b, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	open := "<<'" + delim + "'\n"
	start := strings.Index(s, open)
	if start < 0 {
		t.Fatalf("docker/entrypoint.sh has no %s heredoc", open[:len(open)-1])
	}
	s = s[start+len(open):]
	end := strings.Index(s, "\n"+delim+"\n")
	if end < 0 {
		t.Fatalf("docker/entrypoint.sh's %s heredoc has no closing %s line", delim, delim)
	}
	return s[:end+1]
}

// writeDockerSeed writes the seeded config.toml into dir and returns its path.
func writeDockerSeed(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(entrypointHeredoc(t, "CONFIG")), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// assertDockerSeedValues checks the values docs/spec/operations.md says the
// seed sets, and that it is a loaded config (so the first-run wizard, which
// cannot complete through Docker's bridge, is skipped).
func assertDockerSeedValues(t *testing.T, cfg *MoomboxConfig) {
	t.Helper()
	if !cfg.ConfigLoaded {
		t.Error("the seed does not load as a config file: the first-run wizard would run, and it cannot complete through a published port")
	}
	if cfg.Network.NetworkAccess != "lan" || cfg.Network.Port != 774 {
		t.Errorf("network = %q:%d, want \"lan\":774 — anything else is unreachable through the published port and its healthcheck", cfg.Network.NetworkAccess, cfg.Network.Port)
	}
	for _, c := range []struct{ name, got, want string }{
		{"database_path", cfg.Paths.DatabasePath, "/data/moombox.db"},
		{"log_file_path", cfg.Paths.LogFilePath, "/data/moombox.log"},
		{"output_directory", cfg.Paths.OutputDirectory, "/data/output"},
		{"staging_directory", cfg.Paths.StagingDirectory, "/data/staging"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q — everything must live on the /data volume", c.name, c.got, c.want)
		}
	}
	if cfg.Cookies.CookieFile != "/data/cookies.txt" {
		t.Errorf("cookie_file = %q, want /data/cookies.txt", cfg.Cookies.CookieFile)
	}
	if cfg.Cookies.AutoEnabled {
		t.Error("auto_enabled is on; the image has no browser for it to run")
	}
	if cfg.Cookies.BrowserProfileDir != Defaults().Cookies.BrowserProfileDir {
		t.Errorf("browser_profile_dir = %q, want the default (it resolves to /data/browser-profile)", cfg.Cookies.BrowserProfileDir)
	}
	if cfg.Updates.AutoCheckUpdates {
		t.Error("auto_check_updates is on; an in-container update is reverted when the container is recreated")
	}
}

// The seed loads as documented, with nothing normalised or ignored, and with
// every section the binary auto-persists already present, so a first boot
// has no new section to write (the version stamp still rewrites the file —
// TestDockerGuidanceSurvivesTheFirstBoot).
//
// Mutants: `network_acess = "lan"` (network = "localhost":774);
// `auto_check_update = false` (auto_check_updates on); dropping the seed's
// `[connectivity]` line (NeedsAutoPersist) or any one of the paths
// (paths); `port = 77` (valid, so kept — network ...:77);
// `network_access = "LAN"` (NormalizedOnLoad); adding `auto_enabled = true`
// to [cookies].
func TestDockerSeedLoadsAsDocumented(t *testing.T) {
	cfg, err := Load(writeDockerSeed(t, t.TempDir()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertDockerSeedValues(t, cfg)
	if len(cfg.NormalizedOnLoad) != 0 || len(cfg.IgnoredOnLoad) != 0 {
		t.Errorf("the seed is normalised or ignored on load: normalised %v, ignored %v", cfg.NormalizedOnLoad, cfg.IgnoredOnLoad)
	}
	if cfg.NeedsAutoPersist {
		t.Error("the seed lacks a section the binary auto-persists: its first boot rewrites config.toml to add it")
	}
	// The sections seeded empty keep the built-in defaults.
	d := Defaults()
	if !reflect.DeepEqual(cfg.Memory, d.Memory) || !reflect.DeepEqual(cfg.Bgutils, d.Bgutils) || !reflect.DeepEqual(cfg.Connectivity, d.Connectivity) {
		t.Errorf("memory/bgutils/connectivity = %+v / %+v / %+v, want the defaults", cfg.Memory, cfg.Bgutils, cfg.Connectivity)
	}
}

// config.toml cannot carry the seed's guidance: every save writes out the
// settings and drops its comments, and the first boot saves it (the
// LastRunVersion stamp). So the guidance is README-docker.txt, which no save
// touches, and the seed's first comment points to it. This boots the seed as
// run() does — the auto-persist when one is needed, then the version stamp —
// and checks README-docker.txt still says, word for word, what the docs
// promise it says, and that every key the seed sets is explained in it.
//
// Mutants: deleting a guidance sentence, e.g. ": change all three
// together" (its needle); a seed key the guidance does not name, e.g. adding
// `https_enabled = false` to the seed's [network]; the seed's opening comment
// not naming README-docker.txt.
func TestDockerGuidanceSurvivesTheFirstBoot(t *testing.T) {
	dir := t.TempDir()
	p := writeDockerSeed(t, dir)
	guidance := entrypointHeredoc(t, "GUIDE")
	guidePath := filepath.Join(dir, "README-docker.txt")
	if err := os.WriteFile(guidePath, []byte(guidance), 0o644); err != nil {
		t.Fatal(err)
	}

	seed := entrypointHeredoc(t, "CONFIG")
	if first, _, _ := strings.Cut(seed, "\n# https://"); !strings.Contains(first, "README-docker.txt") {
		t.Errorf("the seed's opening comment does not point to README-docker.txt:\n%s", first)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	store := NewStore(cfg, p)
	if cfg.NeedsAutoPersist && cfg.ConfigLoaded { // services.go
		if err := store.SaveLocked(); err != nil {
			t.Fatalf("auto-persist: %v", err)
		}
	}
	if err := store.Update(func(c *MoomboxConfig) { c.Updates.LastRunVersion = "9.9.9" }); err != nil { // main.go
		t.Fatalf("version stamp: %v", err)
	}

	after, err := os.ReadFile(guidePath)
	if err != nil {
		t.Fatalf("README-docker.txt after the first boot: %v", err)
	}
	if string(after) != guidance {
		t.Error("the first boot's saves changed README-docker.txt")
	}
	for _, needle := range []string{
		"config.toml itself keeps no comments",
		"change all three together",             // port <-> port publish <-> healthcheck
		"Do NOT bind-mount the file on its own", // cookies.txt
		"auto_enabled is left OFF here on purpose",
		"Update by pulling a new image instead",
		"docker compose pull && docker compose up -d",
		"README-docker.txt.new", // an edited copy is kept
	} {
		if !strings.Contains(string(after), needle) {
			t.Errorf("README-docker.txt no longer says %q", needle)
		}
	}
	var raw map[string]map[string]any
	if _, err := toml.Decode(seed, &raw); err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	for section, keys := range raw {
		if !strings.Contains(string(after), "["+section+"]") {
			t.Errorf("README-docker.txt has no part for the seeded [%s]", section)
		}
		for key := range keys {
			if !strings.Contains(string(after), key) {
				t.Errorf("README-docker.txt does not explain the seeded %s.%s", section, key)
			}
		}
	}

	// And the stamp's rewrite kept every seeded value.
	reloaded, err := Load(p)
	if err != nil {
		t.Fatalf("Load after the first boot: %v", err)
	}
	assertDockerSeedValues(t, reloaded)
}

// sha256Hex is what the entrypoint's sha256_of prints.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
