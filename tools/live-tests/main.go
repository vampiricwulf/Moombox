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
//	live-tests redact --cookies <file>... filter stdin to stdout, masking every cookie value of
//	                                      8+ characters in those files and the header and
//	                                      parameter shapes that carry a session (redactPatterns)
//	live-tests scan --cookies <file>... <log>...
//	                                      print how many cookie values the logs hold; exit 1 if any
//	live-tests summary <log>              print the per-test verdicts of a runner log
//
// redact fails closed: when a cookie file it was given cannot be read at the
// start it passes nothing through and exits 2. It re-reads a file whose size
// or modification time changed (code under test may rewrite its copy with
// renewed cookies) and keeps masking the values it had before.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// minSecretLen is the shortest cookie value that is masked. Shorter values
// ("1", "en", "true") are flags, not secrets, and masking them would mangle
// ordinary output.
const minSecretLen = 8

// mask replaces every secret the redactor finds.
const mask = "[REDACTED]"

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
		n, err = runScan(os.Args[2:])
		if err == nil {
			fmt.Printf("%d\n", n)
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
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "live-tests:", err)
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: live-tests copy <source> <dest> | redact --cookies <file>... | scan --cookies <file>... <log>... | summary <log>")
	os.Exit(2)
}

// cookieRow is what the helper keeps of one Netscape row: its platform and
// its value. Names are never kept.
type cookieRow struct {
	platform string
	value    string
}

// parseNetscape reads the rows of a Netscape cookie file. Malformed lines are
// skipped silently: naming them would mean printing them.
func parseNetscape(data []byte) []cookieRow {
	var rows []cookieRow
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#HttpOnly_") {
			line = strings.TrimPrefix(line, "#HttpOnly_")
		} else if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		rows = append(rows, cookieRow{
			platform: platformOf(fields[0]),
			value:    strings.Join(fields[6:], "\t"),
		})
	}
	return rows
}

