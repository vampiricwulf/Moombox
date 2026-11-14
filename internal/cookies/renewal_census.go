package cookies

// renewal_census.go — the renewal census: which session cookies a browser
// pass renewed, by name (owner ruling K5, 2026-11-13).
//
// Until this existed the only gauge of a pass was the LOGIN_INFO-pinned
// horizon, which cannot say whether a visit re-stamped the SID family, the
// SIDCC cookies or the PSIDTS pair. The census reads each row's write stamp
// out of the profile where the cookie store keeps one and sorts the names of
// each group into renewed (written at or after the pass started), unchanged
// and unknown (no stamp). It needs no earlier sample, so a restart loses
// nothing.
//
// Only Firefox's moz_cookies.updateTime is a write stamp. creationTime is
// kept when a cookie is overwritten and lastAccessed moves when a cookie is
// merely sent, so neither is used. A cookie store without the column, and a
// read that does not go through moz_cookies at all (Chromium over CDP, the
// DPAPI fallback), report every name as unknown, never as unchanged.
//
// Nothing here ever holds a cookie value: names, groups, write times and
// expiries only. Nothing is persisted.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// CensusGroup names one group of session cookies the census reports.
type CensusGroup string

const (
	// CensusSID is the SID family on youtube.com: SID, HSID, SSID, APISID,
	// SAPISID and their __Secure- forms.
	CensusSID CensusGroup = "sid"
	// CensusSIDCC is SIDCC and the __Secure-*PSIDCC pair on youtube.com.
	CensusSIDCC CensusGroup = "sidcc"
	// CensusPSIDTS is the __Secure-*PSIDTS pair on youtube.com.
	CensusPSIDTS CensusGroup = "psidts"
	// CensusLoginInfo is LOGIN_INFO on youtube.com.
	CensusLoginInfo CensusGroup = "login_info"
	// CensusGoogleAccount is the Google sign-in's own rows on google.com
	// hosts (accounts.google.com included). They are never copied to
	// cookies.txt; the census reports their names and times only.
	CensusGoogleAccount CensusGroup = "google_account"
	// CensusTwitch is Twitch's auth-token.
	CensusTwitch CensusGroup = "twitch"
)

// censusGroupOrder is the order groups are reported in.
var censusGroupOrder = []CensusGroup{
	CensusSID, CensusSIDCC, CensusPSIDTS, CensusLoginInfo, CensusGoogleAccount, CensusTwitch,
}

// googleAccountOnlyNames are the Google sign-in rows that exist on google.com
// hosts only, beside the SID family google.com also holds.
var googleAccountOnlyNames = map[string]bool{
	"LSID": true, "__Host-1PLSID": true, "__Host-3PLSID": true,
	"__Host-GAPS": true, "ACCOUNT_CHOOSER": true,
}

// censusGroupOf sorts one cookie row into its group by name and host. ok is
// false for a row the census does not report.
func censusGroupOf(name, host string) (group CensusGroup, ok bool) {
	switch {
	case isTwitchDomain(host):
		if name == "auth-token" {
			return CensusTwitch, true
		}
	case isGoogleDomain(host):
		if isGoogleOnlyAuthName(name) || googleAccountOnlyNames[name] {
			return CensusGoogleAccount, true
		}
	case isYouTubeDomain(host):
		switch {
		case name == "LOGIN_INFO":
			return CensusLoginInfo, true
		case name == "SIDCC" || strings.HasSuffix(name, "PSIDCC"):
			return CensusSIDCC, true
		case strings.HasSuffix(name, "PSIDTS"):
			return CensusPSIDTS, true
		case isGoogleOnlyAuthName(name):
			return CensusSID, true
		}
	}
	return "", false
}

