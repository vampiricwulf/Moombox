package cookies

// renewals.go — keeping the session cookies YouTube renews on its replies.
//
// A browser applies every Set-Cookie it is sent and sends the newest value on
// its next request. Moombox used to keep only what the guide check's reply
// carried and replay the copy from the last browser visit on everything else.
// CookieJar.OfferYouTubeReply is the one place a reply's Set-Cookie headers
// reach the cookie store: every request that carries the session cookies to a
// youtube.com host offers its reply here, with one exception. The guide POST
// that judges a pass's merged cookies (CheckYouTubeAuth, the verify path)
// does not: the reply that decides whether an import is kept or rolled back
// must not also write the jar it is judging.
//
// What is kept:
//
//   - only a reply from youtube.com or a subdomain of it, over https, with a
//     2xx status, to a request that carried the session the jar holds now;
//   - only a cookie the jar keeps for YouTube, with a value: a name it
//     tracks (trackedCookieName), or any name scoped where www.youtube.com
//     is sent it with Path=/ (wwwYouTubeRow, owner ruling KA-7; plain SIDCC
//     is one);
//   - never from a reply whose caller read it as signed out.
//
// A deletion or an empty value is never applied. For an auth cookie it is
// logged at Warn, by name, with the endpoint and the client that drew it.
//
// A value the jar already holds, re-sent with an expiry less than
// renewalExpirySlack later, is not a renewal: nothing is kept, stamped or
// written for it.
//
// A kept renewal is in the jar at once, so the next request sends it.
// cookies.txt is rewritten at most once a minute while renewals are waiting,
// always from the flush timer's goroutine and never on the request path, and
// once more on shutdown (FlushRenewals). Nothing here is persisted anywhere
// else: the times renewals were received live in memory only.
//
// Only the final reply of a redirect chain is seen. A Set-Cookie on a
// redirect hop is not.
//
// No cookie value is ever logged: names, counts and endpoints only.

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// renewalFlushEvery is the least time between two writes of cookies.txt for
// renewals.
const renewalFlushEvery = time.Minute

// renewalWarnEvery is the least time between two Warn lines for the same
// cleared auth cookie on the same endpoint and client (a dead session draws
// the same deletion on every request, about a thousand a day), between two
// Info lines for a signed-out reply's cookies on the same endpoint and
// client, and between two Warn lines for a cookies.txt that cannot be
// written.
const renewalWarnEvery = time.Hour

// renewalExpirySlack is how much later a re-sent cookie's expiry must be,
// with its value unchanged, before the re-send counts as a renewal. YouTube
// re-sends some cookies unchanged on most replies with the expiry moved on by
// the seconds since the last one; keeping each would rewrite cookies.txt
// every minute for nothing. A day's slack still moves a long-lived cookie's
// expiry on daily.
const renewalExpirySlack = 24 * time.Hour

// ReplyVerdict is what the caller read off a reply about the session.
type ReplyVerdict int

const (
	// ReplyNoVerdict is a reply its caller reads no session verdict from, or
	// one whose verdict was inconclusive.
	ReplyNoVerdict ReplyVerdict = iota
	// ReplySignedIn is a reply read as signed in.
	ReplySignedIn
	// ReplySignedOut is a reply read as signed out. Its renewals are not
	// kept.
	ReplySignedOut
)

// YouTubeReply is what OfferYouTubeReply needs of one HTTP reply. ReplyOf
// fills the unexported part from the response; the caller names the endpoint,
// the client and the verdict.
//
// It never holds the request's Cookie header or a raw Set-Cookie line: ReplyOf
// reduces the first to the session's fingerprint and the second to the
// renewals the store may keep. Those renewals are still credentials, so the
// reply holds them inside a func value: fmt can print a func only as an
// address, with any verb and at any depth, so a struct with a reply in it
// (a page result) cannot print them by accident. String and GoString print
// the endpoint and counts only.
type YouTubeReply struct {
	// Endpoint names the request for the log: "guide", "membership page".
	Endpoint string
	// Client is the client identity the request was made as: an Innertube
	// client name, or "web page" for an HTML page.
	Client string
	// Verdict is what the caller read off the reply about the session.
	Verdict ReplyVerdict

	scheme string
	host   string
	status int
	// held returns what the Set-Cookie headers came to; nil when they came
	// to nothing. A func and not a pointer, for the reason above.
	held func() *replyCookies
}

