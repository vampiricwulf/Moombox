package cookies

// renewals.go — keeping the session cookies YouTube renews on its replies.
//
// A browser applies every Set-Cookie it is sent and sends the newest value on
// its next request. Moombox used to keep only what the guide check's reply
// carried and replay the copy from the last browser visit on everything else.
// CookieJar.OfferYouTubeReply is the one place a reply's Set-Cookie headers
// reach the cookie store: every request that carries the session cookies to a
// youtube.com host offers its reply here.
//
// What is kept:
//
//   - only a reply from youtube.com or a subdomain of it, over https, with a
//     2xx status, to a request that carried the session the jar holds now;
//   - only a name the jar tracks for YouTube (trackedCookieName), with a
//     value;
//   - never from a reply whose caller read it as signed out.
//
// A deletion or an empty value is never applied. For an auth cookie it is
// logged at Warn, by name, with the endpoint and the client that drew it.
//
// A kept renewal is in the jar at once, so the next request sends it.
// cookies.txt is rewritten at most once a minute while renewals are waiting,
// and once more on shutdown (FlushRenewals). Nothing here is persisted
// anywhere else: the times renewals were received live in memory only.
//
// Only the final reply of a redirect chain is seen. A Set-Cookie on a
// redirect hop is not.
//
// No cookie value is ever logged: names, counts and endpoints only.

import (
	"errors"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// renewalFlushEvery is the least time between two writes of cookies.txt for
// renewals.
const renewalFlushEvery = time.Minute

// renewalWarnEvery is the least time between two Warn lines for the same
// cleared auth cookie on the same endpoint and client. A dead session draws
// the same deletion on every request, about a thousand a day.
const renewalWarnEvery = time.Hour

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
// It holds the reply's raw Set-Cookie headers, which are credentials. Keep it
// on the stack between the fetch and the offer, and never log it.
type YouTubeReply struct {
	// Endpoint names the request for the log: "guide", "membership page".
	Endpoint string
	// Client is the client identity the request was made as: an Innertube
	// client name, or "web page" for an HTML page.
	Client string
	// Verdict is what the caller read off the reply about the session.
	Verdict ReplyVerdict

	scheme     string
	host       string
	status     int
	sentCookie string
	setCookies []string
}

// ReplyOf captures what OfferYouTubeReply reads from resp: where the final
// reply came from, its status, the Cookie header the answering request
// carried and the reply's Set-Cookie headers. A reply with no Set-Cookie
// header gives the zero value, which OfferYouTubeReply ignores.
func ReplyOf(resp *http.Response) YouTubeReply {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return YouTubeReply{}
	}
	setCookies := resp.Header.Values("Set-Cookie")
	if len(setCookies) == 0 {
		return YouTubeReply{}
	}
	return YouTubeReply{
		scheme:     resp.Request.URL.Scheme,
		host:       resp.Request.URL.Hostname(),
		status:     resp.StatusCode,
		sentCookie: resp.Request.Header.Get("Cookie"),
		setCookies: slices.Clone(setCookies),
	}
}

// From returns r with its endpoint, client and verdict set.
func (r YouTubeReply) From(endpoint, client string, verdict ReplyVerdict) YouTubeReply {
	r.Endpoint, r.Client, r.Verdict = endpoint, client, verdict
	return r
}

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
	// hostOK replaces the youtube.com host rule in tests. nil in production.
	hostOK func(scheme, host string) bool

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

	// endpointNames is the set of names each endpoint has renewed so far.
	// It only grows, so two alternating replies cannot make the Info line
	// repeat.
	endpointNames map[string]map[string]bool
	// warned is when each cleared-auth-cookie Warn was last logged.
	warned map[string]time.Time
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

