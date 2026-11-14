package cookies

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// renewalLog records every line at every level, as "LEVEL msg k=v ...".
type renewalLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *renewalLog) add(level, msg string, args ...any) {
	var b strings.Builder
	b.WriteString(level + " " + msg)
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	l.mu.Lock()
	l.lines = append(l.lines, b.String())
	l.mu.Unlock()
}
func (l *renewalLog) Debug(msg string, args ...any) { l.add("DEBUG", msg, args...) }
func (l *renewalLog) Info(msg string, args ...any)  { l.add("INFO", msg, args...) }
func (l *renewalLog) Warn(msg string, args ...any)  { l.add("WARN", msg, args...) }
func (l *renewalLog) Error(msg string, args ...any) { l.add("ERROR", msg, args...) }

func (l *renewalLog) containing(level, needle string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.HasPrefix(line, level+" ") && strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}

func (l *renewalLog) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

const renewalSeed = "# Netscape HTTP Cookie File\n" +
	".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSAPISID\tsapisid-A\n" +
	".youtube.com\tTRUE\t/\tTRUE\t2000000000\tLOGIN_INFO\tlogin-A\n" +
	".youtube.com\tTRUE\t/\tTRUE\t2000000000\tSID\tsid-A\n" +
	".youtube.com\tTRUE\t/\tTRUE\t2000000000\t__Secure-1PSIDCC\tsidcc-old\n"

// renewalJar is a jar loaded from a cookies.txt holding session A, with a
// recording logger.
func renewalJar(t *testing.T) (*CookieJar, string, *renewalLog) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(renewalSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	log := &renewalLog{}
	jar.SetRenewalLogger(log)
	return jar, path, log
}

// replyTo builds the reply a GET of rawURL carrying cookieHeader drew.
func replyTo(t *testing.T, rawURL, cookieHeader string, status int, setCookies ...string) YouTubeReply {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{URL: u, Header: make(http.Header)}
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	resp := &http.Response{StatusCode: status, Header: make(http.Header), Request: req}
	for _, sc := range setCookies {
		resp.Header.Add("Set-Cookie", sc)
	}
	return ReplyOf(resp).From("membership page", "web page", ReplyNoVerdict)
}

func fileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const sidccRenewal = "__Secure-1PSIDCC=sidcc-new; Domain=.youtube.com; Path=/; Max-Age=31536000"

// A renewal on a youtube.com reply to a request that carried the session is
// in the jar at once and in cookies.txt at the first flush.
func TestRenewalIsKeptInTheJarAndTheFile(t *testing.T) {
	jar, path, _ := renewalJar(t)

	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/feed/x", jar.GetCookieHeader(), 200, sidccRenewal))

	if got := jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "sidcc-new" {
		t.Errorf("jar __Secure-1PSIDCC = %q, want the renewed value: the next request must send it", got)
	}
	if !strings.Contains(fileText(t, path), "\t__Secure-1PSIDCC\tsidcc-new") {
		t.Errorf("cookies.txt does not hold the renewed __Secure-1PSIDCC after the first flush")
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "sid-A" {
		t.Errorf("an unrelated cookie moved: SID = %q", got)
	}
}

// What is not kept, one rule per row. Each row differs from the kept case in
// one thing.
func TestRenewalIsNotKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		url    string
		cookie func(j *CookieJar) string
		status int
		header string
		edit   func(r YouTubeReply) YouTubeReply
	}{
		{name: "a googlevideo host", url: "https://rr3---sn-x.googlevideo.com/videoplayback"},
		{name: "a google.com host", url: "https://accounts.google.com/x"},
		{name: "a look-alike host", url: "https://evil-youtube.com/x"},
		{name: "plain http", url: "http://www.youtube.com/x"},
		{name: "a 403", status: 403},
		{name: "a 302", status: 302},
		{name: "a request with no cookies", cookie: func(*CookieJar) string { return "" }},
		{name: "a request with the consent cookies alone", cookie: func(*CookieJar) string { return "SOCS=CAI; PREF=hl=en" }},
		{name: "a request that carried another session", cookie: func(*CookieJar) string { return "SAPISID=sapisid-B; LOGIN_INFO=login-B" }},
		{name: "a reply read as signed out", edit: func(r YouTubeReply) YouTubeReply { r.Verdict = ReplySignedOut; return r }},
		{name: "a name the jar does not track", header: "foo=sidcc-new; Domain=.youtube.com; Path=/"},
		{name: "a domain off the platform", header: "__Secure-1PSIDCC=sidcc-new; Domain=.evil.tld; Path=/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar, path, _ := renewalJar(t)
			rawURL, status, header := "https://www.youtube.com/x", 200, sidccRenewal
			if tc.url != "" {
				rawURL = tc.url
			}
			if tc.status != 0 {
				status = tc.status
			}
			if tc.header != "" {
				header = tc.header
			}
			cookie := jar.GetCookieHeader()
			if tc.cookie != nil {
				cookie = tc.cookie(jar)
			}
			reply := replyTo(t, rawURL, cookie, status, header)
			if tc.edit != nil {
				reply = tc.edit(reply)
			}

			jar.OfferYouTubeReply(reply)
			jar.FlushRenewals()

			if strings.Contains(jar.GetCookieHeader(), "sidcc-new") {
				t.Errorf("the jar took the value")
			}
			if strings.Contains(fileText(t, path), "sidcc-new") {
				t.Errorf("cookies.txt took the value")
			}
		})
	}
}