// replyCookies is what a reply's Set-Cookie headers came to.
type replyCookies struct {
	// sentAs is the session fingerprint of the Cookie header the answered
	// request carried; "" when it carried no session.
	sentAs string
	// updates are the renewals the store may keep.
	updates map[cookieUpdateKey]cookieUpdate
	// clearedAuth and clearedOther are the names of the tracked cookies the
	// reply cleared, auth cookies and the rest, each sorted.
	clearedAuth  []string
	clearedOther []string
}

// ReplyOf captures what OfferYouTubeReply reads from resp: where the final
// reply came from, its status, which session the answering request carried
// and what the reply's Set-Cookie headers renew or clear. A reply that sets
// and clears no cookie the jar tracks gives the zero value, which
// OfferYouTubeReply ignores.
func ReplyOf(resp *http.Response) YouTubeReply {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return YouTubeReply{}
	}
	setCookies := resp.Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		return YouTubeReply{}
	}
	updates, clearedAuth, clearedOther := classifyYouTubeSetCookies(setCookies)
	if len(updates) == 0 && len(clearedAuth) == 0 && len(clearedOther) == 0 {
		return YouTubeReply{}
	}
	held := &replyCookies{
		sentAs:       youTubeSessionKeyOfHeader(resp.Request.Header.Get("Cookie")),
		updates:      updates,
		clearedAuth:  clearedAuth,
		clearedOther: clearedOther,
	}
	return YouTubeReply{
		scheme: resp.Request.URL.Scheme,
		host:   resp.Request.URL.Hostname(),
		status: resp.StatusCode,
		held:   func() *replyCookies { return held },
	}
}

// Empty reports whether r sets and clears no cookie the jar tracks, so there
// is nothing to offer and no reason to work out a verdict for it.
func (r YouTubeReply) Empty() bool { return r.held == nil }

// From returns r with its endpoint, client and verdict set.
func (r YouTubeReply) From(endpoint, client string, verdict ReplyVerdict) YouTubeReply {
	r.Endpoint, r.Client, r.Verdict = endpoint, client, verdict
	return r
}

// String is what a %v of a reply prints: where it came from and how many
// cookies it renews and clears. Never a name's value.
func (r YouTubeReply) String() string {
	renews, clears := 0, 0
	if r.held != nil {
		held := r.held()
		renews, clears = len(held.updates), len(held.clearedAuth)+len(held.clearedOther)
	}
	return fmt.Sprintf("YouTubeReply{endpoint=%q client=%q status=%d renews=%d clears=%d}",
		r.Endpoint, r.Client, r.status, renews, clears)
}

// GoString is String, for %#v.
func (r YouTubeReply) GoString() string { return r.String() }

