package main

import (
	"context"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestCLIAddHonoursConfigOnEitherSide runs main() in a child process (the
// test binary re-executed). main used to recognise `add` only as argv[1], so
// `moombox -config X add URL` fell through to the daemon with its arguments
// silently dropped, and `add` loaded the config from the cwd search whatever
// -config said. Both orders now reach addVideo with the named config, which
// here does not exist, so it must exit 1 naming that path.
//
// Mutant: check os.Args[1] == "add" before parsing flags again — the first
// row boots the daemon instead (and fails on the timeout).
func TestCLIAddHonoursConfigOnEitherSide(t *testing.T) {
	if os.Getenv("MOOMBOX_CLI_HELPER") == "1" {
		os.Args = append([]string{"moombox"}, strings.Fields(os.Getenv("MOOMBOX_CLI_ARGS"))...)
		main()
		os.Exit(0)
	}
	for _, args := range []string{
		"-config /nonexistent/moombox-cli-test.toml add dQw4w9WgXcQ",
		"add -config /nonexistent/moombox-cli-test.toml dQw4w9WgXcQ",
	} {
		t.Run(args, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIAddHonoursConfigOnEitherSide$")
			cmd.Dir = t.TempDir() // a wrong dispatch writes a daemon's files here, not into the package
			cmd.Env = append(os.Environ(),
				"MOOMBOX_CLI_HELPER=1",
				"MOOMBOX_CLI_ARGS="+args,
				"_MOOMBOX_CHILD=1", // a wrong dispatch boots run() here, not a launcher
				"MOOMBOX_NO_TUI=1",
			)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("the command did not exit (it booted the daemon?):\n%s", out)
			}
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("exit = %v, want status 1:\n%s", err, out)
			}
			if !strings.Contains(string(out), "No config file at /nonexistent/moombox-cli-test.toml") {
				t.Errorf("add did not use the named config:\n%s", out)
			}
		})
	}
}

// TestCLIVersionEqualsTrueSkipsTheLauncher: only the literal -version and
// --version were answered before the launcher gate, so `-version=true` went
// through the launcher — taking the single-instance lock (failing while the
// daemon runs) and spawning a child just to print a line. Run WITHOUT
// _MOOMBOX_CHILD, the way a user would: a launcher dispatch here would spawn
// this test binary again rather than print the version.
//
// Mutant: move the *showVersion check back below the launcher gate.
func TestCLIVersionEqualsTrueSkipsTheLauncher(t *testing.T) {
	if os.Getenv("MOOMBOX_CLI_HELPER") == "1" {
		os.Args = append([]string{"moombox"}, strings.Fields(os.Getenv("MOOMBOX_CLI_ARGS"))...)
		main()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIVersionEqualsTrueSkipsTheLauncher$")
	cmd.Dir = t.TempDir()
	env := []string{"MOOMBOX_CLI_HELPER=1", "MOOMBOX_CLI_ARGS=-version=true", "MOOMBOX_NO_TUI=1"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "_MOOMBOX_CHILD=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the command did not exit:\n%s", out)
	}
	if err != nil || !strings.HasPrefix(string(out), "moombox "+version) {
		t.Fatalf("exit = %v, output = %q; want the version line and status 0", err, out)
	}
}

// TestUsageNamesTheAddSubcommand: -h printed the daemon's flags only, and
// `add` appeared nowhere until it was run without an argument. It names the
// `--` that ends add's flags too.
func TestUsageNamesTheAddSubcommand(t *testing.T) {
	var b strings.Builder
	prev := flagOutput(&b)
	defer flagOutput(prev)
	printUsage()
	if !strings.Contains(b.String(), "add [--] <video_id_or_url>") {
		t.Errorf("usage does not mention add and its --:\n%s", b.String())
	}
}

