package main

// What the redactor masks, and how the scan checks it (leak review, RV-15):
//
//   - every spelling of every cookie value of 8+ characters in the files it
//     was given (secretForms): as is, percent-encoded both ways, decoded,
//     JSON- and JS-escaped, base64 (standard and URL-safe, padded or not)
//     and hex. The values are read exactly as the jar reads them (the line
//     and the value trimmed, cookies.CookieJar.parseRows), and the untrimmed
//     spelling is kept beside the trimmed one;
//   - any run of 16+ characters of one of those spellings (fragLen): a value
//     cut short ("%.300v"), or split across two lines, is masked where it
//     shows, the split one by joining each line's end to the next line's
//     start with go test's indent skipped;
//   - the value after a cookie's NAME (the names in the files, plus the
//     YouTube and Twitch auth names), so a renewed value YouTube just sent,
//     which no file holds yet, is masked too;
//   - the header and parameter shapes that carry a session (redactPatterns),
//     quoted or JSON-escaped as well as bare.
//
// The scan is the second reading. It counts the spellings, then the
// 16-character runs, then the runs found once every line break and space in
// the log is removed, then any cookie name still followed by a value, then
// any session header still followed by one. The first two are what the
// redactor applies; the last three are checks it applies differently or not
// at all, so a hole in the redactor's own reading shows as a count.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
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

// fragLen is the shortest run of a secret's spelling that is masked on its
// own: a cut-off or split value. 16 characters of a cookie value never occur
// by chance in test output.
const fragLen = 16

// joinSpan is how much of a line's end and the next line's start the split
// check joins: enough for any fragment window or short value to cross.
const joinSpan = 64

// mask replaces every secret the redactor finds.
const mask = "[REDACTED]"

// knownAuthNames are the session cookies of YouTube/Google and Twitch, masked
// by name even when the files given hold none of them.
var knownAuthNames = []string{
	"SID", "HSID", "SSID", "APISID", "SAPISID", "SIDCC", "LOGIN_INFO",
	"__Secure-1PSID", "__Secure-3PSID", "__Secure-1PAPISID", "__Secure-3PAPISID",
	"__Secure-1PSIDTS", "__Secure-3PSIDTS", "__Secure-1PSIDCC", "__Secure-3PSIDCC",
	"__Secure-1PSIDRTS", "__Secure-3PSIDRTS", "__Secure-ROLLOUT_TOKEN", "__Secure-YEC",
	"VISITOR_INFO1_LIVE", "VISITOR_PRIVACY_METADATA", "NID", "AEC",
	"auth-token", "persistent", "twilight-user", "api_token", "server_session_id", "unique_id",
}

// genericNames are cookie names that are also ordinary words in test output
// ("name=...", "login=..."); a file's row with one of them is masked by its
// value only.
var genericNames = map[string]bool{"name": true, "login": true, "lang": true, "language": true, "locale": true, "theme": true}

// cookieRow is what the helper keeps of one Netscape row.
type cookieRow struct {
	platform string
	name     string
	value    string // trimmed, as the jar keeps it
	raw      string // as the file spells it
}