// A deletion or an empty value is never applied, whatever the reply's
// verdict. For an auth cookie one Warn names the cookie, the endpoint and the
// client; for any other name nothing is logged.
func TestRenewalStoreNeverAppliesADeletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   string
		verdict  ReplyVerdict
		wantWarn bool
	}{
		{"LOGIN_INFO expired in the past", "LOGIN_INFO=; Domain=.youtube.com; Expires=Thu, 01-Jan-1970 00:00:00 GMT; Path=/", ReplyNoVerdict, true},
		{"LOGIN_INFO max-age 0 on a signed-out reply", "LOGIN_INFO=gone; Max-Age=0; Domain=.youtube.com", ReplySignedOut, true},
		{"SAPISID unscoped max-age 0", "SAPISID=; Max-Age=0", ReplySignedIn, true},
		{"SID empty with no expiry", "SID=; Domain=.youtube.com; Path=/", ReplySignedIn, true},
		{"a cookie that is not an auth cookie", "__Secure-1PSIDCC=; Max-Age=0; Domain=.youtube.com", ReplySignedIn, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar, path, log := renewalJar(t)
			before := jar.GetCookieHeader()
			reply := replyTo(t, "https://www.youtube.com/x", before, 200, tc.header)
			reply.Verdict = tc.verdict

			jar.OfferYouTubeReply(reply)
			jar.OfferYouTubeReply(reply) // a dead session repeats it on every request
			jar.FlushRenewals()

			if got := jar.GetCookieHeader(); got != before {
				t.Errorf("the jar changed: a deletion was applied")
			}
			if got := fileText(t, path); got != renewalSeed {
				t.Errorf("cookies.txt changed: a deletion was applied\n%s", got)
			}
			warns := log.containing("WARN", "cleared an auth cookie")
			if !tc.wantWarn {
				if len(warns) != 0 {
					t.Errorf("want no Warn for a cookie that is not an auth cookie, got %v", warns)
				}
				return
			}
			if len(warns) != 1 {
				t.Fatalf("want exactly one Warn for two identical replies, got %d: %v", len(warns), warns)
			}
			name, _, _ := strings.Cut(tc.header, "=")
			for _, want := range []string{"cookie=" + name, "endpoint=membership page", "client=web page"} {
				if !strings.Contains(warns[0], want) {
					t.Errorf("the Warn does not say %q: %s", want, warns[0])
				}
			}
		})
	}
}

// cookies.txt is written at most once a minute: a second renewal inside the
// minute is in the jar, survives a reload of the unchanged file, and reaches
// the file with the next flush.
func TestSecondRenewalWaitsForTheNextFlush(t *testing.T) {
	jar, path, _ := renewalJar(t)
	offer := func(v string) {
		jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200,
			"__Secure-1PSIDCC="+v+"; Domain=.youtube.com; Path=/; Max-Age=31536000"))
	}
	offer("sidcc-first")
	offer("sidcc-second")

	if got := jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "sidcc-second" {
		t.Errorf("jar __Secure-1PSIDCC = %q, want sidcc-second at once", got)
	}
	if got := fileText(t, path); !strings.Contains(got, "sidcc-first") || strings.Contains(got, "sidcc-second") {
		t.Errorf("cookies.txt was written twice inside a minute, or not at all:\n%s", got)
	}
	// The file is inside the jar's settle window, so this Load re-reads it.
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "sidcc-second" {
		t.Errorf("a reload put the jar back on %q", got)
	}
	jar.renew.mu.Lock()
	armed := jar.renew.timer != nil
	jar.renew.mu.Unlock()
	if !armed {
		t.Errorf("no flush is scheduled for the waiting renewal")
	}

	jar.FlushRenewals() // what shutdown does
	if got := fileText(t, path); !strings.Contains(got, "\t__Secure-1PSIDCC\tsidcc-second") {
		t.Errorf("the shutdown flush did not write the waiting renewal:\n%s", got)
	}
}