// cookieStamp is what the census keeps of one cookie row. There is no value
// field on purpose: a census cannot leak what it never held.
type cookieStamp struct {
	group CensusGroup
	name  string
	// updated is when the browser last wrote the row. Zero when the cookie
	// store keeps no write stamp, or keeps none for this row.
	updated time.Time
	// expires is the row's expiry. Zero for a session cookie.
	expires time.Time
}

// firefoxStampTime reads a moz_cookies updateTime. Firefox writes it in
// microseconds; the unit is read off the magnitude so a store that kept
// milliseconds or seconds is not read as the year 1970 or 50000. Zero and
// negative mean no stamp.
func firefoxStampTime(v int64) time.Time {
	switch {
	case v <= 0:
		return time.Time{}
	case v >= 1e14:
		return time.UnixMicro(v)
	case v >= 1e11:
		return time.UnixMilli(v)
	default:
		return time.Unix(v, 0)
	}
}

// stampsOfNetscape lists the census rows of a Netscape cookie blob, for a
// read that has no write stamps to give. Every row comes back with a zero
// updated time.
func stampsOfNetscape(netscape string) []cookieStamp {
	var out []cookieStamp
	for line := range strings.SplitSeq(netscape, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#HttpOnly_"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 6 {
			continue
		}
		group, ok := censusGroupOf(fields[5], fields[0])
		if !ok {
			continue
		}
		st := cookieStamp{group: group, name: fields[5]}
		if secs := parseInt64OrZero(fields[4]); secs > 0 {
			st.expires = time.Unix(secs, 0)
		}
		out = append(out, st)
	}
	return out
}

// parseInt64OrZero reads a decimal Netscape expiry; anything else is 0.
func parseInt64OrZero(s string) int64 {
	var n int64
	if s == "" {
		return 0
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
		if n < 0 {
			return 0
		}
	}
	return n
}

// CensusGroupResult is one group's part of a census. Names only.
type CensusGroupResult struct {
	Group CensusGroup `json:"group"`
	// Renewed are the names written at or after the pass started.
	Renewed []string `json:"renewed"`
	// Unchanged are the names last written before the pass started.
	Unchanged []string `json:"unchanged"`
	// Unknown are the names with no write stamp to judge by.
	Unknown []string `json:"unknown"`
	// RenewedAt is the newest write stamp in the group: when the browser
	// last renewed any of it. Absent when no row of the group has a stamp.
	RenewedAt *time.Time `json:"renewedAt,omitempty"`
	// Expires is the earliest expiry in the group. Absent when the group
	// holds session cookies only.
	Expires *time.Time `json:"expires,omitempty"`
}

// RenewalCensus is what one browser pass's profile read says was renewed.
type RenewalCensus struct {
	// At is when the pass started; a row written at or after it is renewed.
	At time.Time `json:"at"`
	// Stamped is false when the read had no write stamp for any row, so
	// every name is unknown.
	Stamped bool                `json:"stamped"`
	Groups  []CensusGroupResult `json:"groups"`
}