// renewalState is the jar's renewal store. Its lock is taken before the
// jar's own and never while holding it.
type renewalState struct {
	mu     sync.Mutex
	logger interface {
		Debug(msg string, args ...any)
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// pending are the renewals kept since cookies.txt was last written.
	// pendingBase is the session key of the file they were first applied
	// over: a file holding another session does not get them.
	pending     map[cookieUpdateKey]cookieUpdate
	pendingBase string

	lastFlush time.Time
	timer     *time.Timer
	// held counts the writers of cookies.txt that have asked the flush to
	// wait for them (HoldRenewalFlush).
	held int

	// file is the last read of cookies.txt, trusted while the file's size
	// and modification time do not move. applied is that file's text with
	// the pending renewals laid in, which is what the jar holds; it is valid
	// only for the file read and the pending set it was made from.
	file    renewalFile
	applied struct {
		ok   bool
		text string
		key  string
	}

	// endpointNames is the set of names each endpoint and client has renewed
	// so far in this process. It only grows, so two alternating replies
	// cannot make the Info line repeat. sincePass is the same set since
	// TakeReplyRenewals last took it (once per browser pass), so a name that
	// stopped being renewed shows as missing from the next pass's line.
	endpointNames map[string]map[string]bool
	sincePass     map[string]map[string]bool
	// warned is when each throttled line was last logged: a cleared auth
	// cookie's Warn and a signed-out reply's Info.
	warned map[string]time.Time
	// lastFlushWarn is when a failed write of cookies.txt was last logged at
	// Warn. The write is retried every minute (flushRenewalsLocked arms the
	// timer again); the Warn is not.
	lastFlushWarn time.Time
	// lastAccepted is when a reply's renewal was last kept.
	lastAccepted time.Time
}

// renewalFile is one read of cookies.txt and the session it holds.
type renewalFile struct {
	ok   bool
	path string
	size int64
	mod  time.Time
	data []byte
	key  string
}

// discardRenewalLogger is the logger a jar with none set logs to.
type discardRenewalLogger struct{}

func (discardRenewalLogger) Debug(string, ...any) {}
func (discardRenewalLogger) Info(string, ...any)  {}
func (discardRenewalLogger) Warn(string, ...any)  {}
func (discardRenewalLogger) Error(string, ...any) {}

// SetRenewalLogger sets where the renewal store logs. NewRefreshService sets
// it to its own logger; a jar with none logs nothing.
func (j *CookieJar) SetRenewalLogger(logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) {
	if j == nil {
		return
	}
	j.renew.mu.Lock()
	defer j.renew.mu.Unlock()
	j.renew.logger = logger
}

// log returns the renewal logger. Callers hold r.mu.
func (r *renewalState) log() interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
} {
	if r.logger == nil {
		return discardRenewalLogger{}
	}
	return r.logger
}

// replyHostRule is the one seam on the host rule: nil, which means
// isYouTubeReplyHost, except in a test that has called
// AllowReplyHostForTesting.
var replyHostRule struct {
	mu sync.RWMutex
	ok func(scheme, host string) bool
}

// replyHostOK applies the host rule in force.
func replyHostOK(scheme, host string) bool {
	replyHostRule.mu.RLock()
	ok := replyHostRule.ok
	replyHostRule.mu.RUnlock()
	if ok == nil {
		ok = isYouTubeReplyHost
	}
	return ok(scheme, host)
}

// AllowReplyHostForTesting replaces, until tb ends, the rule that a reply
// must come from youtube.com over https, so a test can offer replies from an
// httptest server. It takes a testing.TB so that nothing but a test can call
// it: the rule is what keeps a reply from any other host from writing session
// cookies.
func AllowReplyHostForTesting(tb testing.TB, ok func(scheme, host string) bool) {
	tb.Helper()
	replyHostRule.mu.Lock()
	prev := replyHostRule.ok
	replyHostRule.ok = ok
	replyHostRule.mu.Unlock()
	tb.Cleanup(func() {
		replyHostRule.mu.Lock()
		replyHostRule.ok = prev
		replyHostRule.mu.Unlock()
	})
}

// isYouTubeReplyHost is the host rule: https, and youtube.com or a subdomain
// of it. Never googlevideo.com, never google.com.
func isYouTubeReplyHost(scheme, host string) bool {
	return strings.EqualFold(scheme, "https") && isYouTubeDomain(host)
}

// OfferYouTubeReply offers one reply's Set-Cookie headers to the cookie
// store. See the file comment for what is kept. It is safe to call with the
// zero value, and safe on a nil jar.
func (j *CookieJar) OfferYouTubeReply(reply YouTubeReply) {
	if j == nil || reply.held == nil {
		return
	}
	if !replyHostOK(reply.scheme, reply.host) {
		return
	}
	held := reply.held()
	r := &j.renew
	r.mu.Lock()
	defer r.mu.Unlock()

	// The reply is about the jar's session only when the request that was
	// answered carried it. A request with the consent cookies alone, one a
	// redirect stripped, and one sent before an import replaced the session
	// all fail here.
	if held.sentAs == "" || held.sentAs != j.youTubeSessionKey() {
		return
	}

	j.warnClearedLocked(held.clearedAuth, reply)
	if reply.status < 200 || reply.status > 299 {
		return
	}
	if reply.Verdict == ReplySignedOut {
		j.noteSignedOutReplyLocked(reply)
		return
	}
	j.acceptRenewalsLocked(held.updates, reply.Endpoint, reply.Client)
}

