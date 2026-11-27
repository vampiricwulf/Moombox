package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Made-up session values; none is a real cookie.
const (
	fakeSID     = "g.a000FAKEsidVALUEforTHEredactorTEST0123456789"
	fakeSAPISID = "FakeSapisid/AbCdEfGh-123"
	fakeTwitch  = "faketwitchauthtoken0987654321"
	fakeRenewed = "RENEWEDvalueTHEtestWROTElater42"
)

func fakeCookieFile(t *testing.T, dir string) string {
	t.Helper()
	body := "# Netscape HTTP Cookie File\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1999999999\tSID\t" + fakeSID + "\n" +
		"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t1999999999\tSAPISID\t" + fakeSAPISID + "\n" +
		".youtube.com\tTRUE\t/\tFALSE\t1999999999\tPREF\tf6=40000000\n" +
		".youtube.com\tTRUE\t/\tFALSE\t1999999999\tCONSENT\ttrue\n" +
		".google.com\tTRUE\t/\tFALSE\t1999999999\tHL\ten\n" +
		".twitch.tv\tTRUE\t/\tTRUE\t1999999999\tauth-token\t" + fakeTwitch + "\n"
	p := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRedactMasksCookieValuesAndSessionShapes(t *testing.T) {
	dir := t.TempDir()
	cookiesPath := fakeCookieFile(t, dir)
	in := strings.Join([]string{
		"=== RUN   TestX",
		"    x_test.go:10: sid=" + fakeSID + " in a sentence",
		"    x_test.go:11: encoded " + strings.ReplaceAll(fakeSAPISID, "/", "%2F"),
		"    x_test.go:12: Authorization: SAPISIDHASH 1700000000_0123456789abcdef0123456789abcdef01234567",
		"    x_test.go:13: GET /videoplayback?itag=137&pot=MlNotACookieButAToken&visitorData=CgtWaXNpdG9y&mime=video",
		"    x_test.go:14: Set-Cookie: __Secure-NEW=newvalue; Path=/",
		"    x_test.go:15: twitch OAuth " + fakeTwitch,
		"    x_test.go:16: CONSENT is true and the language en; status signed in",
		"--- PASS: TestX (0.01s)",
		"",
	}, "\n")
	var out bytes.Buffer
	if err := runRedact([]string{"--cookies", cookiesPath}, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, secret := range []string{fakeSID, fakeSAPISID, strings.ReplaceAll(fakeSAPISID, "/", "%2F"), fakeTwitch,
		"0123456789abcdef0123456789abcdef01234567", "MlNotACookieButAToken", "CgtWaXNpdG9y", "newvalue"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted output still holds %q:\n%s", secret, got)
		}
	}
	// Short values are flags, not secrets: the words around them survive.
	for _, keep := range []string{"CONSENT is true and the language en; status signed in", "--- PASS: TestX (0.01s)", "itag=137", "&mime=video", "in a sentence"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redacted output lost %q:\n%s", keep, got)
		}
	}
}

