package main

import (
	"bytes"
	"crypto/sha256"
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
# and prints that, and reads its stdin (which must be empty).
p=${MOOMBOX_LIVE_YT_COOKIES:-${MOOMBOX_POT_ACCEPT_COOKIES:-}}
stdin=$(head -c 200 || true)
echo "call cwd=$(basename "$PWD") yt=${MOOMBOX_LIVE_YT_COOKIES:--} pot=${MOOMBOX_POT_ACCEPT_COOKIES:--} video=${MOOMBOX_POT_ACCEPT_VIDEO:--} stdin=${#stdin}" >>"$FAKE_RECORD"
echo "=== RUN   TestFake"
if [ -n "$p" ]; then
	while IFS= read -r l; do echo "    fake_test.go:1: $l"; done <"$p"
	printf '.youtube.com\tTRUE\t/\tTRUE\t1999999999\tSIDCC\t%s\n' "$FAKE_RENEWED" >>"$p"
	echo "    fake_test.go:2: renewed $FAKE_RENEWED"
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

// TestRunnerKeepsTheSourceAndRemovesItsCopies drives the real script with a
// stand-in go command against a made-up cookie file: the source is never
// written, a test is only ever handed a copy, every copy is gone when the run
// ends (a failing test included), and neither the console nor the saved log
// holds a cookie value. It also runs the paths the owner's run does not: a
// two-package group, no cookies, and pot-accept in another tree, with and
// without a video.
func TestRunnerKeepsTheSourceAndRemovesItsCopies(t *testing.T) {
	bash := bashPath(t)
	script, err := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "live-tests.sh"))
	if err != nil {
		t.Fatal(err)
	}
	ownTree := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(script))))
	cases := []struct {
		name     string
		args     []string // "{tree}" is replaced by the other tree's path
		mode     string
		wantFail bool
		calls    int
		cookieOn string // the variable that carries the copy; "" for none
		cwd      string
		want     []string
	}{
		{name: "pass", args: []string{"youtube-auth"}, mode: "pass", calls: 1, cookieOn: "yt", cwd: ownTree,
			want: []string{"PASS  internal/youtube", "RESULT: PASS"}},
		{name: "fail", args: []string{"youtube-auth"}, mode: "fail", wantFail: true, calls: 1, cookieOn: "yt", cwd: ownTree,
			want: []string{"FAIL  internal/youtube", "RESULT: FAIL"}},
		{name: "two packages", args: []string{"cookies"}, mode: "pass", calls: 2, cookieOn: "yt", cwd: ownTree,
			want: []string{"PASS  internal/youtube", "PASS  internal/twitch"}},
		{name: "no cookies", args: []string{"--no-cookies", "youtube-auth"}, mode: "pass", calls: 1, cwd: ownTree,
			want: []string{"Cookie source: none (cookie tests skip)"}},
		{name: "pot-accept in another tree", args: []string{"--worktree", "{tree}", "--video", "aqz-KE-bpKQ", "pot-accept"},
			mode: "pass", calls: 1, cookieOn: "pot", cwd: "othertree", want: []string{"PASS  internal/bgutils"}},
		{name: "pot-accept without a video", args: []string{"pot-accept"}, mode: "pass", calls: 0,
			want: []string{"SKIP", "no video"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srcDir := filepath.Join(dir, "src")
			os.Mkdir(srcDir, 0o700)
			source := fakeCookieFile(t, srcDir)
			past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
			os.Chtimes(source, past, past)
			before, _ := os.ReadFile(source)
			beforeSum := sha256.Sum256(before)

			other := filepath.Join(dir, "othertree")
			os.Mkdir(other, 0o700)
			state := filepath.Join(dir, "state")
			settings := filepath.Join(dir, "live-tests.env")
			os.WriteFile(settings, []byte("# test\nMOOMBOX_LIVE_COOKIE_SOURCE="+filepath.ToSlash(source)+"\nUNKNOWN_KEY=x\n"), 0o600)
			fake := filepath.Join(dir, "fakego")
			os.WriteFile(fake, []byte(fakeGo), 0o755)
			record := filepath.Join(dir, "record.txt")

			args := []string{filepath.ToSlash(script), "--no-probe"}
			for _, a := range tc.args {
				args = append(args, strings.ReplaceAll(a, "{tree}", filepath.ToSlash(other)))
			}
			cmd := exec.Command(bash, args...)
			cmd.Env = append(os.Environ(),
				"MOOMBOX_LIVE_STATE_DIR="+filepath.ToSlash(state),
				"MOOMBOX_LIVE_SETTINGS="+filepath.ToSlash(settings),
				"MOOMBOX_LIVE_GO_TEST="+filepath.ToSlash(fake),
				"FAKE_RECORD="+filepath.ToSlash(record),
				"FAKE_RENEWED="+fakeRenewed,
				"FAKE_MODE="+tc.mode,
			)
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

			after, _ := os.ReadFile(source)
			st, _ := os.Stat(source)
			if sha256.Sum256(after) != beforeSum || !st.ModTime().Equal(past) {
				t.Error("the cookie source was written")
			}

			calls := readCalls(record)
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
				for _, k := range []string{"yt", "pot"} {
					got := c[k]
					if k != tc.cookieOn {
						if got != "-" {
							t.Errorf("%s carried a cookie path it should not: %s", k, got)
						}
						continue
					}
					if got == "-" || got == filepath.ToSlash(source) {
						t.Errorf("%s = %q, want a private copy", k, got)
					}
					if _, err := os.Stat(got); !os.IsNotExist(err) {
						t.Errorf("the copy %s survived the run", got)
					}
				}
				if tc.cookieOn == "pot" && c["video"] != "aqz-KE-bpKQ" {
					t.Errorf("pot-accept video = %q", c["video"])
				}
			}
			if left, _ := filepath.Glob(filepath.Join(state, "live-tmp", "*")); len(left) != 0 {
				t.Errorf("run directories left behind: %v", left)
			}
			logs, _ := filepath.Glob(filepath.Join(state, "live-logs", "*.log"))
			if len(logs) != 1 {
				t.Fatalf("want one saved log, got %v", logs)
			}
			logText, _ := os.ReadFile(logs[0])
			for _, secret := range []string{fakeSID, fakeSAPISID, fakeTwitch, fakeRenewed} {
				if strings.Contains(console, secret) || strings.Contains(string(logText), secret) {
					t.Errorf("a cookie value reached the console or the log")
				}
			}
			if tc.cookieOn != "" {
				if !strings.Contains(string(logText), mask) {
					t.Error("the log holds no mask: the stand-in's leak never went through the redactor")
				}
				if !strings.Contains(console, "Self-check: 0 cookie values") {
					t.Errorf("no clean self-check line:\n%s", console)
				}
			}
		})
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