// classifyYouTubeSetCookies sorts a youtube.com reply's Set-Cookie headers
// into the renewals that may be kept and the names of the cookies the reply
// cleared, auth cookies apart from the rest. A header admitSetCookie refuses,
// and a name the jar does not track, is in none of them.
func classifyYouTubeSetCookies(setCookies []string) (updates map[cookieUpdateKey]cookieUpdate, clearedAuth, clearedOther []string) {
	updates = make(map[cookieUpdateKey]cookieUpdate)
	for _, sc := range setCookies {
		key, cu, ok := admitSetCookie(sc, originYouTube)
		if !ok || !renewableCookie(key, cu) {
			continue
		}
		if cu.Delete || cu.Value == "" {
			into := &clearedOther
			if slices.Contains(youtubeAuthCookieNames, key.Name) {
				into = &clearedAuth
			}
			if !slices.Contains(*into, key.Name) {
				*into = append(*into, key.Name)
			}
			continue
		}
		updates[key] = cu
	}
	sort.Strings(clearedAuth)
	sort.Strings(clearedOther)
	return updates, clearedAuth, clearedOther
}

// renewableCookie reports whether a Set-Cookie admitSetCookie let through is
// one the jar keeps for YouTube. An unscoped header already passed
// admitSetCookie's own name and scope rule; a scoped one is held to the same
// names here, with the youtube-only names (SOCS) kept to a domain
// www.youtube.com is sent.
//
// Since owner ruling KA-7 a scoped header is also kept under any other name
// when its domain is one www.youtube.com is sent and it states Path=/
// (wwwYouTubeRow, the rule Load keeps the row by once it is written). Plain
// SIDCC, which YouTube renews on every page reply, is kept this way.
func renewableCookie(key cookieUpdateKey, cu cookieUpdate) bool {
	if key.Domain == "" {
		return true
	}
	if youtubeOnlyEssentialCookies[key.Name] {
		return sentToWWWYouTube(key.Domain)
	}
	return essentialYouTubeCookies[key.Name] || isGoogleOnlyAuthName(key.Name) ||
		wwwYouTubeRow(key.Name, key.Domain, cu.Path)
}

// youTubeSessionKeyOfHeader is CookieJar.youTubeSessionKey for a Cookie
// request header: "" when it carries no SAPISID.
func youTubeSessionKeyOfHeader(header string) string {
	if header == "" {
		return ""
	}
	var sapisid, sapisid3p, sapisid1p, loginInfo string
	for pair := range strings.SplitSeq(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		switch name {
		case "SAPISID":
			sapisid = value
		case "__Secure-3PAPISID":
			sapisid3p = value
		case "__Secure-1PAPISID":
			sapisid1p = value
		case "LOGIN_INFO":
			loginInfo = value
		}
	}
	// The order youTubeSessionParts reads them in.
	if sapisid == "" {
		sapisid = sapisid3p
	}
	if sapisid == "" {
		sapisid = sapisid1p
	}
	if sapisid == "" {
		return ""
	}
	return youTubeSessionFingerprint(sapisid, loginInfo)
}

// throttledLocked reports whether the line keyed k may be logged now, and
// stamps it when it may: at most one per renewalWarnEvery. Callers hold r.mu.
func (r *renewalState) throttledLocked(k string, now time.Time) bool {
	if last, ok := r.warned[k]; ok && now.Sub(last) < renewalWarnEvery {
		return false
	}
	if r.warned == nil {
		r.warned = make(map[string]time.Time)
	}
	r.warned[k] = now
	return true
}