// A waiting renewal is for the session it was kept under. A cookies.txt that
// comes to hold another session does not get it, in the jar or in the file.
func TestWaitingRenewalIsDroppedWhenTheFileChangesSession(t *testing.T) {
	jar, path, _ := renewalJar(t)
	jar.renew.mu.Lock()
	jar.renew.lastFlush = time.Now() // so the renewal below waits
	jar.renew.mu.Unlock()
	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200, sidccRenewal))

	sessionB := strings.NewReplacer("sapisid-A", "sapisid-B", "login-A", "login-B", "sidcc-old", "sidcc-B").Replace(renewalSeed)
	if err := os.WriteFile(path, []byte(sessionB), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	jar.FlushRenewals()

	if got := jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "sidcc-B" {
		t.Errorf("jar __Secure-1PSIDCC = %q, want session B's own", got)
	}
	if got := fileText(t, path); got != sessionB {
		t.Errorf("session A's renewal was written into session B's file:\n%s", got)
	}
}

// A writer that holds the flush (a browser pass, an import, a setup) reads a
// file that already has every earlier renewal, and a renewal that arrives
// while it works wins over what it writes: in the jar after its reload, and
// in the file after its release.
func TestRenewalDuringAHeldWriteWinsOverWhatTheWriterWrote(t *testing.T) {
	jar, path, _ := renewalJar(t)
	jar.renew.mu.Lock()
	jar.renew.lastFlush = time.Now()
	jar.renew.mu.Unlock()
	offer := func(header string) {
		jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200, header))
	}
	offer("SID=sid-before-pass; Domain=.youtube.com; Path=/; Max-Age=31536000")

	release := jar.HoldRenewalFlush()
	if got := fileText(t, path); !strings.Contains(got, "sid-before-pass") {
		t.Fatalf("the hold did not write the waiting renewal, so the writer would not read it:\n%s", got)
	}
	offer(sidccRenewal) // arrives after the pass started

	// The writer: the profile's values win by name, as the merge does.
	fromProfile := strings.NewReplacer("sid-before-pass", "sid-profile", "sidcc-old", "sidcc-profile").Replace(fileText(t, path))
	if err := os.WriteFile(path, []byte(fromProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "sidcc-new" {
		t.Errorf("jar __Secure-1PSIDCC = %q after the writer's reload, want the value YouTube sent during the pass", got)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "SID"); got != "sid-profile" {
		t.Errorf("jar SID = %q, want the profile's: a renewal from before the pass does not outrank it", got)
	}
	if strings.Contains(fileText(t, path), "sidcc-new") {
		t.Errorf("the flush wrote cookies.txt while a writer held it")
	}

	release()
	jar.FlushRenewals()
	got := fileText(t, path)
	if !strings.Contains(got, "\t__Secure-1PSIDCC\tsidcc-new") || !strings.Contains(got, "\tSID\tsid-profile") {
		t.Errorf("after the release the file should hold the renewal from during the pass and the profile's SID:\n%s", got)
	}
}

// One Info line when the set of names an endpoint renews grows, not one per
// reply, and none when two replies alternate between sets already seen.
func TestRenewedNamesAreLoggedOncePerEndpoint(t *testing.T) {
	jar, _, log := renewalJar(t)
	offer := func(endpoint string, headers ...string) {
		r := replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200, headers...)
		r.Endpoint = endpoint
		jar.OfferYouTubeReply(r)
	}
	sid := "SID=sid-renewed; Domain=.youtube.com; Path=/; Max-Age=31536000"
	offer("membership page", sidccRenewal)
	offer("membership page", sidccRenewal)
	offer("membership page", sid)
	offer("membership page", sidccRenewal)
	offer("membership page", sid)
	offer("player", sidccRenewal)

	lines := log.containing("INFO", "renewed session cookies")
	want := []string{
		"endpoint=membership page client=web page names=__Secure-1PSIDCC",
		"endpoint=membership page client=web page names=SID,__Secure-1PSIDCC",
		"endpoint=player client=web page names=__Secure-1PSIDCC",
	}
	if len(lines) != len(want) {
		t.Fatalf("want %d Info lines, got %d: %v", len(want), len(lines), lines)
	}
	for i := range want {
		if !strings.HasSuffix(lines[i], want[i]) {
			t.Errorf("line %d = %q, want it to end %q", i, lines[i], want[i])
		}
	}
}