// censusOf sorts stamps into a census for a pass that started at passStart.
// A name that appears on more than one row of a group (google.com and
// accounts.google.com) is listed once, by its newest stamp. Groups with no
// row are left out.
func censusOf(stamps []cookieStamp, passStart time.Time) RenewalCensus {
	passStart = passStart.UTC()
	census := RenewalCensus{At: passStart, Groups: []CensusGroupResult{}}

	type nameFacts struct {
		updated time.Time
		expires time.Time
	}
	byGroup := make(map[CensusGroup]map[string]nameFacts)
	for _, st := range stamps {
		names := byGroup[st.group]
		if names == nil {
			names = make(map[string]nameFacts)
			byGroup[st.group] = names
		}
		f, seen := names[st.name]
		if !seen || st.updated.After(f.updated) {
			f.updated = st.updated
		}
		if !st.expires.IsZero() && (f.expires.IsZero() || st.expires.Before(f.expires)) {
			f.expires = st.expires
		}
		names[st.name] = f
	}

	for _, group := range censusGroupOrder {
		names := byGroup[group]
		if len(names) == 0 {
			continue
		}
		res := CensusGroupResult{Group: group, Renewed: []string{}, Unchanged: []string{}, Unknown: []string{}}
		var newest, earliestExpiry time.Time
		for name, f := range names {
			switch {
			case f.updated.IsZero():
				res.Unknown = append(res.Unknown, name)
			case !f.updated.Before(passStart):
				res.Renewed = append(res.Renewed, name)
			default:
				res.Unchanged = append(res.Unchanged, name)
			}
			if f.updated.After(newest) {
				newest = f.updated
			}
			if !f.expires.IsZero() && (earliestExpiry.IsZero() || f.expires.Before(earliestExpiry)) {
				earliestExpiry = f.expires
			}
		}
		sort.Strings(res.Renewed)
		sort.Strings(res.Unchanged)
		sort.Strings(res.Unknown)
		if !newest.IsZero() {
			census.Stamped = true
			t := newest.UTC()
			res.RenewedAt = &t
		}
		if !earliestExpiry.IsZero() {
			t := earliestExpiry.UTC()
			res.Expires = &t
		}
		census.Groups = append(census.Groups, res)
	}
	return census
}

// oldestGroup returns the group the browser has gone longest without
// renewing, among those with a write stamp. ok is false when none has one.
func (c RenewalCensus) oldestGroup() (CensusGroupResult, bool) {
	var (
		oldest CensusGroupResult
		found  bool
	)
	for _, g := range c.Groups {
		if g.RenewedAt == nil {
			continue
		}
		if !found || g.RenewedAt.Before(*oldest.RenewedAt) {
			oldest, found = g, true
		}
	}
	return oldest, found
}

// logFields are the census as one log line's fields: names by group, and the
// age and expiry of the group renewed longest ago. No values exist to log.
func (c RenewalCensus) logFields(now time.Time) []any {
	list := func(pick func(CensusGroupResult) []string) string {
		var parts []string
		for _, g := range c.Groups {
			if names := pick(g); len(names) > 0 {
				parts = append(parts, string(g.Group)+"["+strings.Join(names, ",")+"]")
			}
		}
		if len(parts) == 0 {
			return "none"
		}
		return strings.Join(parts, " ")
	}
	fields := []any{
		"renewed", list(func(g CensusGroupResult) []string { return g.Renewed }),
		"unchanged", list(func(g CensusGroupResult) []string { return g.Unchanged }),
		"unknown", list(func(g CensusGroupResult) []string { return g.Unknown }),
	}
	if g, ok := c.oldestGroup(); ok {
		fields = append(fields,
			"oldest_group", string(g.Group),
			"oldest_age", now.Sub(*g.RenewedAt).Round(time.Minute).String())
		if g.Expires != nil {
			fields = append(fields, "oldest_expires", g.Expires.Format(time.RFC3339))
		}
	}
	return fields
}

// passCensus is the profile read of the pass in flight, kept between the read
// and the census.
type passCensus struct {
	set    bool
	stamps []cookieStamp
}

// keepPassStamps hands a moz_cookies read's stamps to the pass that made it.
func (s *AutoCookieService) keepPassStamps(stats firefoxReadStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.passRead = passCensus{set: true, stamps: stats.stamps}
}

// takePassStamps returns the stamps the pass's read kept, and forgets them.
func (s *AutoCookieService) takePassStamps() (stamps []cookieStamp, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	read := s.passRead
	s.passRead = passCensus{}
	return read.stamps, read.set
}