// warnClearedLocked logs the auth cookies a reply cleared. Nothing is
// removed from the store. Callers hold r.mu.
func (j *CookieJar) warnClearedLocked(cleared []string, reply YouTubeReply) {
	r := &j.renew
	now := time.Now()
	for _, name := range cleared {
		if !r.throttledLocked(name+"\x00"+reply.Endpoint+"\x00"+reply.Client, now) {
			continue
		}
		r.log().Warn("YouTube cleared an auth cookie on a reply to this copy of the session; the stored value was kept",
			"cookie", name, "endpoint", reply.Endpoint, "client", reply.Client)
	}
}

// noteSignedOutReplyLocked logs, at Info, the names a reply read as signed
// out set and cleared. None of it is kept; the line is the record of what a
// signed-out render does to the cookies, which the next loss is diagnosed
// from. At most one per renewalWarnEvery for an endpoint and client. Callers
// hold r.mu.
func (j *CookieJar) noteSignedOutReplyLocked(reply YouTubeReply) {
	r := &j.renew
	if !r.throttledLocked("\x00signed out\x00"+reply.Endpoint+"\x00"+reply.Client, time.Now()) {
		return
	}
	held := reply.held()
	set := make([]string, 0, len(held.updates))
	for key := range held.updates {
		if !slices.Contains(set, key.Name) {
			set = append(set, key.Name)
		}
	}
	sort.Strings(set)
	cleared := slices.Concat(held.clearedAuth, held.clearedOther)
	sort.Strings(cleared)
	r.log().Info("a reply that read as signed out set session cookies; none of them was kept",
		"endpoint", reply.Endpoint, "client", reply.Client,
		"names_set", strings.Join(set, ","), "names_cleared", strings.Join(cleared, ","))
}

// changedRenewals returns the updates that differ from what the jar holds:
// another value, or the same value with an expiry at least renewalExpirySlack
// later. It reads the jar's memory only, so a reply that re-sends what the
// jar already has costs no file read.
func (j *CookieJar) changedRenewals(updates map[cookieUpdateKey]cookieUpdate) map[cookieUpdateKey]cookieUpdate {
	changed := make(map[cookieUpdateKey]cookieUpdate, len(updates))
	j.mu.RLock()
	defer j.mu.RUnlock()
	for key, cu := range updates {
		have, ok := j.youtube[key.Name]
		same := ok && have.value == cu.Value &&
			(key.Domain == "" || sameCookieScope(key.Domain, have.domain)) &&
			cu.Expiry-have.expiry < int64(renewalExpirySlack/time.Second)
		if !same {
			changed[key] = cu
		}
	}
	return changed
}

// readFileLocked returns cookies.txt and the session it holds, from the last
// read when the file's size and modification time have not moved since. A
// read is trusted for the next call only once the file has been still for
// cookieJarStatSettle, the rule CookieJar.Load memoises by. cached says the
// bytes are the ones the previous call returned. Callers hold r.mu.
func (r *renewalState) readFileLocked(path string) (data []byte, key string, cached bool, err error) {
	pre, preErr := os.Stat(path)
	if preErr == nil && r.file.ok && r.file.path == path &&
		r.file.size == pre.Size() && r.file.mod.Equal(pre.ModTime()) {
		return r.file.data, r.file.key, true, nil
	}
	r.file.ok, r.applied.ok = false, false
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, "", false, err
	}
	key = youTubeSessionKeyOf(data)
	post, postErr := os.Stat(path)
	if preErr == nil && postErr == nil &&
		pre.Size() == post.Size() && pre.ModTime().Equal(post.ModTime()) &&
		time.Since(post.ModTime()) >= cookieJarStatSettle {
		r.file = renewalFile{ok: true, path: path, size: post.Size(), mod: post.ModTime(), data: data, key: key}
	}
	return data, key, false, nil
}