// TestCLIAddTakesADashLeadingVideoID runs main() in a child process against a
// config whose database exists. YouTube video IDs are drawn from
// [A-Za-z0-9_-], so about one in 64 begins with '-', and `moombox add
// -XyZ123abcd` was read as an undefined flag: the daemon's usage and status 2,
// although the same ID was accepted after `--` or inside its watch URL. Each
// order of -config and add now queues it; `--` keeps working; and a target
// with a second positional before it is still refused with add's usage.
//
// Mutants: addTarget returns its arguments unchanged (the first two rows exit
// 2); runAddCommand drops `|| fs.NArg() != 0` (the last row queues
// -AbCdEfGhIj and exits 0).
func TestCLIAddTakesADashLeadingVideoID(t *testing.T) {
	if os.Getenv("MOOMBOX_CLI_HELPER") == "1" {
		os.Args = append([]string{"moombox"}, strings.Split(os.Getenv("MOOMBOX_CLI_ARGS"), "\x1f")...)
		main()
		os.Exit(0)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "moombox.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[paths]\ndatabase_path = '"+dbPath+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		id   string // queued, and so in the database afterwards
		code int
		out  string
	}{
		{[]string{"-config", cfgPath, "add", "-XyZ123abcd"}, "-XyZ123abcd", 0, "Added -XyZ123abcd to queue."},
		{[]string{"add", "-config", cfgPath, "-QrS456tuvw"}, "-QrS456tuvw", 0, "Added -QrS456tuvw to queue."},
		{[]string{"-config", cfgPath, "add", "--", "-MnO789pqrs"}, "-MnO789pqrs", 0, "Added -MnO789pqrs to queue."},
		{[]string{"-config", cfgPath, "add", "extra", "-AbCdEfGhIj"}, "", 1, "Usage: moombox [-config path] add [--] <video_id_or_url>"},
	} {
		t.Run(strings.Join(tc.args[len(tc.args)-2:], " "), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIAddTakesADashLeadingVideoID$")
			cmd.Dir = dir
			home := t.TempDir() // no daemon lock to find: add takes the no-daemon rule
			cmd.Env = append(os.Environ(),
				"MOOMBOX_CLI_HELPER=1",
				"MOOMBOX_CLI_ARGS="+strings.Join(tc.args, "\x1f"),
				"_MOOMBOX_CHILD=1", // a wrong dispatch boots run() here, not a launcher
				"MOOMBOX_NO_TUI=1",
				"HOME="+home,
				"LOCALAPPDATA="+home,
			)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("the command did not exit:\n%s", out)
			}
			code := 0
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.code || !strings.Contains(string(out), tc.out) {
				t.Fatalf("exit %d, want %d with %q:\n%s", code, tc.code, tc.out, out)
			}
		})
	}
	db, err = database.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"-XyZ123abcd", "-QrS456tuvw", "-MnO789pqrs"} {
		if !db.JobExists(id) {
			t.Errorf("%s is not in the database", id)
		}
	}
	if db.JobExists("-AbCdEfGhIj") {
		t.Error("the refused row's ID was queued")
	}
}

// TestAddTargetTakesOnlyATrailingMediaID pins what addTarget takes from the
// flag parser: a trailing argument that begins with '-' and is a video ID, and
// nothing else — not a misspelt flag, which the parser must still report,
// and not the value of a -config before it.
//
// Mutants: drop the ExtractMediaID test (the -nosuch row is taken as a
// target); drop the -config value guard (the two -config rows lose their
// value).
func TestAddTargetTakesOnlyATrailingMediaID(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		flags  []string
		target string
	}{
		{[]string{"-XyZ123abcd"}, []string{}, "-XyZ123abcd"},
		{[]string{"-config", "c.toml", "-XyZ123abcd"}, []string{"-config", "c.toml"}, "-XyZ123abcd"},
		{[]string{"-config=c.toml", "-XyZ123abcd"}, []string{"-config=c.toml"}, "-XyZ123abcd"},
		{[]string{"--", "-XyZ123abcd"}, []string{"--"}, "-XyZ123abcd"},
		{[]string{"dQw4w9WgXcQ"}, []string{"dQw4w9WgXcQ"}, ""},
		{[]string{"-nosuch"}, []string{"-nosuch"}, ""},
		{[]string{"-config", "-XyZ123abcd"}, []string{"-config", "-XyZ123abcd"}, ""},
		{[]string{"--config", "-XyZ123abcd"}, []string{"--config", "-XyZ123abcd"}, ""},
		{nil, nil, ""},
	} {
		flags, target := addTarget(tc.args)
		if target != tc.target || strings.Join(flags, "\x1f") != strings.Join(tc.flags, "\x1f") {
			t.Errorf("addTarget(%q) = %q, %q; want %q, %q", tc.args, flags, target, tc.flags, tc.target)
		}
	}
}

// flagOutput swaps flag.CommandLine's output and returns the previous one.
func flagOutput(w io.Writer) io.Writer {
	prev := flag.CommandLine.Output()
	flag.CommandLine.SetOutput(w)
	return prev
}