func TestRedactRereadsARewrittenCopy(t *testing.T) {
	dir := t.TempDir()
	cookiesPath := fakeCookieFile(t, dir)
	set, err := newSecretSet([]string{cookiesPath})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(cookiesPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(".youtube.com\tTRUE\t/\tTRUE\t1999999999\tSIDCC\t" + fakeRenewed + "\n")
	f.Close()
	set.refresh()
	if got := set.redactLine("renewed " + fakeRenewed + " and old " + fakeSID); strings.Contains(got, fakeRenewed) || strings.Contains(got, fakeSID) {
		t.Errorf("a value written after the start, or one from before, survived: %q", got)
	}
}

func TestRedactFailsClosedWithoutItsCookieFile(t *testing.T) {
	var out bytes.Buffer
	err := runRedact([]string{"--cookies", filepath.Join(t.TempDir(), "missing.txt")}, strings.NewReader("secret line\n"), &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("redact with an unreadable cookie file: err=%v, passed through %q; want an error and nothing", err, out.String())
	}
}

// bashPath finds a POSIX bash: Git for Windows' on Windows (never WSL's
// System32 bash, which cannot see these paths), PATH's elsewhere.
func bashPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		p, err := exec.LookPath("bash")
		if err != nil {
			t.Skip("no bash on PATH")
		}
		return p
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH, so no Git Bash")
	}
	for _, rel := range []string{`..\bin\bash.exe`, `..\..\bin\bash.exe`, `..\usr\bin\bash.exe`} {
		p := filepath.Join(filepath.Dir(git), rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("Git Bash not found beside git")
	return ""
}

const fakeGo = `#!/usr/bin/env bash
# Stands in for "go test": records where it ran and what it was given, prints
# the cookie copy it got (a test that leaks), rewrites it with a renewed value
# and prints that, and reads its stdin (which must be empty). FAKE_MODE=sleep
# sleeps instead, writing FAKE_RECORD.late if it is ever allowed to finish.
pkg=${!#}
p=${MOOMBOX_LIVE_YT_COOKIES:-${MOOMBOX_LIVE_TWITCH_COOKIES:-${MOOMBOX_POT_ACCEPT_COOKIES:-}}}
stdin=$(head -c 200 || true)
echo "call pkg=${pkg#./} cwd=$(basename "$PWD") yt=${MOOMBOX_LIVE_YT_COOKIES:--} twitch=${MOOMBOX_LIVE_TWITCH_COOKIES:--} pot=${MOOMBOX_POT_ACCEPT_COOKIES:--} video=${MOOMBOX_POT_ACCEPT_VIDEO:--} private=${MOOMBOX_LIVE_COOKIES_PRIVATE:--} godebug=${GODEBUG:--} tmp=${TMP:--} channel=${MOOMBOX_LIVE_TWITCH_CHANNEL:--} stdin=${#stdin}" >>"$FAKE_RECORD"
echo "=== RUN   TestFake"
if [ "$FAKE_MODE" = sleep ]; then
	echo "    fake_test.go:1: sleeping"
	sleep 60
	echo late >"$FAKE_RECORD.late"
fi
if [ -n "$p" ]; then
	while IFS= read -r l; do echo "    fake_test.go:1: $l"; done <"$p"
	printf '.youtube.com\tTRUE\t/\tTRUE\t1999999999\tSIDCC\t%s\n' "$FAKE_RENEWED" >>"$p"
	echo "    fake_test.go:2: renewed $FAKE_RENEWED"
	echo "    fake_test.go:3: \"set-cookie\" = \"__Secure-3PSIDCC=QuotedRenewalNotInTheFile77; Path=/\""
fi
if [ "$FAKE_MODE" = fail ]; then
	echo "--- FAIL: TestFake (0.00s)"; echo FAIL; exit 1
fi
echo "--- PASS: TestFake (0.00s)"; echo PASS
`

// fakeCall is one run of the stand-in go, as it recorded it.
type fakeCall map[string]string

func readCalls(record string) []fakeCall {
	data, _ := os.ReadFile(record)
	var calls []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasPrefix(line, "call ") {
			continue
		}
		c := fakeCall{}
		for _, kv := range strings.Fields(line)[1:] {
			k, v, _ := strings.Cut(kv, "=")
			c[k] = v
		}
		calls = append(calls, c)
	}
	return calls
}

// runnerEnv is one made-up setup for the real script: a cookie source, a
// state directory, a settings file and the stand-in go.
type runnerEnv struct {
	dir, script, source, state, record, other string
	env                                       []string
}

