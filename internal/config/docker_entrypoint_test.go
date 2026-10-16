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
	return runImageEntrypoint(t, entrypointPath, dataDir, stubs, args...)
}

// otherImage stands in for another image's entrypoint: a copy of
// docker/entrypoint.sh whose README-docker.txt text has one line more,
// naming tag. It returns the copy's path and that text.
func otherImage(t *testing.T, tag string) (script, guidance string) {
	t.Helper()
	b, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatal(err)
	}
	const title = "<<'GUIDE'\nMoombox in Docker\n=================\n"
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if strings.Count(s, title) != 1 {
		t.Fatal("docker/entrypoint.sh's GUIDE heredoc no longer opens with its title")
	}
	s = strings.Replace(s, title, title+"(the "+tag+" image)\n", 1)
	script = filepath.Join(t.TempDir(), "entrypoint.sh")
	if err := os.WriteFile(script, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
	title0 := strings.TrimPrefix(title, "<<'GUIDE'\n")
	return script, strings.Replace(entrypointHeredoc(t, "GUIDE"), title0, title0+"(the "+tag+" image)\n", 1)
}

// runImageEntrypoint is runEntrypoint for the entrypoint at script.
func runImageEntrypoint(t *testing.T, script, dataDir string, stubs map[string]string, args ...string) entrypointRun {
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
	script, err := filepath.Abs(script)
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
// operator's edits: written when missing, refreshed while it still holds a
// text an entrypoint wrote — to it, or beside it as README-docker.txt.new —
// which .README-docker.txt.sha256 lists, one checksum per text, and an
// edited copy kept, with a text not given before beside it as
// README-docker.txt.new, announced once per text. A .new the operator moved
// over the copy is refreshed by the next image, whichever it is — the
// entrypoint runs only at container start, so that start is usually the
// next update — and a .new the operator deleted is not written back. A
// failure to write the guidance never stops the container from starting.
// The config seed is written only when config.toml is missing, and the
// binary is exec'd with -config and the container's arguments either way.
//
// Mutants: taking `known_guidance "$have"` as true (an edited copy is
// overwritten) or as false (an older image's unedited text is never
// refreshed, nor a .new moved over the copy); remembering nothing after the
// README-docker.txt write (an edit of this image's text gets a .new) or
// after the .new write (the notice repeats every start, a deleted .new comes
// back, and a .new moved over the copy counts as an edit); dropping the
// `known_guidance "$want"` skip (a deleted .new comes back); not
// remembering a file that already holds this image's text (a lost stamp
// makes the next image take it for an edit); remember_guidance appending
// without its known_guidance check (a checksum listed twice); dropping
// either `rm -f` of a .new, once the file is current or after a refresh (a
// copied-over .new outlives it); `rm -f` of the temp path without `|| true` (a failed write stops the
// container); dropping the not-a-file guard (the text is moved INTO a
// README-docker.txt directory); seeding when config.toml exists (`if true`
// for `[ ! -f ... ]` — an existing config is rewritten); exec without "$@"
// (`moombox add <url>` loses its arguments).
func TestDockerEntrypointKeepsTheGuidanceWithoutClobberingEdits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the entrypoint runs in the Linux image")
	}
	for _, tool := range []string{"sh", "sha256sum", "grep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on PATH (the Debian image has all three): %v", tool, err)
		}
	}
	guidance := entrypointHeredoc(t, "GUIDE")
	seed := entrypointHeredoc(t, "CONFIG")
	older, olderText := otherImage(t, "older")
	imageB, textB := otherImage(t, "B")
	imageC, textC := otherImage(t, "C")
	imageD, textD := otherImage(t, "D")
	imageE, textE := otherImage(t, "E")
	data := t.TempDir()
	cfgPath := filepath.Join(data, "config.toml")
	guide := filepath.Join(data, "README-docker.txt")
	stamp := filepath.Join(data, ".README-docker.txt.sha256")
	newCopy := guide + ".new"
	stampLists := func(texts ...string) {
		t.Helper()
		var want []string
		for _, text := range texts {
			want = append(want, sha256Hex(text))
		}
		if got := strings.Fields(readFileString(t, stamp)); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("the stamp lists %q, want the checksum of each text written, once, in order: %q", got, want)
		}
	}
	guideHolds := func(want, what string) {
		t.Helper()
		if got := readFileString(t, guide); got != want {
			t.Errorf("README-docker.txt does not hold %s:\n%s", what, got)
		}
	}
	quiet := func(run entrypointRun, what string) {
		t.Helper()
		if strings.Contains(run.out, "README-docker.txt") {
			t.Errorf("%s announced the guidance:\n%s", what, run.out)
		}
	}

	// First start, of an older image: the seed and its guidance, and the
	// exec.
	run := runImageEntrypoint(t, older, data, nil, "add", "https://example.invalid/v")
	if got := readFileString(t, cfgPath); got != seed {
		t.Errorf("first start seeded config.toml as\n%s\nwant the CONFIG heredoc", got)
	}
	guideHolds(olderText, "the first image's text")
	stampLists(olderText)
	if want := []string{"-config", cfgPath, "add", "https://example.invalid/v"}; strings.Join(run.execArg, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("moombox exec'd with %q, want %q", run.execArg, want)
	}

	// An existing config.toml is never touched; a current guide is left be,
	// and a lost stamp is rebuilt from it.
	writeFileString(t, cfgPath, "[network]\nnetwork_access = \"external\"\n")
	if err := os.Remove(stamp); err != nil {
		t.Fatal(err)
	}
	run = runImageEntrypoint(t, older, data, nil)
	if got := readFileString(t, cfgPath); got != "[network]\nnetwork_access = \"external\"\n" {
		t.Errorf("a later start rewrote config.toml:\n%s", got)
	}
	quiet(run, "a start with the guide current")
	if fileExists(newCopy) {
		t.Error("a start with the guide current wrote README-docker.txt.new")
	}
	stampLists(olderText)

	// An update: the older image's text, unedited, is refreshed.
	run = runEntrypoint(t, data, nil)
	guideHolds(guidance, "this image's text after an update")
	stampLists(olderText, guidance)
	if !strings.Contains(run.out, "refreshing") {
		t.Errorf("the refresh was not logged:\n%s", run.out)
	}

	// The operator edits it: kept, and as it was edited from this image's
	// own text, there is nothing new to put beside it.
	const edited = "Moombox in Docker\n\nmy own notes: port 8774 on this host\n"
	writeFileString(t, guide, edited)
	run = runEntrypoint(t, data, nil)
	guideHolds(edited, "the operator's edit")
	quiet(run, "a restart after an edit of this image's text")
	if fileExists(newCopy) {
		t.Error("an edit of this image's own text got it back as README-docker.txt.new")
	}

	// The next image keeps the edit and puts its text beside it, announced
	// once; a .new the operator deleted is not written back.
	run = runImageEntrypoint(t, imageB, data, nil)
	guideHolds(edited, "the operator's edit after an update")
	if !fileExists(newCopy) || readFileString(t, newCopy) != textB {
		t.Error("the image's text was not put beside the edited copy as README-docker.txt.new")
	}
	if !strings.Contains(run.out, "local edits") || !strings.Contains(run.out, newCopy) {
		t.Errorf("keeping the edited copy was not logged, naming the .new:\n%s", run.out)
	}
	stampLists(olderText, guidance, textB)
	quiet(runImageEntrypoint(t, imageB, data, nil), "a restart with nothing new to say")
	if err := os.Remove(newCopy); err != nil {
		t.Fatal(err)
	}
	quiet(runImageEntrypoint(t, imageB, data, nil), "a restart after the .new was deleted")
	if fileExists(newCopy) {
		t.Error("a README-docker.txt.new the operator deleted was written back")
	}
	guideHolds(edited, "the operator's edit after the .new was deleted")

	// The operator copies a .new over the file, leaving the .new: on the
	// next start it is current, and the .new goes.
	runImageEntrypoint(t, imageC, data, nil)
	if !fileExists(newCopy) || readFileString(t, newCopy) != textC {
		t.Fatal("a newer image's text was not put beside the edited copy")
	}
	writeFileString(t, guide, textC)
	quiet(runImageEntrypoint(t, imageC, data, nil), "a start with the guide current")
	if fileExists(newCopy) {
		t.Error("README-docker.txt.new outlived the guide becoming current")
	}

	// The operator edits it again, and the next image's .new is moved over
	// it while that image runs: the start after it is another image's, which
	// refreshes the file, as it is a text an entrypoint wrote.
	writeFileString(t, guide, edited)
	runImageEntrypoint(t, imageD, data, nil)
	if err := os.Rename(newCopy, guide); err != nil {
		t.Fatal(err)
	}
	guideHolds(textD, "the .new moved over it")
	run = runEntrypoint(t, data, nil)
	if strings.Contains(run.out, "local edits") {
		t.Errorf("a .new moved over the file was taken for the operator's edit:\n%s", run.out)
	}
	guideHolds(guidance, "this image's text after a .new was moved over it")
	stampLists(olderText, guidance, textB, textC, textD)

	// Copied over instead, leaving the .new, and refreshed by another image
	// the same way: the .new it holds is stale, and goes.
	writeFileString(t, guide, edited)
	runImageEntrypoint(t, imageE, data, nil)
	writeFileString(t, guide, textE)
	runEntrypoint(t, data, nil)
	guideHolds(guidance, "this image's text after a .new was copied over it")
	if fileExists(newCopy) {
		t.Error("a refresh left README-docker.txt.new behind")
	}
	stampLists(olderText, guidance, textB, textC, textD, textE)

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