func platformOf(domain string) string {
	d := strings.ToLower(strings.TrimPrefix(domain, "."))
	switch {
	case d == "youtube.com" || strings.HasSuffix(d, ".youtube.com") ||
		d == "google.com" || strings.HasSuffix(d, ".google.com") ||
		strings.HasPrefix(d, "google.") || strings.Contains(d, ".google."):
		return "youtube"
	case d == "twitch.tv" || strings.HasSuffix(d, ".twitch.tv"):
		return "twitch"
	}
	return "other"
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

// secretForms returns the spellings of one cookie value that output may
// carry: as is, percent-encoded both ways, decoded, and JSON-escaped.
func secretForms(v string) []string {
	if len(v) < minSecretLen {
		return nil
	}
	forms := []string{v, url.QueryEscape(v), url.PathEscape(v)}
	if dec, err := url.QueryUnescape(v); err == nil {
		forms = append(forms, dec)
	}
	if b, err := json.Marshal(v); err == nil {
		forms = append(forms, string(b[1:len(b)-1]))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) == nil {
		s := strings.TrimSpace(buf.String())
		forms = append(forms, s[1:len(s)-1])
	}
	var out []string
	for _, f := range forms {
		if len(f) >= minSecretLen {
			out = append(out, f)
		}
	}
	return out
}

// redactPatterns mask what carries a session whatever its value: header lines
// (a renewed cookie the copy has not been re-read for yet still sits behind a
// Cookie or Set-Cookie name), YouTube's SAPISIDHASH authorization, Twitch's
// OAuth bearer, PO tokens and visitor data. Each keeps its first group.
var redactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(\b(?:set-cookie|cookie)["']?\s*[:=]\s*)[^\s,;"'][^\r\n]*`),
	regexp.MustCompile(`(?i)(\bauthorization["']?\s*[:=]\s*["']?)[^\r\n"']+`),
	regexp.MustCompile(`(?i)(\bSAPISID(?:1P|3P)?HASH\s+)[0-9]+_[0-9a-f]+`),
	regexp.MustCompile(`(?i)(\b(?:OAuth|Bearer)\s+)[A-Za-z0-9._~+/-]{16,}=*`),
	regexp.MustCompile(`(?i)([?&;]pot=)[^&\s"'<>]+`),
	regexp.MustCompile(`(?i)(\bpo_?token["']?\s*[:=]\s*["']?)[^&\s"',<>()]+`),
	regexp.MustCompile(`(?i)(["']pot["']\s*:\s*["'])[^"']+`),
	regexp.MustCompile(`(?i)([?&;]visitor_?data=)[^&\s"'<>]+`),
	regexp.MustCompile(`(?i)(\bvisitor_?data["']?\s*[:=]\s*["']?)[^&\s"',<>()]+`),
	regexp.MustCompile(`(?i)(\bx-goog-visitor-id["']?\s*[:=]\s*["']?)[^\s"',]+`),
}

// secretSet is the values of the cookie files a redact or scan was given.
type secretSet struct {
	files []*watchedFile
	known map[string]bool
	repl  *strings.Replacer
}

type watchedFile struct {
	path  string
	size  int64
	mod   time.Time
	ready bool
}

func newSecretSet(paths []string) (*secretSet, error) {
	s := &secretSet{known: map[string]bool{}}
	for i, p := range paths {
		w := &watchedFile{path: p}
		if !s.reload(w) {
			return nil, fmt.Errorf("cannot read cookie file #%d (%s)", i+1, p)
		}
		s.files = append(s.files, w)
	}
	s.rebuild()
	return s, nil
}

// reload reads w when it is new or has changed, adding its values to the set;
// values seen before are kept. It reports whether w could be read.
func (s *secretSet) reload(w *watchedFile) bool {
	st, err := os.Stat(w.path)
	if err != nil {
		return w.ready
	}
	if w.ready && st.Size() == w.size && st.ModTime().Equal(w.mod) {
		return true
	}
	data, err := os.ReadFile(w.path)
	if err != nil {
		return w.ready
	}
	w.size, w.mod, w.ready = st.Size(), st.ModTime(), true
	for _, r := range parseNetscape(data) {
		for _, f := range secretForms(r.value) {
			s.known[f] = true
		}
	}
	return true
}

// refresh re-reads any file that changed and reports whether the set grew.
func (s *secretSet) refresh() {
	before := len(s.known)
	for _, w := range s.files {
		s.reload(w)
	}
	if len(s.known) != before {
		s.rebuild()
	}
}

func (s *secretSet) list() []string {
	vals := make([]string, 0, len(s.known))
	for v := range s.known {
		vals = append(vals, v)
	}
	// Longest first: strings.Replacer tries its pairs in argument order, so
	// a value that contains another is masked whole.
	sort.Slice(vals, func(i, j int) bool {
		if len(vals[i]) != len(vals[j]) {
			return len(vals[i]) > len(vals[j])
		}
		return vals[i] < vals[j]
	})
	return vals
}

func (s *secretSet) rebuild() {
	var pairs []string
	for _, v := range s.list() {
		pairs = append(pairs, v, mask)
	}
	s.repl = strings.NewReplacer(pairs...)
}

func (s *secretSet) redactLine(line string) string {
	line = s.repl.Replace(line)
	for _, re := range redactPatterns {
		line = re.ReplaceAllString(line, "${1}"+mask)
	}
	return line
}

func cookieArgs(args []string) (cookies, rest []string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--cookies" && i+1 < len(args) {
			cookies = append(cookies, args[i+1])
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return cookies, rest
}

func runRedact(args []string, in io.Reader, out io.Writer) error {
	paths, rest := cookieArgs(args)
	if len(rest) != 0 {
		return errors.New("redact takes only --cookies <file> arguments")
	}
	set, err := newSecretSet(paths)
	if err != nil {
		return err
	}
	r := bufio.NewReaderSize(in, 64*1024)
	w := bufio.NewWriter(out)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			set.refresh()
			if _, werr := w.WriteString(set.redactLine(line)); werr != nil {
				return werr
			}
			if werr := w.Flush(); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func runScan(args []string) (int, error) {
	paths, logs := cookieArgs(args)
	if len(paths) == 0 || len(logs) == 0 {
		usage()
	}
	set, err := newSecretSet(paths)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range logs {
		data, err := os.ReadFile(l)
		if err != nil {
			return 0, fmt.Errorf("cannot read log %s", l)
		}
		// Count on a working copy that is masked as it goes, longest value
		// first, so one leaked value is counted once, not once per form.
		text := string(data)
		for _, v := range set.list() {
			if c := strings.Count(text, v); c > 0 {
				n += c
				text = strings.ReplaceAll(text, v, mask)
			}
		}
	}
	return n, nil
}