// SetReplyHostCheckForTesting replaces the rule that a reply must come from
// youtube.com over https, so a test can offer replies from an httptest
// server. Production code never calls it.
func (j *CookieJar) SetReplyHostCheckForTesting(ok func(scheme, host string) bool) {
	j.renew.mu.Lock()
	defer j.renew.mu.Unlock()
	j.renew.hostOK = ok
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

// replyHostRule is the host rule in force: isYouTubeReplyHost, except in
// this package's tests, which point the guide check at an httptest server.
var replyHostRule = isYouTubeReplyHost

// isYouTubeReplyHost is the host rule: https, and youtube.com or a subdomain
// of it. Never googlevideo.com, never google.com.
func isYouTubeReplyHost(scheme, host string) bool {
	return strings.EqualFold(scheme, "https") && isYouTubeDomain(host)
}

// OfferYouTubeReply offers one reply's Set-Cookie headers to the cookie
// store. See the file comment for what is kept. It is safe to call with the
// zero value, and safe on a nil jar.
func (j *CookieJar) OfferYouTubeReply(reply YouTubeReply) {
	if j == nil || len(reply.setCookies) == 0 {
		return
	}
	r := &j.renew
	r.mu.Lock()
	defer r.mu.Unlock()

	hostOK := replyHostRule
	if r.hostOK != nil {
		hostOK = r.hostOK
	}
	if !hostOK(reply.scheme, reply.host) {
		return
	}
	// The reply is about the jar's session only when the request that was
	// answered carried it. A request with the consent cookies alone, one a
	// redirect stripped, and one sent before an import replaced the session
	// all fail here.
	sentAs := youTubeSessionKeyOfHeader(reply.sentCookie)
	if sentAs == "" || sentAs != j.youTubeSessionKey() {
		return
	}

	updates, cleared := classifyYouTubeSetCookies(reply.setCookies)
	j.warnClearedLocked(cleared, reply)
	if reply.status < 200 || reply.status > 299 || reply.Verdict == ReplySignedOut {
		return
	}
	j.acceptRenewalsLocked(updates, reply.Endpoint, reply.Client)
}

// classifyYouTubeSetCookies sorts a youtube.com reply's Set-Cookie headers
// into the renewals that may be kept and the auth cookies the reply cleared.
// A header admitSetCookie refuses, and a name the jar does not track, is in
// neither.
func classifyYouTubeSetCookies(setCookies []string) (updates map[cookieUpdateKey]cookieUpdate, cleared []string) {
	updates = make(map[cookieUpdateKey]cookieUpdate)
	for _, sc := range setCookies {
		key, cu, ok := admitSetCookie(sc, originYouTube)
		if !ok || !renewableCookie(key) {
			continue
		}
		if cu.Delete || cu.Value == "" {
			if slices.Contains(youtubeAuthCookieNames, key.Name) && !slices.Contains(cleared, key.Name) {
				cleared = append(cleared, key.Name)
			}
			continue
		}
		updates[key] = cu
	}
	sort.Strings(cleared)
	return updates, cleared
}

// renewableCookie reports whether a Set-Cookie admitSetCookie let through is
// one the jar tracks for YouTube. An unscoped header already passed
// trackedCookieName; a scoped one is held to the same names here, with the
// youtube-only names (SOCS) kept to a domain www.youtube.com is sent.
func renewableCookie(key cookieUpdateKey) bool {
	if key.Domain == "" {
		return true
	}
	if youtubeOnlyEssentialCookies[key.Name] {
		return sentToWWWYouTube(key.Domain)
	}
	return essentialYouTubeCookies[key.Name] || isGoogleOnlyAuthName(key.Name)
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

// warnClearedLocked logs the auth cookies a reply cleared. Nothing is
// removed from the store. Callers hold r.mu.
func (j *CookieJar) warnClearedLocked(cleared []string, reply YouTubeReply) {
	r := &j.renew
	now := time.Now()
	for _, name := range cleared {
		k := name + "\x00" + reply.Endpoint + "\x00" + reply.Client
		if last, ok := r.warned[k]; ok && now.Sub(last) < renewalWarnEvery {
			continue
		}
		if r.warned == nil {
			r.warned = make(map[string]time.Time)
		}
		r.warned[k] = now
		r.log().Warn("YouTube cleared an auth cookie on a reply to this copy of the session; the stored value was kept",
			"cookie", name, "endpoint", reply.Endpoint, "client", reply.Client)
	}
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
	data, err := os.ReadFile(path)
	if err != nil {
		r.log().Debug("youtube session renewal: cookies.txt could not be read; the renewal was not kept", "err", err)
		return
	}
	fileKey := youTubeSessionKeyOf(data)
	if len(r.pending) > 0 && fileKey != r.pendingBase {
		// The file was replaced by another session's since the first waiting
		// renewal arrived. Those renewals belong to the session that is gone.
		r.pending = nil
	}
	// The file must hold the session the jar answered as. An import that
	// landed while the request was in flight fails here: its file is not
	// given the old session's renewals.
	base := data
	if len(r.pending) > 0 {
		applied, _, err := rewriteCookieRows(data, discardRenewalLogger{}, r.pending, originYouTube)
		if err != nil {
			return
		}
		base = []byte(applied)
	}
	if youTubeSessionKeyOf(base) != j.youTubeSessionKey() {
		return
	}

	if len(r.pending) == 0 {
		r.pending = make(map[cookieUpdateKey]cookieUpdate, len(updates))
		r.pendingBase = fileKey
	}
	names := make([]string, 0, len(updates))
	for key, cu := range updates {
		r.pending[key] = cu
		names = append(names, key.Name)
	}
	next, _, err := rewriteCookieRows(data, discardRenewalLogger{}, r.pending, originYouTube)
	if err != nil {
		r.log().Debug("youtube session renewal: cookies.txt could not be parsed; the renewal was not kept", "err", err)
		return
	}
	// The jar holds what the next flush will write: the same pending
	// renewals, applied by the same rules, over the same file. The install
	// moves the jar's load generation on, so a Load that read the file
	// before this renewal does not memoise what it installed.
	j.parseRows([]byte(next), path)
	j.mu.Lock()
	j.loadedMemo = false
	j.mu.Unlock()

	j.noteEndpointNamesLocked(endpoint, client, names)
	j.scheduleFlushLocked()
}

// noteEndpointNamesLocked logs, at Info, when an endpoint renews a name it
// has not renewed before in this process. Callers hold r.mu.
func (j *CookieJar) noteEndpointNamesLocked(endpoint, client string, names []string) {
	r := &j.renew
	if r.endpointNames == nil {
		r.endpointNames = make(map[string]map[string]bool)
	}
	seen := r.endpointNames[endpoint]
	if seen == nil {
		seen = make(map[string]bool)
		r.endpointNames[endpoint] = seen
	}
	grew := false
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			grew = true
		}
	}
	if !grew {
		return
	}
	all := make([]string, 0, len(seen))
	for name := range seen {
		all = append(all, name)
	}
	sort.Strings(all)
	r.log().Info("YouTube renewed session cookies on a reply; Moombox keeps them",
		"endpoint", endpoint, "client", client, "names", strings.Join(all, ","))
}