func newRunnerEnv(t *testing.T, mode string) *runnerEnv {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "live-tests.sh"))
	if err != nil {
		t.Fatal(err)
	}
	r := &runnerEnv{dir: t.TempDir(), script: filepath.ToSlash(script)}
	srcDir := filepath.Join(r.dir, "src")
	os.Mkdir(srcDir, 0o700)
	r.source = fakeCookieFile(t, srcDir)
	r.other = filepath.Join(r.dir, "othertree")
	os.Mkdir(r.other, 0o700)
	r.state = filepath.Join(r.dir, "state")
	settings := filepath.Join(r.dir, "live-tests.env")
	os.WriteFile(settings, []byte("# test\nMOOMBOX_LIVE_COOKIE_SOURCE="+filepath.ToSlash(r.source)+"\nUNKNOWN_KEY=x\n"), 0o600)
	fake := filepath.Join(r.dir, "fakego")
	os.WriteFile(fake, []byte(fakeGo), 0o755)
	r.record = filepath.Join(r.dir, "record.txt")
	r.env = append(os.Environ(),
		"MOOMBOX_LIVE_STATE_DIR="+filepath.ToSlash(r.state),
		"MOOMBOX_LIVE_SETTINGS="+filepath.ToSlash(settings),
		"MOOMBOX_LIVE_GO_TEST="+filepath.ToSlash(fake),
		"MOOMBOX_LIVE_TWITCH_PICK=somechannel",
		"FAKE_RECORD="+filepath.ToSlash(r.record),
		"FAKE_RENEWED="+fakeRenewed,
		"FAKE_MODE="+mode,
		"GODEBUG=http2debug=2",
	)
	return r
}

func (r *runnerEnv) leftovers() []string {
	left, _ := filepath.Glob(filepath.Join(r.state, "live-tmp", "*"))
	return left
}

