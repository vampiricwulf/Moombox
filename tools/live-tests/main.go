// Command live-tests is the helper behind .github/scripts/live-tests.sh, the
// one-command runner for Moombox's live tests (owner ruling RV-15). It is
// test support only: nothing in the moombox binary imports it.
//
// The runner never reads the owner's cookie file itself; every read of it,
// and of the private copies the tests are given, goes through this program,
// which prints at most counts about it: its size, its age, how many rows it
// holds per platform. No subcommand ever prints a cookie name, value or line.
//
//	live-tests copy <source> <dest>       copy the source (opened read-only) to dest, mode 0600,
//	                                      and print "size=N age_s=N rows_youtube=N rows_twitch=N rows_other=N"
//	live-tests redact --cookies <file>... filter stdin to stdout (redact.go says what it masks)
//	live-tests scan [-v] --cookies <file>... <log>...
//	                                      print how many secrets the logs hold, by an
//	                                      independent second reading; exit 1 if any. -v adds
//	                                      one "line N: class" per hit, never the text
//	live-tests summary <log>              print the per-test verdicts of a runner log
//	live-tests pick-twitch-live           print the login of a Twitch channel that is live now
//	                                      (Twitch's anonymous top-streams query)
//
// redact fails closed: when a cookie file it was given cannot be read at the
// start it passes nothing through and exits 2. It re-reads a file whose size
// or modification time changed (code under test may rewrite its copy with
// renewed cookies) and keeps masking the values it had before.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "copy":
		err = runCopy(os.Args[2:], os.Stdout)
	case "redact":
		err = runRedact(os.Args[2:], os.Stdin, os.Stdout)
		if err != nil {
			fmt.Fprintln(os.Stderr, "live-tests redact:", err)
			os.Exit(2)
		}
		return
	case "scan":
		var n int
		n, err = runScan(os.Args[2:], os.Stdout)
		if err == nil {
			if n > 0 {
				os.Exit(1)
			}
			return
		}
	case "summary":
		if len(os.Args) != 3 {
			usage()
		}
		err = runSummary(os.Args[2], os.Stdout)
	case "pick-twitch-live":
		err = runPickTwitchLive(os.Stdout)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "live-tests:", err)
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: live-tests copy <source> <dest> | redact --cookies <file>... | scan [-v] --cookies <file>... <log>... | summary <log> | pick-twitch-live")
	os.Exit(2)
}

func runCopy(args []string, out io.Writer) error {
	if len(args) != 2 {
		usage()
	}
	src, dst := args[0], args[1]
	in, err := openSourceShared(src)
	if err != nil {
		return errors.New("cannot open the cookie source (path only: " + src + ")")
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return errors.New("the cookie source is not a regular file: " + src)
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return errors.New("cannot read the cookie source: " + src)
	}
	o, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("cannot create the private copy %s: %w", dst, err)
	}
	if _, err := o.Write(data); err != nil {
		o.Close()
		os.Remove(dst)
		return fmt.Errorf("cannot write the private copy %s", dst)
	}
	if err := o.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("cannot write the private copy %s", dst)
	}
	counts := map[string]int{}
	for _, r := range parseNetscape(data) {
		counts[r.platform]++
	}
	fmt.Fprintf(out, "size=%d age_s=%d rows_youtube=%d rows_twitch=%d rows_other=%d\n",
		len(data), int64(time.Since(st.ModTime()).Seconds()),
		counts["youtube"], counts["twitch"], counts["other"])
	return nil
}
