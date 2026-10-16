package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// entrypointRun is one start of the container's entrypoint against a data
// directory, with a stub in place of the moombox binary it execs.
type entrypointRun struct {
	out     string   // stdout and stderr
	execArg []string // what the stub moombox was started with; nil if never
}

// runEntrypoint runs docker/entrypoint.sh with MOOMBOX_CONFIG in dataDir, as
// the image does (/bin/sh, which is dash on Debian), and the given extra
// arguments. The moombox stub records its arguments, so a run that never
// reached its exec is visible; stubs adds more commands ahead of PATH.
func runEntrypoint(t *testing.T, dataDir string, stubs map[string]string, args ...string) entrypointRun {
	t.Helper()
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	all := map[string]string{"moombox": "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$STUB_ARGS\"\n"}
	for name, body := range stubs {
		all[name] = body
	}
	for name, body := range all {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := filepath.Abs(entrypointPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = dataDir
	cmd.Env = append(os.Environ(),
		"MOOMBOX_CONFIG="+filepath.Join(dataDir, "config.toml"),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_ARGS="+argsFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entrypoint failed: %v\n%s", err, out)
	}
	run := entrypointRun{out: string(out)}
	if b, err := os.ReadFile(argsFile); err == nil {
		run.execArg = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	}
	return run
}

func readFileString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFileString(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// The entrypoint keeps README-docker.txt current without ever overwriting an
// operator's edits: written when missing, refreshed while it is still the
// text an entrypoint wrote (the checksum in .README-docker.txt.sha256), and
// an edited copy kept, with this image's text beside it as
// README-docker.txt.new — announced once per text, not on every start. A
// failure to write it never stops the container from starting. The config
// seed is written only when config.toml is missing, and the binary is
// exec'd with -config and the container's arguments either way.
//
// Mutants: dropping the stamp comparison (`[ "$have" != "$wrote" ]` taken as
// false — an edited copy is overwritten); taking it as true (an old image's
// unedited text is never refreshed); not writing the stamp after a write
// (the next image's text counts as an edit); dropping the `.new`
// already-current check (the edit notice repeats every start); not
// re-stamping a file that already holds this image's text (a .new moved
// over it is treated as an edit by the next image); `rm -f` of the temp
// path without `|| true` (a failed write stops the container); dropping the
// not-a-file guard (the text is moved INTO a README-docker.txt directory);
// seeding when config.toml exists (`if true` for `[ ! -f ... ]` — an existing
// config is rewritten); exec without "$@" (`moombox add <url>` loses its
// arguments).
func TestDockerEntrypointKeepsTheGuidanceWithoutClobberingEdits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the entrypoint runs in the Linux image")
	}
	for _, tool := range []string{"sh", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on PATH (the Debian image has both): %v", tool, err)
		}
	}
	guidance := entrypointHeredoc(t, "GUIDE")
	seed := entrypointHeredoc(t, "CONFIG")
	data := t.TempDir()
	cfgPath := filepath.Join(data, "config.toml")
	guide := filepath.Join(data, "README-docker.txt")
	stamp := filepath.Join(data, ".README-docker.txt.sha256")
	newCopy := guide + ".new"
	stampHolds := func(want string) {
		t.Helper()
		if got := strings.TrimSpace(readFileString(t, stamp)); got != sha256Hex(want) {
			t.Errorf("the stamp holds %s, want the checksum of the text written (%s)", got, sha256Hex(want))
		}
	}

	// First start: the seed and the guidance, and the exec.
	run := runEntrypoint(t, data, nil, "add", "https://example.invalid/v")
	if got := readFileString(t, cfgPath); got != seed {
		t.Errorf("first start seeded config.toml as\n%s\nwant the CONFIG heredoc", got)
	}
	if got := readFileString(t, guide); got != guidance {
		t.Error("first start did not write README-docker.txt as the GUIDE heredoc")
	}
	stampHolds(guidance)
	if want := []string{"-config", cfgPath, "add", "https://example.invalid/v"}; strings.Join(run.execArg, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("moombox exec'd with %q, want %q", run.execArg, want)
	}

	// An existing config.toml is never touched; a current guide is left be.
	writeFileString(t, cfgPath, "[network]\nnetwork_access = \"external\"\n")
	run = runEntrypoint(t, data, nil)
	if got := readFileString(t, cfgPath); got != "[network]\nnetwork_access = \"external\"\n" {
		t.Errorf("a later start rewrote config.toml:\n%s", got)
	}
	if strings.Contains(run.out, "README-docker.txt") {
		t.Errorf("a start with the guide current announced it:\n%s", run.out)
	}
	if fileExists(newCopy) {
		t.Error("a start with the guide current wrote README-docker.txt.new")
	}

	// An older image's text, unedited: refreshed.
	const older = "Moombox in Docker (an older image's text)\n"
	writeFileString(t, guide, older)
	writeFileString(t, stamp, sha256Hex(older)+"\n")
	run = runEntrypoint(t, data, nil)
	if got := readFileString(t, guide); got != guidance {
		t.Errorf("an older image's unedited text was not refreshed:\n%s", got)
	}
	stampHolds(guidance)
	if !strings.Contains(run.out, "refreshing") {
		t.Errorf("the refresh was not logged:\n%s", run.out)
	}

	// The operator's edits: kept, and this image's text put beside them,
	// announced once.
	const edited = "Moombox in Docker\n\nmy own notes: port 8774 on this host\n"
	writeFileString(t, guide, edited)
	run = runEntrypoint(t, data, nil)
	if got := readFileString(t, guide); got != edited {
		t.Fatalf("an edited README-docker.txt was overwritten:\n%s", got)
	}
	if !fileExists(newCopy) || readFileString(t, newCopy) != guidance {
		t.Error("the image's text was not put beside the edited copy as README-docker.txt.new")
	}
	if !strings.Contains(run.out, "local edits") || !strings.Contains(run.out, newCopy) {
		t.Errorf("keeping the edited copy was not logged, naming the .new:\n%s", run.out)
	}
	stampHolds(guidance)
	run = runEntrypoint(t, data, nil)
	if strings.Contains(run.out, "local edits") {
		t.Errorf("the edit notice repeated on a start with nothing new to say:\n%s", run.out)
	}
	if got := readFileString(t, guide); got != edited {
		t.Errorf("the edited copy was overwritten on a later start:\n%s", got)
	}

	// The operator takes the .new: it is this image's text, the .new goes,
	// and the stamp records it so the next image may refresh it.
	if err := os.Rename(newCopy, guide); err != nil {
		t.Fatal(err)
	}
	writeFileString(t, stamp, sha256Hex(older)+"\n")
	runEntrypoint(t, data, nil)
	stampHolds(guidance)
	if fileExists(newCopy) {
		t.Error("README-docker.txt.new outlived the guide becoming current")
	}

	// A guide that cannot be written does not stop the start.
	if err := os.Remove(guide); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(guide+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	run = runEntrypoint(t, data, nil)
	if run.execArg == nil {
		t.Fatalf("a failed guidance write stopped the container before moombox started:\n%s", run.out)
	}
	if !strings.Contains(run.out, "could not write") {
		t.Errorf("the failed guidance write was not reported:\n%s", run.out)
	}

	// Nor does a README-docker.txt that is not a file, which is left be.
	if err := os.Remove(guide + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(guide, 0o755); err != nil {
		t.Fatal(err)
	}
	run = runEntrypoint(t, data, nil)
	if run.execArg == nil || !strings.Contains(run.out, "could not write") {
		t.Errorf("a README-docker.txt directory: exec'd %v, output:\n%s", run.execArg, run.out)
	}
	if entries, err := os.ReadDir(guide); err != nil || len(entries) != 0 {
		t.Errorf("the entrypoint wrote into a README-docker.txt directory: %v %v", entries, err)
	}
}

// With no sha256sum the entrypoint cannot tell an edited guide from its own
// text, so it writes none and says so, and the container still starts.
//
// Mutant: dropping the `[ -n "$want" ]` guard — a guide and an empty stamp
// are written, and every later start reads the guide as an operator's edit.
func TestDockerEntrypointWithoutAChecksumWritesNoGuidance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the entrypoint runs in the Linux image")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	data := t.TempDir()
	run := runEntrypoint(t, data, map[string]string{"sha256sum": "#!/bin/sh\nexit 127\n"})
	if run.execArg == nil {
		t.Fatalf("the container did not start:\n%s", run.out)
	}
	if !strings.Contains(run.out, "could not write") {
		t.Errorf("the skipped guidance was not reported:\n%s", run.out)
	}
	for _, name := range []string{"README-docker.txt", ".README-docker.txt.sha256"} {
		if fileExists(filepath.Join(data, name)) {
			t.Errorf("%s written with no checksum to keep it by", name)
		}
	}
	if !fileExists(filepath.Join(data, "config.toml")) {
		t.Error("the config seed depends on the guidance")
	}
}