// TestRunnerKeepsTheSourceAndRemovesItsCopies drives the real script with a
// stand-in go command against a made-up cookie file: the source is never
// written, a test is only ever handed a copy, and only the package that reads
// that platform's cookies gets one, every copy is gone when the run ends (a
// failing test included), and neither the console nor the saved log holds a
// cookie value. It also runs the paths the owner's run does not: a
// two-package group, no cookies, and pot-accept in another tree, with and
// without a video.
func TestRunnerKeepsTheSourceAndRemovesItsCopies(t *testing.T) {
	bash := bashPath(t)
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ownTree := filepath.Base(abs)
	cookieVar := map[string]string{"internal/youtube": "yt", "internal/twitch": "twitch", "internal/bgutils": "pot"}
	cases := []struct {
		name     string
		args     []string // "{tree}" is replaced by the other tree's path
		mode     string
		wantFail bool
		calls    int
		cookies  bool
		cwd      string
		want     []string
		channel  string // the live channel the internal/twitch run must get
		onlyPkg  string // when set, the one package that may run
	}{
		{name: "pass", args: []string{"youtube-auth"}, mode: "pass", calls: 1, cookies: true, cwd: ownTree,
			want: []string{"PASS  internal/youtube", "RESULT: PASS"}},
		{name: "fail", args: []string{"youtube-auth"}, mode: "fail", wantFail: true, calls: 1, cookies: true, cwd: ownTree,
			want: []string{"FAIL  internal/youtube", "RESULT: FAIL"}},
		{name: "two packages", args: []string{"cookies"}, mode: "pass", calls: 2, cookies: true, cwd: ownTree, channel: "somechannel",
			want: []string{"PASS  internal/youtube", "PASS  internal/twitch"}},
		{name: "no cookies", args: []string{"--no-cookies", "youtube-auth"}, mode: "pass", calls: 1, cwd: ownTree,
			want: []string{"Cookie source: none (cookie tests skip)"}},
		{name: "pot-accept in another tree", args: []string{"--worktree", "{tree}", "--video", "aqz-KE-bpKQ", "pot-accept"},
			mode: "pass", calls: 1, cookies: true, cwd: "othertree", want: []string{"PASS  internal/bgutils"}},
		{name: "pot-accept without a video", args: []string{"pot-accept"}, mode: "pass", calls: 0,
			want: []string{"SKIP", "no video"}},
		{name: "cipher without its fixture", args: []string{"--worktree", "{tree}", "cipher"}, mode: "pass", calls: 1, cookies: true,
			cwd: "othertree", onlyPkg: "internal/youtube",
			want: []string{"SKIP", "no player fixture"}},
		{name: "twitch picks a live channel", args: []string{"twitch"}, mode: "pass", calls: 1, cookies: true, cwd: ownTree, channel: "somechannel",
			want: []string{"Twitch channel: somechannel"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunnerEnv(t, tc.mode)
			past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
			os.Chtimes(r.source, past, past)
			before, _ := os.ReadFile(r.source)
			beforeSum := sha256.Sum256(before)

			args := []string{r.script, "--no-probe"}
			for _, a := range tc.args {
				args = append(args, strings.ReplaceAll(a, "{tree}", filepath.ToSlash(r.other)))
			}
			cmd := exec.Command(bash, args...)
			cmd.Env = r.env
			out, err := cmd.CombinedOutput()
			console := string(out)
			if tc.wantFail != (err != nil) {
				t.Errorf("run error = %v, want failure %v:\n%s", err, tc.wantFail, console)
			}
			for _, w := range tc.want {
				if !strings.Contains(console, w) {
					t.Errorf("console lacks %q:\n%s", w, console)
				}
			}
			if strings.Contains(console, "cygpath") {
				t.Errorf("a tool complained on the console:\n%s", console)
			}

			after, _ := os.ReadFile(r.source)
			st, _ := os.Stat(r.source)
			if sha256.Sum256(after) != beforeSum || !st.ModTime().Equal(past) {
				t.Error("the cookie source was written")
			}

			calls := readCalls(r.record)
			if len(calls) != tc.calls {
				t.Fatalf("the stand-in go ran %d times, want %d:\n%s", len(calls), tc.calls, console)
			}
			for _, c := range calls {
				if c["stdin"] != "0" {
					t.Errorf("a test inherited %s bytes of stdin; it must get none", c["stdin"])
				}
				if c["cwd"] != tc.cwd {
					t.Errorf("a test ran in %q, want %q", c["cwd"], tc.cwd)
				}
				if c["godebug"] != "-" {
					t.Errorf("GODEBUG reached a test: %s", c["godebug"])
				}
				if c["private"] != "1" {
					t.Error("a test was not told its cookie file is already a private copy")
				}
				if !strings.Contains(strings.ToLower(c["tmp"]), "live-tmp") {
					t.Errorf("a test's TMP is outside the run directory: %s", c["tmp"])
				}
				if tc.onlyPkg != "" && c["pkg"] != tc.onlyPkg {
					t.Errorf("package %s ran; only %s should", c["pkg"], tc.onlyPkg)
				}
				if tc.channel != "" && c["pkg"] == "internal/twitch" && c["channel"] != tc.channel {
					t.Errorf("the Twitch channel tests got channel %q, want %q", c["channel"], tc.channel)
				}
				want := ""
				if tc.cookies {
					want = cookieVar[c["pkg"]]
				}
				for _, k := range []string{"yt", "twitch", "pot"} {
					got := c[k]
					if k != want {
						if got != "-" {
							t.Errorf("%s (%s) carried a cookie path it should not: %s", k, c["pkg"], got)
						}
						continue
					}
					if got == "-" || got == filepath.ToSlash(r.source) {
						t.Errorf("%s = %q, want a private copy", k, got)
					}
					if _, err := os.Stat(got); !os.IsNotExist(err) {
						t.Errorf("the copy %s survived the run", got)
					}
				}
				if tc.name == "pot-accept in another tree" && c["video"] != "aqz-KE-bpKQ" {
					t.Errorf("pot-accept video = %q", c["video"])
				}
			}
			if left := r.leftovers(); len(left) != 0 {
				t.Errorf("run directories left behind: %v", left)
			}
			logs, _ := filepath.Glob(filepath.Join(r.state, "live-logs", "*.log"))
			if len(logs) != 1 {
				t.Fatalf("want one saved log, got %v", logs)
			}
			logText, _ := os.ReadFile(logs[0])
			for _, secret := range []string{fakeSID, fakeSAPISID, fakeTwitch, fakeRenewed, "QuotedRenewalNotInTheFile77"} {
				if strings.Contains(console, secret) || strings.Contains(string(logText), secret) {
					t.Errorf("a cookie value reached the console or the log")
				}
			}
			if tc.cookies && tc.calls > 0 {
				if !strings.Contains(string(logText), mask) {
					t.Error("the log holds no mask: the stand-in's leak never went through the redactor")
				}
				if !strings.Contains(console, "Self-check: 0 secrets") {
					t.Errorf("no clean self-check line:\n%s", console)
				}
			}
		})
	}
}