// recordCensus works out the census of a pass that started at passStart and
// read netscape out of the browser, keeps it for the status and logs it: one
// Info line per pass. A read that kept no stamps (Chromium, the DPAPI
// fallback) is judged from the names it returned, all unknown.
func (s *AutoCookieService) recordCensus(passStart time.Time, netscape string) {
	stamps, ok := s.takePassStamps()
	if !ok {
		stamps = stampsOfNetscape(netscape)
	}
	census := censusOf(stamps, passStart)
	if len(census.Groups) == 0 {
		return
	}
	s.mu.Lock()
	s.lastCensus = &census
	s.mu.Unlock()

	msg := "cookie renewal census"
	if !census.Stamped {
		msg = "cookie renewal census — this browser's cookie store records no write time, so what the pass renewed is unknown"
	}
	s.logger.Info(msg, census.logFields(time.Now())...)
}

// nextVisitDue is when the daily visit next runs, from the schedule the loop
// last published: the later of the start hold, 24 h (plus jitter) after the
// last renewing pass, and a running retry wait. A time in the past means the
// next check runs it. ok is false when no loop is running. A last pass
// stamped in the future counts as never, as it does for dailyVisitOverdue.
func nextVisitDue(now time.Time, last *time.Time, v dailyVisit) (due time.Time, ok bool) {
	if v.started.IsZero() {
		return time.Time{}, false
	}
	due = v.started.Add(dailyVisitStartHold)
	if last != nil && !last.After(now) {
		if t := last.Add(dailyVisitInterval + v.jitter); t.After(due) {
			due = t
		}
	}
	if v.retryAt.After(due) {
		due = v.retryAt
	}
	return due, true
}

// censusGroupLabel is the name a group is shown under.
func censusGroupLabel(g CensusGroup) string {
	switch g {
	case CensusSID:
		return "SID family"
	case CensusSIDCC:
		return "SIDCC"
	case CensusPSIDTS:
		return "PSIDTS pair"
	case CensusLoginInfo:
		return "LOGIN_INFO"
	case CensusGoogleAccount:
		return "Google account"
	case CensusTwitch:
		return "Twitch auth-token"
	}
	return string(g)
}

// renewalSpan words a length of time for RenewalLine: whole minutes under an
// hour, whole hours under two days, whole days from there.
func renewalSpan(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	default:
		return fmt.Sprintf("%d d", int(d/(24*time.Hour)))
	}
}

// RenewalLine is the one line both UIs show for the renewal facts of a
// status: when a YouTube reply last renewed a cookie, what the last browser
// pass renewed, and when the next daily visit is due. Empty when the status
// carries none of them. The dashboard's renewalLine
// (web/public/modules/utils.js) is its twin, and the two are compared over
// web/tests/fixtures/renewal-line-parity.json.
func RenewalLine(status AutoCookieStatus, now time.Time) string {
	ago := func(t time.Time) string { return renewalSpan(max(now.Sub(t), 0)) + " ago" }
	var parts []string
	if status.ReplyRenewedAt != nil {
		parts = append(parts, "YouTube replies renewed cookies "+ago(*status.ReplyRenewedAt))
	}
	if c := status.RenewalCensus; c != nil {
		part := "browser pass " + ago(c.At)
		if !c.Stamped {
			part += ", what it renewed is unknown"
		} else {
			var renewed []string
			for _, g := range c.Groups {
				if len(g.Renewed) > 0 {
					renewed = append(renewed, censusGroupLabel(g.Group))
				}
			}
			if len(renewed) == 0 {
				part += " renewed nothing"
			} else {
				part += " renewed " + strings.Join(renewed, ", ")
			}
			if g, ok := c.oldestGroup(); ok && g.RenewedAt.Before(c.At) {
				part += " (oldest: " + censusGroupLabel(g.Group) + ", " + ago(*g.RenewedAt) + ")"
			}
		}
		parts = append(parts, part)
	}
	if status.NextVisitDue != nil {
		if wait := status.NextVisitDue.Sub(now); wait > 0 {
			parts = append(parts, "next daily visit in "+renewalSpan(wait))
		} else {
			parts = append(parts, "daily visit due")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Renewal: " + strings.Join(parts, "; ")
}