// acceptRenewalsLocked keeps updates: they go into the jar at once and wait
// in pending for the next write of cookies.txt. Callers hold r.mu and have
// already established that the reply is about the jar's session.
func (j *CookieJar) acceptRenewalsLocked(updates map[cookieUpdateKey]cookieUpdate, endpoint, client string) {
	if len(updates) == 0 {
		return
	}
	r := &j.renew
	path := j.GetFilePath()
	if path == "" {
		// A jar with no file has nowhere to keep a renewal.
		return
	}
	// A reply that re-sends what the jar holds renews nothing. While a
	// writer holds the flush the re-send is kept all the same: the writer's
	// merge may be about to put an older value from the profile over the
	// one the jar holds, and only a waiting renewal is laid back over it.
	if r.held == 0 {
		if updates = j.changedRenewals(updates); len(updates) == 0 {
			return
		}
	}

	data, fileKey, cached, err := r.readFileLocked(path)
	if err != nil {
		r.log().Debug("youtube session renewal: cookies.txt could not be read; the renewal was not kept", "err", err)
		return
	}
	if len(r.pending) > 0 && fileKey != r.pendingBase {
		if r.held > 0 {
			// A writer is between its merge and its verdict, and the file
			// holds another session for now. Whether the waiting renewals
			// still have a file to go to is decided when the writer is done
			// (it may roll back); this one is for neither session's file.
			return
		}
		// The file was replaced by another session's since the first waiting
		// renewal arrived. Those renewals belong to the session that is gone.
		r.pending, r.applied.ok = nil, false
	}
	// The file, with what is already waiting, must hold the session the jar
	// answered as. An import that landed while the request was in flight
	// fails here: its file is not given the old session's renewals.
	base, baseKey := string(data), fileKey
	if len(r.pending) > 0 {
		if !cached || !r.applied.ok {
			applied, _, err := rewriteCookieRows(data, discardRenewalLogger{}, r.pending, originYouTube)
			if err != nil {
				return
			}
			r.applied.ok, r.applied.text, r.applied.key = true, applied, youTubeSessionKeyOf([]byte(applied))
		}
		base, baseKey = r.applied.text, r.applied.key
	}
	if baseKey != j.youTubeSessionKey() {
		return
	}

	merged := make(map[cookieUpdateKey]cookieUpdate, len(r.pending)+len(updates))
	maps.Copy(merged, r.pending)
	names := make([]string, 0, len(updates))
	for key, cu := range updates {
		merged[key] = cu
		names = append(names, key.Name)
	}
	next, _, err := rewriteCookieRows(data, discardRenewalLogger{}, merged, originYouTube)
	if err != nil {
		r.log().Debug("youtube session renewal: cookies.txt could not be parsed; the renewal was not kept", "err", err)
		return
	}
	changed := next != base
	if !changed && r.held == 0 {
		// The file with these updates is the file without them: there is
		// nothing to keep, to stamp or to write.
		return
	}
	if len(r.pending) == 0 {
		r.pendingBase = fileKey
	}
	r.pending = merged
	// The jar holds what the next flush will write: the same pending
	// renewals, applied by the same rules, over the same file. The install
	// moves the jar's load generation on, so a Load that read the file
	// before this renewal does not memoise what it installed.
	j.parseRows([]byte(next), path)
	j.mu.Lock()
	j.loadedMemo = false
	j.mu.Unlock()
	r.applied.ok, r.applied.text, r.applied.key = true, next, j.youTubeSessionKey()

	if changed {
		r.lastAccepted = time.Now()
		j.noteEndpointNamesLocked(endpoint, client, names)
	}
	j.scheduleFlushLocked()
}

// noteEndpointNamesLocked logs, at Info, when an endpoint and client renew a
// name they have not renewed before in this process, and adds the names to
// the set the next browser pass reports. Callers hold r.mu.
func (j *CookieJar) noteEndpointNamesLocked(endpoint, client string, names []string) {
	r := &j.renew
	who := endpoint + "\x00" + client
	if r.endpointNames == nil {
		r.endpointNames = make(map[string]map[string]bool)
	}
	if r.sincePass == nil {
		r.sincePass = make(map[string]map[string]bool)
	}
	seen, since := r.endpointNames[who], r.sincePass[who]
	if seen == nil {
		seen = make(map[string]bool)
		r.endpointNames[who] = seen
	}
	if since == nil {
		since = make(map[string]bool)
		r.sincePass[who] = since
	}
	grew := false
	for _, name := range names {
		since[name] = true
		if !seen[name] {
			seen[name] = true
			grew = true
		}
	}
	if !grew {
		return
	}
	r.log().Info("YouTube renewed session cookies on a reply; Moombox keeps them",
		"endpoint", endpoint, "client", client, "names", strings.Join(slices.Sorted(maps.Keys(seen)), ","))
}