// TestRunnerStopsAtOnceOnTerm sends TERM to a run whose test sleeps: the run
// ends at once rather than when the test would, the test's processes are
// ended, and nothing of the run is left. Ctrl+C (INT) takes the same trap,
// which was checked by hand from a bash parent against a native process
// tree (cmd.exe running ping): exit 130, the tree gone, nothing left. Sent
// from this Go-started wrapper, INT did not reach the runner's trap, so TERM
// stands in for it here.
func TestRunnerStopsAtOnceOnTerm(t *testing.T) {
	bash := bashPath(t)
	r := newRunnerEnv(t, "sleep")
	wrapper := `"$@" & p=$!
for _ in $(seq 1 100); do [ -s "$FAKE_RECORD" ] && break; sleep 0.2; done
sleep 1
kill -TERM $p
wait $p`
	start := time.Now()
	cmd := exec.Command(bash, "-c", wrapper, "wrap", bash, r.script, "--no-probe", "youtube-auth")
	cmd.Env = r.env
	out, _ := cmd.CombinedOutput()
	if took := time.Since(start); took > 40*time.Second {
		t.Errorf("the interrupted run took %v; it waited for the test", took)
	}
	if len(readCalls(r.record)) != 1 {
		t.Fatalf("the stand-in never started:\n%s", out)
	}
	time.Sleep(65 * time.Second)
	if _, err := os.Stat(r.record + ".late"); err == nil {
		t.Error("the sleeping test outlived the interrupted run")
	}
	if left := r.leftovers(); len(left) != 0 {
		t.Errorf("left behind after an interrupt: %v\n%s", left, out)
	}
}

// TestRunnerSweepsAndChecksWhatAKilledRunLeft plants what a hard kill leaves
// (a run directory past its deadline, one whose runner is gone, and a log
// with a cookie value and no summary) and checks that --sweep and the next
// run clear them.
func TestRunnerSweepsAndChecksWhatAKilledRunLeft(t *testing.T) {
	bash := bashPath(t)
	r := newRunnerEnv(t, "pass")
	tmp := filepath.Join(r.state, "live-tmp")
	logs := filepath.Join(r.state, "live-logs")
	os.MkdirAll(tmp, 0o700)
	os.MkdirAll(logs, 0o700)
	plant := func(name, owner string) {
		d := filepath.Join(tmp, name)
		os.Mkdir(d, 0o700)
		os.WriteFile(filepath.Join(d, "owner"), []byte(owner), 0o600)
		os.WriteFile(filepath.Join(d, "work-1.txt"), []byte("copy"), 0o600)
	}
	// Past its deadline: stale whatever its pid names now.
	plant("run.pastdeadline", fmt.Sprintf("1 %d\n", time.Now().Add(-time.Hour).Unix()))
	plant("run.deadpid", fmt.Sprintf("999999 %d\n", time.Now().Add(time.Hour).Unix()))
	cmd := exec.Command(bash, r.script, "--sweep")
	cmd.Env = r.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("--sweep: %v\n%s", err, out)
	}
	if left := r.leftovers(); len(left) != 0 {
		t.Errorf("--sweep left %v", left)
	}

	stopped := filepath.Join(logs, "live-20000101-000000-abcd.log")
	os.WriteFile(stopped, []byte("=== RUN   TestX\n    x_test.go:1: leaked "+fakeSID+"\n"), 0o600)
	cmd = exec.Command(bash, r.script, "--no-probe", "youtube-auth")
	cmd.Env = r.env
	out, _ := cmd.CombinedOutput()
	data, err := os.ReadFile(stopped)
	if err == nil && strings.Contains(string(data), fakeSID) {
		t.Errorf("a stopped run's log still holds a cookie value:\n%s", out)
	}
	if err == nil && !strings.Contains(string(data), "==== summary") {
		t.Error("the stopped run's log was not marked as checked")
	}
	if !strings.Contains(string(out), "redacted again") {
		t.Errorf("the console does not say the stopped log was redacted:\n%s", out)
	}
	if strings.Contains(string(out), fakeSID) {
		t.Error("the check printed the value it found")
	}
}