// overlayRenewalsLocked applies the renewals waiting for a flush over
// cookies.txt bytes just read, so a reload does not put the jar back on the
// values the file still holds. A file that holds another session than the
// one the renewals were kept for drops them instead. Callers hold r.mu.
func (j *CookieJar) overlayRenewalsLocked(data []byte) []byte {
	r := &j.renew
	if len(r.pending) == 0 {
		return data
	}
	if youTubeSessionKeyOf(data) != r.pendingBase {
		r.pending = nil
		return data
	}
	applied, _, err := rewriteCookieRows(data, discardRenewalLogger{}, r.pending, originYouTube)
	if err != nil {
		return data
	}
	return []byte(applied)
}

// scheduleFlushLocked writes cookies.txt now when a minute has passed since
// the last write, and otherwise arms one timer for the rest of that minute.
// Callers hold r.mu.
func (j *CookieJar) scheduleFlushLocked() {
	r := &j.renew
	if r.held > 0 || len(r.pending) == 0 {
		return
	}
	wait := renewalFlushEvery - time.Since(r.lastFlush)
	if r.lastFlush.IsZero() || wait <= 0 {
		j.flushRenewalsLocked()
		return
	}
	if r.timer != nil {
		return
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
		r.pending = nil
	case errors.Is(err, errCookieSessionReplaced):
		r.log().Debug("youtube session renewal: cookies.txt now holds another session; its renewals were not written")
		r.pending = nil
	default:
		// The renewals stay in the jar and are tried again with the next
		// one. Name the deployment mistake that usually causes this: the
		// write ends in a rename, and a rename cannot replace a single-file
		// bind mount.
		r.log().Warn("youtube session renewal: failed to update cookie file — renewed session cookies are in memory only and the file will go stale",
			"err", err,
			"hint", "if this is Docker, do not bind-mount cookies.txt as an individual file; put it inside the mounted /data directory so the atomic rename can replace it")
	}
}

// FlushRenewals writes any waiting renewals into cookies.txt now. Shutdown
// calls it, so a renewal kept in the last minute is not lost.
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
// file still holds the same session. Which renewals are waiting is known in
// memory only; after a restart the profile wins.
func (j *CookieJar) HoldRenewalFlush() (release func()) {
	r := &j.renew
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if r.held == 0 {
		j.flushRenewalsLocked()
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