// TakeReplyRenewals returns when a reply's renewal was last kept (zero when
// none has been) and the names kept since the previous call, by endpoint and
// client, as one string for a log line: "guide (WEB): A,B; player (TVHTML5):
// C". Empty when nothing was kept. It starts the set over, so the caller (the
// per-pass census line) shows a name that stopped being renewed as missing.
// Names only. Safe on a nil jar.
func (j *CookieJar) TakeReplyRenewals() (last time.Time, names string) {
	if j == nil {
		return time.Time{}, ""
	}
	r := &j.renew
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := make([]string, 0, len(r.sincePass))
	for _, who := range slices.Sorted(maps.Keys(r.sincePass)) {
		endpoint, client, _ := strings.Cut(who, "\x00")
		parts = append(parts, endpoint+" ("+client+"): "+strings.Join(slices.Sorted(maps.Keys(r.sincePass[who])), ","))
	}
	r.sincePass = nil
	return r.lastAccepted, strings.Join(parts, "; ")
}

// overlayRenewalsLocked applies the renewals waiting for a flush over
// cookies.txt bytes just read, so a reload does not put the jar back on the
// values the file still holds. A file that holds another session than the
// one the renewals were kept for drops them instead, except while a writer
// holds the flush: its file may be rolled back to the session they belong
// to, so they wait, unapplied, until it is done. Callers hold r.mu.
func (j *CookieJar) overlayRenewalsLocked(data []byte) []byte {
	r := &j.renew
	if len(r.pending) == 0 {
		return data
	}
	if youTubeSessionKeyOf(data) != r.pendingBase {
		if r.held == 0 {
			r.pending, r.applied.ok = nil, false
		}
		return data
	}
	applied, _, err := rewriteCookieRows(data, discardRenewalLogger{}, r.pending, originYouTube)
	if err != nil {
		return data
	}
	return []byte(applied)
}

// scheduleFlushLocked arms the one flush timer: for now when a minute has
// passed since the last write, and otherwise for the rest of that minute.
// The write always runs on the timer's goroutine. The caller is a request
// that has not read its reply's body yet, or a pass handing the file back,
// and the atomic write can take a second on Windows. Callers hold r.mu.
func (j *CookieJar) scheduleFlushLocked() {
	r := &j.renew
	if r.held > 0 || len(r.pending) == 0 || r.timer != nil {
		return
	}
	wait := time.Duration(0)
	if !r.lastFlush.IsZero() {
		wait = max(renewalFlushEvery-time.Since(r.lastFlush), 0)
	}
	r.timer = time.AfterFunc(wait, func() {
		defer func() {
			if rec := recover(); rec != nil {
				r.mu.Lock()
				logger := r.log()
				r.mu.Unlock()
				logger.Error("youtube session renewal: the cookies.txt flush panicked", "panic", rec)
			}
		}()
		r.mu.Lock()
		defer r.mu.Unlock()
		r.timer = nil
		if r.held == 0 {
			j.flushRenewalsLocked()
		}
	})
}