// TestRunnerStateIsTheScriptsCheckout runs a copy of the script from the
// main checkout of a repository (where git names the common dir relatively)
// while standing at the root of another one, with no state hook: its state
// directory is the script's checkout's .superpowers, never the caller's.
// Only --sweep runs, which stops before the settings and any cookie source.
func TestRunnerStateIsTheScriptsCheckout(t *testing.T) {
	bash := bashPath(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	src, err := os.ReadFile(filepath.Join("..", "..", ".github", "scripts", "live-tests.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	repo, other := filepath.Join(dir, "repo"), filepath.Join(dir, "other")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MOOMBOX_LIVE_") && !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	for _, d := range []string{repo, other} {
		cmd := exec.Command(git, "init", "-q", d)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %v\n%s", d, err, out)
		}
	}
	scripts := filepath.Join(repo, ".github", "scripts")
	os.MkdirAll(scripts, 0o700)
	script := filepath.Join(scripts, "live-tests.sh")
	if err := os.WriteFile(script, src, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, filepath.ToSlash(script), "--sweep")
	cmd.Dir = other
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("--sweep: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".superpowers", "live-tmp")); err != nil {
		t.Errorf("the script's checkout has no state directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, ".superpowers")); err == nil {
		t.Error("the state directory landed in the caller's repository")
	}
}

// TestRunnerRefusesASecondRun: while a live run holds the lock, another
// refuses to start.
func TestRunnerRefusesASecondRun(t *testing.T) {
	bash := bashPath(t)
	r := newRunnerEnv(t, "pass")
	lock := filepath.Join(r.state, "live-tmp", "lock")
	os.MkdirAll(lock, 0o700)
	// The holder is the wrapper shell itself: a live pid of the same runtime.
	wrapper := `echo "$$ $2" >"$1/owner"; shift 2; "$@"`
	cmd := exec.Command(bash, "-c", wrapper, "wrap", filepath.ToSlash(lock), fmt.Sprint(time.Now().Add(time.Hour).Unix()),
		bash, r.script, "--no-probe", "youtube-auth")
	cmd.Env = r.env
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "another live run") {
		t.Errorf("a second run started: %v\n%s", err, out)
	}
	if len(readCalls(r.record)) != 0 {
		t.Error("the second run ran a test")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Error("the refused run removed the other run's lock")
	}
}

func TestSummaryReadsVerdictsSkipsAndPackageFailures(t *testing.T) {
	log := strings.Join([]string{
		"### live-tests begin group=probe pkg=internal/youtube",
		"=== RUN   TestLivePremiumVerdict",
		"    premium_live_test.go:60: LIVE-SESSION youtube=signed_in premium=no",
		"--- PASS: TestLivePremiumVerdict (1.20s)",
		"### live-tests end group=probe pkg=internal/youtube exit=0",
		"### live-tests begin group=probe pkg=internal/twitch",
		"=== RUN   TestLiveAuthenticatedTokenValidate",
		"    auth_live_test.go:49: set MOOMBOX_LIVE_TWITCH_COOKIES=<path> to run it",
		"--- SKIP: TestLiveAuthenticatedTokenValidate (0.00s)",
		"### live-tests end group=probe pkg=internal/twitch exit=0",
		"### live-tests begin group=cipher pkg=internal/cipher",
		"# github.com/x/internal/cipher [build failed]",
		"### live-tests end group=cipher pkg=internal/cipher exit=1",
		"### live-tests begin group=goja pkg=internal/bgutils",
		"ok  	x/internal/bgutils	0.1s [no tests to run]",
		"### live-tests end group=goja pkg=internal/bgutils exit=0",
	}, "\n")
	p := filepath.Join(t.TempDir(), "x.log")
	os.WriteFile(p, []byte(log), 0o600)
	var out bytes.Buffer
	if err := runSummary(p, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"YouTube session: signed in; Premium: no; Twitch session: no cookies",
		"SKIP  internal/twitch",
		"to run it",
		"FAIL  internal/cipher",
		"NONE  internal/bgutils",
		"RESULT: FAIL",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
}
