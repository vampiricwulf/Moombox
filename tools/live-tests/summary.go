package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// The runner brackets each package run in the log with these two lines; the
// end line carries go test's own exit status, the only verdict that also sees
// a build failure, a panic or a timeout (none of which prints "--- FAIL").
var (
	beginRe   = regexp.MustCompile(`^### live-tests begin group=(\S+) pkg=(\S+)`)
	endRe     = regexp.MustCompile(`^### live-tests end group=(\S+) pkg=(\S+) exit=(\d+)`)
	runRe     = regexp.MustCompile(`^=== (?:RUN|CONT|NAME|PAUSE)\s+(\S+)`)
	verdictRe = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (\S+) \(([0-9.]+s)\)`)
	logRe     = regexp.MustCompile(`^\s+\S+_test\.go:\d+: (.*)$`)
	sessionRe = regexp.MustCompile(`LIVE-SESSION youtube=(\S+) premium=(\S+)`)
	// A group the runner did not start, and why.
	groupSkip = regexp.MustCompile(`^### live-tests skip group=(\S+) reason=(.*)$`)
)

type testVerdict struct {
	group, pkg, name, verdict, elapsed, reason string
}

type pkgRun struct {
	group, pkg string
	exit       string
	tests      int
	failed     int
}

// summarize reads a runner log and returns its verdicts, its package runs and
// the session line the probe printed (empty when none ran).
func summarize(r io.Reader) ([]testVerdict, []pkgRun, [2]string) {
	var (
		verdicts []testVerdict
		runs     []pkgRun
		session  [2]string
		cur      *pkgRun
		current  string
		lastLog  = map[string]string{}
	)
	sc := bufio.NewReader(r)
	for {
		line, err := sc.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case beginRe.MatchString(line):
			m := beginRe.FindStringSubmatch(line)
			runs = append(runs, pkgRun{group: m[1], pkg: m[2]})
			cur = &runs[len(runs)-1]
			current, lastLog = "", map[string]string{}
		case endRe.MatchString(line):
			m := endRe.FindStringSubmatch(line)
			if cur != nil && cur.pkg == m[2] {
				cur.exit = m[3]
			}
			cur = nil
		case groupSkip.MatchString(line):
			m := groupSkip.FindStringSubmatch(line)
			verdicts = append(verdicts, testVerdict{group: m[1], pkg: "(group " + m[1] + ")", name: "-", verdict: "SKIP", elapsed: "0s", reason: m[2]})
		case runRe.MatchString(line):
			current = runRe.FindStringSubmatch(line)[1]
		case verdictRe.MatchString(line):
			m := verdictRe.FindStringSubmatch(line)
			v := testVerdict{verdict: m[1], name: m[2], elapsed: m[3]}
			if cur != nil {
				v.group, v.pkg = cur.group, cur.pkg
				cur.tests++
				if v.verdict == "FAIL" {
					cur.failed++
				}
			}
			if v.verdict != "PASS" {
				v.reason = lastLog[v.name]
			}
			verdicts = append(verdicts, v)
		default:
			if m := logRe.FindStringSubmatch(line); m != nil && current != "" {
				lastLog[current] = m[1]
			}
		}
		if m := sessionRe.FindStringSubmatch(line); m != nil {
			session = [2]string{m[1], m[2]}
		}
		if err != nil {
			break
		}
	}
	return verdicts, runs, session
}

func words(s string) string { return strings.ReplaceAll(s, "_", " ") }

func runSummary(path string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot read log %s", path)
	}
	defer f.Close()
	verdicts, runs, session := summarize(f)

	yt, premium := "not checked", "unknown"
	if session[0] != "" {
		yt, premium = words(session[0]), session[1]
	}
	twitch := "not checked"
	for _, v := range verdicts {
		if v.name == "TestLiveAuthenticatedTokenValidate" {
			switch v.verdict {
			case "PASS":
				twitch = "signed in"
			case "FAIL":
				twitch = "not signed in (see the log)"
			case "SKIP":
				twitch = "no cookies"
			}
		}
	}
	fmt.Fprintf(out, "YouTube session: %s; Premium: %s; Twitch session: %s\n", yt, premium, twitch)

	failed := false
	for _, v := range verdicts {
		if strings.Contains(v.name, "/") && v.verdict == "PASS" {
			continue // a passing subtest adds nothing its parent's PASS does not say
		}
		line := fmt.Sprintf("%-4s  %-28s %s (%s)", v.verdict, v.pkg, v.name, v.elapsed)
		if v.reason != "" {
			r := v.reason
			if rs := []rune(r); len(rs) > 160 {
				r = string(rs[:160]) + "..."
			}
			line += " - " + r
		}
		fmt.Fprintln(out, line)
		if v.verdict == "FAIL" {
			failed = true
		}
	}
	for _, r := range runs {
		switch {
		case r.exit == "":
			fmt.Fprintf(out, "FAIL  %-28s (group %s) did not finish: the run was interrupted\n", r.pkg, r.group)
			failed = true
		case r.exit != "0" && r.failed == 0:
			fmt.Fprintf(out, "FAIL  %-28s (group %s) exit %s with no test verdict: a build failure, panic or timeout (see the log)\n", r.pkg, r.group, r.exit)
			failed = true
		case r.exit == "0" && r.tests == 0:
			fmt.Fprintf(out, "NONE  %-28s (group %s) no test matched: the tree has none of this group's tests, or -run excluded them\n", r.pkg, r.group)
		}
	}
	if failed {
		fmt.Fprintln(out, "RESULT: FAIL")
	} else {
		fmt.Fprintln(out, "RESULT: PASS")
	}
	return nil
}