// parseNetscape reads the rows of a Netscape cookie file exactly as the jar
// does (cookies.CookieJar.parseRows): the line is trimmed, '#' lines other
// than #HttpOnly_ ones are comments, a row has 7+ tab-separated fields, and
// the value is the trimmed join of the fields from the seventh on. Malformed
// lines are skipped silently: naming them would mean printing them.
func parseNetscape(data []byte) []cookieRow {
	var rows []cookieRow
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		domain := strings.TrimPrefix(strings.TrimSpace(fields[0]), "#HttpOnly_")
		name := strings.TrimSpace(fields[5])
		if domain == "" || name == "" {
			continue
		}
		raw := strings.Join(fields[6:], "\t")
		rows = append(rows, cookieRow{
			platform: platformOf(domain),
			name:     name,
			value:    strings.TrimSpace(raw),
			raw:      raw,
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

// jsEscapes are the escapes a JS or JSON encoder may put in a cookie value.
var jsEscapes = [][2]string{{"/", `\/`}, {"=", `\u003d`}, {"+", `\u002b`}, {"&", `\u0026`}, {"=", `\u003D`}, {"+", `\u002B`}}

// secretForms returns the spellings of one cookie value that output may
// carry (see the file comment). Values under minSecretLen give none.
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
	// JS escapes, one kind at a time and all at once.
	all := v
	for _, e := range jsEscapes {
		forms = append(forms, strings.ReplaceAll(v, e[0], e[1]))
		all = strings.ReplaceAll(all, e[0], e[1])
	}
	forms = append(forms, all)
	b := []byte(v)
	forms = append(forms,
		base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b),
		hex.EncodeToString(b), strings.ToUpper(hex.EncodeToString(b)))
	var out []string
	for _, f := range forms {
		if len(f) >= minSecretLen {
			out = append(out, f)
		}
	}
	return out
}

// redactPatterns mask what carries a session whatever its value: header lines
// (bare, quoted as a debug dump prints them, or JSON-escaped), YouTube's
// SAPISIDHASH authorization, Twitch's OAuth bearer, PO tokens and visitor
// data. Each keeps its first group.
var redactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(\b(?:set-cookie|cookie)(?:\\?["'])?\s*[:=]\s*(?:\\?["'])?\s*)[^\s,;"'\\][^\r\n]*`),
	regexp.MustCompile(`(?i)(\bauthorization(?:\\?["'])?\s*[:=]\s*(?:\\?["'])?)[^\r\n"'\\]+`),
	regexp.MustCompile(`(?i)(\bSAPISID(?:1P|3P)?HASH\s+)[0-9]+_[0-9a-f]+`),
	regexp.MustCompile(`(?i)(\b(?:OAuth|Bearer)\s+)[A-Za-z0-9._~+/-]{16,}=*`),
	regexp.MustCompile(`(?i)([?&;]pot=)[^&\s"'<>]+`),
	regexp.MustCompile(`(?i)(\bpo_?token(?:\\?["'])?\s*[:=]\s*(?:\\?["'])?)[^&\s"',<>()\\]+`),
	regexp.MustCompile(`(?i)((?:\\?["'])pot(?:\\?["'])\s*:\s*(?:\\?["']))[^"'\\]+`),
	regexp.MustCompile(`(?i)([?&;]visitor_?data=)[^&\s"'<>]+`),
	regexp.MustCompile(`(?i)(\bvisitor_?data(?:\\?["'])?\s*[:=]\s*(?:\\?["'])?)[^&\s"',<>()\\]+`),
	regexp.MustCompile(`(?i)(\bx-goog-visitor-id(?:\\?["'])?\s*[:=]\s*(?:\\?["'])?)[^\s"',\\]+`),
}

// secretSet is the values and names of the cookie files a redact or scan was
// given.
type secretSet struct {
	files   []*watchedFile
	known   map[string]bool
	names   map[string]bool
	windows map[string]struct{}
	short   []string // spellings under fragLen, matched whole only
	nameRe  *regexp.Regexp
}

type watchedFile struct {
	path  string
	size  int64
	mod   time.Time
	ready bool
}

func newSecretSet(paths []string) (*secretSet, error) {
	s := &secretSet{known: map[string]bool{}, names: map[string]bool{}, windows: map[string]struct{}{}}
	for _, n := range knownAuthNames {
		s.names[n] = true
	}
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

// reload reads w when it is new or has changed, adding its values and names
// to the set; values seen before are kept. It reports whether w could be read.
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
		if len(r.name) >= 3 && !genericNames[strings.ToLower(r.name)] {
			s.names[r.name] = true
		}
		for _, v := range []string{r.value, r.raw} {
			for _, f := range secretForms(v) {
				s.known[f] = true
			}
		}
	}
	return true
}

// refresh re-reads any file that changed and rebuilds when the set grew.
func (s *secretSet) refresh() {
	before, beforeNames := len(s.known), len(s.names)
	for _, w := range s.files {
		s.reload(w)
	}
	if len(s.known) != before || len(s.names) != beforeNames {
		s.rebuild()
	}
}

func (s *secretSet) list() []string {
	vals := make([]string, 0, len(s.known))
	for v := range s.known {
		vals = append(vals, v)
	}
	// Longest first: the scan masks as it counts, so a value that contains
	// another is counted once, whole.
	sort.Slice(vals, func(i, j int) bool {
		if len(vals[i]) != len(vals[j]) {
			return len(vals[i]) > len(vals[j])
		}
		return vals[i] < vals[j]
	})
	return vals
}

func (s *secretSet) rebuild() {
	s.short = s.short[:0]
	for _, v := range s.list() {
		if len(v) < fragLen {
			s.short = append(s.short, v)
		}
		for i := 0; i+fragLen <= len(v); i++ {
			s.windows[v[i:i+fragLen]] = struct{}{}
		}
	}
	names := make([]string, 0, len(s.names))
	for n := range s.names {
		names = append(names, regexp.QuoteMeta(n))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	// name=value, name="value", "name":"value" (also JSON-escaped); the value
	// is masked when it is minSecretLen or longer.
	s.nameRe = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(` + strings.Join(names, "|") +
		`)(=\\?["']?|\\?["']\s*:\s*\\?["'])([^;\s"'\\&,]{` + fmt.Sprint(minSecretLen) + `,})`)
}

// fragRanges returns the merged ranges of s that a 16-character window of a
// secret spelling covers, plus every whole short spelling.
func (s *secretSet) fragRanges(text string) [][2]int {
	var rs [][2]int
	for i := 0; i+fragLen <= len(text); i++ {
		if _, ok := s.windows[text[i:i+fragLen]]; ok {
			rs = append(rs, [2]int{i, i + fragLen})
		}
	}
	for _, v := range s.short {
		for off := 0; ; {
			j := strings.Index(text[off:], v)
			if j < 0 {
				break
			}
			rs = append(rs, [2]int{off + j, off + j + len(v)})
			off += j + len(v)
		}
	}
	return mergeRanges(rs)
}

func mergeRanges(rs [][2]int) [][2]int {
	if len(rs) < 2 {
		return rs
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i][0] < rs[j][0] })
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r[0] <= last[1] {
			if r[1] > last[1] {
				last[1] = r[1]
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

func applyRanges(text string, rs [][2]int) string {
	if len(rs) == 0 {
		return text
	}
	var b strings.Builder
	prev := 0
	for _, r := range rs {
		b.WriteString(text[prev:r[0]])
		b.WriteString(mask)
		prev = r[1]
	}
	b.WriteString(text[prev:])
	return b.String()
}

func (s *secretSet) maskNames(line string) string {
	return s.nameRe.ReplaceAllString(line, "${1}${2}${3}"+mask)
}

// finish masks the given ranges of a raw line, then the session shapes and
// the named values.
func (s *secretSet) finish(raw string, rs [][2]int) string {
	line := applyRanges(raw, mergeRanges(rs))
	for _, re := range redactPatterns {
		line = re.ReplaceAllString(line, "${1}"+mask)
	}
	return s.maskNames(line)
}

// redactLine masks one line on its own. Every spelling is found as ranges of
// the raw line (a spelling of fragLen or more is the union of its windows, a
// shorter one is matched whole), so a split across lines can be found on the
// same raw text (crossRanges) before anything is masked.
func (s *secretSet) redactLine(line string) string {
	return s.finish(line, s.fragRanges(line))
}

// leadingSpace is the length of go test's indent on a continuation line.
func leadingSpace(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}

// crossRanges finds a secret split across the end of a and the start of b
// (go test's indent skipped) and returns the parts of each to mask.
func (s *secretSet) crossRanges(a, b string) (ra, rb [][2]int) {
	aBody := strings.TrimRight(a, "\r\n")
	tailStart := max(0, len(aBody)-joinSpan)
	w := leadingSpace(b)
	bBody := strings.TrimRight(b, "\r\n")
	headEnd := min(len(bBody), w+joinSpan)
	if w > headEnd {
		return nil, nil
	}
	tail := aBody[tailStart:]
	k := len(tail)
	for _, r := range s.fragRanges(tail + bBody[w:headEnd]) {
		if r[0] >= k || r[1] <= k {
			continue // inside one line: that line's own pass masked it
		}
		ra = append(ra, [2]int{tailStart + r[0], tailStart + k})
		rb = append(rb, [2]int{w, w + r[1] - k})
	}
	return ra, rb
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

// runRedact filters in to out a line at a time, holding one line back so a
// secret split across a line break can be masked on both sides.
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
	// held is the raw line held back and heldRs its ranges so far.
	var (
		held   string
		heldRs [][2]int
		have   bool
	)
	emit := func() error {
		have = false
		if _, err := w.WriteString(set.finish(held, heldRs)); err != nil {
			return err
		}
		return w.Flush()
	}
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			set.refresh()
			curRs := set.fragRanges(line)
			if have {
				ra, rb := set.crossRanges(held, line)
				heldRs = append(heldRs, ra...)
				curRs = append(curRs, rb...)
				if werr := emit(); werr != nil {
					return werr
				}
			}
			held, heldRs, have = line, curRs, true
			// A line that ends without a newline is the last one, or a
			// prompt: send it on now rather than hold it.
			if !strings.HasSuffix(line, "\n") {
				if werr := emit(); werr != nil {
					return werr
				}
			}
		}
		if err == io.EOF {
			if have {
				return emit()
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// scanHit is one secret the scan found: the log line it is on and the
// reading that found it. Never its text.
type scanHit struct {
	line  int
	class string
}

// scanText runs the second reading over one log's text (see the file
// comment) and returns its hits.
func (s *secretSet) scanText(text string) []scanHit {
	var hits []scanHit
	lines := strings.SplitAfter(text, "\n")
	vals := s.list()
	// 1. Whole spellings, longest first, masked as found so one value is
	// counted once, not once per form.
	for i := range lines {
		for _, v := range vals {
			if c := strings.Count(lines[i], v); c > 0 {
				for range c {
					hits = append(hits, scanHit{i + 1, "value"})
				}
				lines[i] = strings.ReplaceAll(lines[i], v, mask)
			}
		}
	}
	// 2. The log with every space and line break removed: a value split
	// across lines. Only a run that spans a line break counts here; one
	// inside a line is the next reading's.
	var joined strings.Builder
	var lineOf []int
	for i, l := range lines {
		for j := 0; j < len(l); j++ {
			switch l[j] {
			case ' ', '\t', '\r', '\n':
				continue
			}
			joined.WriteByte(l[j])
			lineOf = append(lineOf, i+1)
		}
	}
	for _, r := range s.fragRanges(joined.String()) {
		if lineOf[r[0]] != lineOf[r[1]-1] {
			hits = append(hits, scanHit{lineOf[r[0]], "joined"})
		}
	}
	// 3. 16-character runs on each line.
	for i := range lines {
		if rs := s.fragRanges(lines[i]); len(rs) > 0 {
			for range rs {
				hits = append(hits, scanHit{i + 1, "fragment"})
			}
			lines[i] = applyRanges(lines[i], rs)
		}
	}
	// 4 and 5. A cookie name or a session header still followed by a value.
	for i, l := range lines {
		for _, m := range s.nameRe.FindAllStringSubmatch(l, -1) {
			if !strings.HasPrefix(m[4], mask) {
				hits = append(hits, scanHit{i + 1, "named"})
			}
		}
		for _, re := range redactPatterns {
			for _, m := range re.FindAllStringSubmatchIndex(l, -1) {
				if !strings.HasPrefix(l[m[3]:], mask) {
					hits = append(hits, scanHit{i + 1, "shape"})
				}
			}
		}
	}
	return hits
}

// runScan prints the number of secrets the logs hold and, with -v, one
// "<log>: line N: <class>" per hit.
func runScan(args []string, out io.Writer) (int, error) {
	verbose := false
	if len(args) > 0 && args[0] == "-v" {
		verbose, args = true, args[1:]
	}
	paths, logs := cookieArgs(args)
	if len(paths) == 0 || len(logs) == 0 {
		usage()
	}
	set, err := newSecretSet(paths)
	if err != nil {
		return 0, err
	}
	var all []string
	n := 0
	for _, l := range logs {
		data, err := os.ReadFile(l)
		if err != nil {
			return 0, fmt.Errorf("cannot read log %s", l)
		}
		for _, h := range set.scanText(string(data)) {
			n++
			all = append(all, fmt.Sprintf("%s: line %d: %s", l, h.line, h.class))
		}
	}
	fmt.Fprintf(out, "%d\n", n)
	if verbose {
		for _, a := range all {
			fmt.Fprintln(out, a)
		}
	}
	return n, nil
}