// flushRenewalsLocked writes the waiting renewals into cookies.txt through
// the one atomic writer. Callers hold r.mu.
func (j *CookieJar) flushRenewalsLocked() {
	r := &j.renew
	if len(r.pending) == 0 {
		return
	}
	r.lastFlush = time.Now()
	err := updateCookieFileAt(j.GetFilePath(), r.log(), r.pending, originYouTube, r.pendingBase)
	switch {
	case err == nil:
		r.log().Debug("youtube session renewal: wrote renewed cookies to cookies.txt", "count", len(r.pending))
		r.pending, r.applied.ok = nil, false
	case errors.Is(err, fs.ErrNotExist):
		// No cookies.txt: the operator deleted it, which is how Moombox is
		// logged out. Nothing is written back.
		r.pending, r.applied.ok = nil, false
	case errors.Is(err, errCookieSessionReplaced):
		r.log().Debug("youtube session renewal: cookies.txt now holds another session; its renewals were not written")
		r.pending, r.applied.ok = nil, false
	default:
		// The renewals stay in the jar and the write is tried again in a
		// minute. Name the deployment mistake that usually causes this: the
		// write ends in a rename, and a rename cannot replace a single-file
		// bind mount. Once an hour at Warn: the retry fails the same way
		// each time.
		j.scheduleFlushLocked()
		if !r.lastFlushWarn.IsZero() && time.Since(r.lastFlushWarn) < renewalWarnEvery {
			r.log().Debug("youtube session renewal: failed to update cookie file again", "err", err)
			return
		}
		r.lastFlushWarn = time.Now()
		r.log().Warn("youtube session renewal: failed to update cookie file — renewed session cookies are in memory only and the file will go stale",
			"err", err,
			"hint", "if this is Docker, do not bind-mount cookies.txt as an individual file; put it inside the mounted /data directory so the atomic rename can replace it")
	}
}

// dropPendingRenewals forgets the renewals waiting to be written.
func (j *CookieJar) dropPendingRenewals() {
	j.renew.mu.Lock()
	j.renew.pending, j.renew.applied.ok = nil, false
	j.renew.mu.Unlock()
}

// FlushRenewals writes any waiting renewals into cookies.txt now, unless a
// writer holds the flush. Shutdown calls it twice, so a renewal kept in the
// last minute is not lost: when the refresh service stops, and again once
// every pass, import and setup has ended and let go of its hold.
func (j *CookieJar) FlushRenewals() {
	if j == nil {
		return
	}
	r := &j.renew
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if r.held == 0 {
		j.flushRenewalsLocked()
	}
}

// HoldRenewalFlush is for a writer that reads cookies.txt, decides, and
// writes it back (a browser pass, an import, a setup): it writes the waiting
// renewals now, so the file the writer reads holds them, and keeps the flush
// from writing the file until release is called. Renewals that arrive in
// between stay in the jar, survive the writer's reload (overlayRenewals) and
// are written after release. release may be called more than once.
//
// This is also how a browser pass does not undo a renewal: a value the
// profile holds wins by name in the pass's merge, as it always has, but a
// name YouTube renewed on a reply after the pass took its hold is applied
// again over the merged file, and over a rolled-back one, for as long as the
// file holds the session the renewal was kept for. A merge that brings
// another session in does not get the renewals and does not lose them
// either: they wait until the hold ends, so a rollback to the first session
// has them, and the flush after the release drops them if the other session
// stayed. Which renewals are waiting is known in memory only; after a restart
// the profile wins.
func (j *CookieJar) HoldRenewalFlush() (release func()) {
	if j == nil {
		return func() {}
	}
	r := &j.renew
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if r.held == 0 {
		j.flushRenewalsLocked()
		// A write that failed armed the timer again; the hold owns the file
		// from here.
		if r.timer != nil {
			r.timer.Stop()
			r.timer = nil
		}
	}
	r.held++
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.held--
			j.scheduleFlushLocked()
		})
	}
}

// lastRenewalTime is LastRenewalAt as a time, zero when no reply has renewed
// anything, for a log line.
func (j *CookieJar) lastRenewalTime() time.Time {
	if t := j.LastRenewalAt(); t != nil {
		return *t
	}
	return time.Time{}
}

// LastRenewalAt is when a YouTube reply last renewed a session cookie the jar
// kept, in this process. nil when none has, and on a nil jar.
func (j *CookieJar) LastRenewalAt() *time.Time {
	if j == nil {
		return nil
	}
	j.renew.mu.Lock()
	defer j.renew.mu.Unlock()
	if j.renew.lastAccepted.IsZero() {
		return nil
	}
	t := j.renew.lastAccepted.UTC()
	return &t
}