// No log line the renewal store writes carries a cookie value: not a kept
// one, not a refused one, not a cleared one, not one already in the jar.
func TestRenewalLogsNeverCarryACookieValue(t *testing.T) {
	const sentinel = "SENTINEL-c00kie-VALUE"
	path := filepath.Join(t.TempDir(), "cookies.txt")
	seed := strings.NewReplacer("sapisid-A", sentinel+"1", "login-A", sentinel+"2", "sid-A", sentinel+"3", "sidcc-old", sentinel+"4").Replace(renewalSeed)
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	jar := NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	log := &renewalLog{}
	jar.SetRenewalLogger(log)

	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200,
		"__Secure-1PSIDCC="+sentinel+"5; Domain=.youtube.com; Path=/; Max-Age=31536000", // kept
		"__Secure-ROLLOUT_TOKEN="+sentinel+"6; Path=/",                                  // kept, new row
		"foo="+sentinel+"7; Domain=.youtube.com",                                        // not tracked
		"SID="+sentinel+"8; Domain=.evil.tld",                                           // refused
		"LOGIN_INFO="+sentinel+"9; Max-Age=0; Domain=.youtube.com",                      // cleared
		"HSID=; Domain=.youtube.com",                                                    // emptied
		"YSC="+sentinel+"\t10; Domain=.youtube.com",                                     // row-breaking
	))
	// A flush that fails, so the write path's own error and Warn are in the
	// log too.
	jar.renew.mu.Lock()
	jar.renew.lastFlush = time.Now()
	jar.renew.mu.Unlock()
	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200,
		"__Secure-1PSIDCC="+sentinel+"11; Domain=.youtube.com; Path=/; Max-Age=31536000"))
	// cookies.txt becomes a directory: unreadable, but not absent.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	jar.FlushRenewals()
	jar.FlushRenewals() // the retry a minute later fails the same way
	if n := len(log.containing("WARN", "failed to update cookie file")); n != 1 {
		t.Errorf("a cookies.txt that cannot be written was warned about %d times in one hour, want 1", n)
	}

	out := log.all()
	if len(log.containing("WARN", "cleared an auth cookie")) == 0 || len(log.containing("INFO", "renewed session cookies")) == 0 ||
		len(log.containing("WARN", "failed to update cookie file")) == 0 {
		t.Fatalf("the test did not reach the lines it is checking:\n%s", out)
	}
	if strings.Contains(out, "SENTINEL") {
		t.Errorf("a cookie value reached the log:\n%s", out)
	}
}

// The guide check offers its reply with its own verdict: a signed-in reply's
// renewal is kept, a signed-out reply's is not, and an auth cookie a
// signed-out reply clears is logged and left in the store.
func TestGuideReplyIsOfferedWithItsVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantKept bool
	}{
		{"signed in", loggedInGuideBody, true},
		{"signed out", loggedOutGuideBody, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar, path, _ := renewalJar(t)
			log := &renewalLog{}
			rs := NewRefreshService(jar, 0, log)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Set-Cookie", sidccRenewal)
				w.Header().Add("Set-Cookie", "LOGIN_INFO=; Max-Age=0; Domain=.youtube.com")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			pointYouTubeGuideAt(t, srv)

			authenticated, err := rs.checkAndRefreshYouTube(context.Background())
			if err != nil || authenticated != tc.wantKept {
				t.Fatalf("checkAndRefreshYouTube = %v, %v", authenticated, err)
			}

			kept := strings.Contains(fileText(t, path), "sidcc-new") && jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC") == "sidcc-new"
			if kept != tc.wantKept {
				t.Errorf("renewal kept = %v, want %v", kept, tc.wantKept)
			}
			if jar.GetCookieFor(PlatformYouTube, "LOGIN_INFO") != "login-A" || !strings.Contains(fileText(t, path), "login-A") {
				t.Errorf("LOGIN_INFO was removed from the store")
			}
			if warns := log.containing("WARN", "cleared an auth cookie"); len(warns) != 1 ||
				!strings.Contains(warns[0], "cookie=LOGIN_INFO endpoint=guide client=WEB") {
				t.Errorf("want one Warn naming LOGIN_INFO, the guide and the WEB client, got %v", warns)
			}
		})
	}
}

// Deleting cookies.txt is how an operator logs Moombox out. A renewal still
// waiting to be written must not bring the file, or the session, back.
func TestWaitingRenewalDoesNotOutliveADeletedCookieFile(t *testing.T) {
	for _, reload := range []bool{true, false} {
		jar, path, log := renewalJar(t)
		jar.renew.mu.Lock()
		jar.renew.lastFlush = time.Now() // so the renewal below waits
		jar.renew.mu.Unlock()
		jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200, sidccRenewal))

		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if reload {
			if err := jar.Load(path); err != nil {
				t.Fatal(err)
			}
			if !jar.IsEmpty() {
				t.Errorf("the jar still holds cookies after cookies.txt was deleted")
			}
		}
		jar.FlushRenewals()

		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("reload=%v: the flush brought cookies.txt back (stat err = %v)", reload, err)
		}
		if warns := log.containing("WARN", ""); len(warns) != 0 {
			t.Errorf("reload=%v: a deliberate delete was warned about: %v", reload, warns)
		}
	}
}
