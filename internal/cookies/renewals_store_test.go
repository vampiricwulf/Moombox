package cookies

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// A reply that re-sends the value the jar already holds renews nothing: no
// waiting renewal, no renewal time, no write of cookies.txt. The same value
// with an expiry a day or more later is a renewal of its lifetime and is
// kept.
//
// Mutation: drop the changedRenewals filter (the first half fails on all
// three).
func TestResentUnchangedCookieIsNotARenewal(t *testing.T) {
	jar, path, log := renewalJar(t)
	offer := func(expiry int64) {
		jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200,
			fmt.Sprintf("__Secure-1PSIDCC=sidcc-old; Domain=.youtube.com; Path=/; Expires=%s",
				time.Unix(expiry, 0).UTC().Format(http.TimeFormat))))
	}
	offer(2000000000 + 3600) // the seed's expiry, an hour on

	jar.renew.mu.Lock()
	waiting, armed := len(jar.renew.pending), jar.renew.timer != nil
	jar.renew.mu.Unlock()
	if waiting != 0 || armed {
		t.Errorf("an unchanged cookie left %d renewals waiting (flush armed: %v), want none", waiting, armed)
	}
	if at := jar.LastRenewalAt(); at != nil {
		t.Errorf("an unchanged cookie was stamped as a renewal at %v", at)
	}
	if lines := log.containing("INFO", "renewed session cookies"); len(lines) != 0 {
		t.Errorf("an unchanged cookie was logged as renewed: %v", lines)
	}

	offer(2000000000 + int64(2*renewalExpirySlack/time.Second))
	jar.FlushRenewals()
	if jar.LastRenewalAt() == nil {
		t.Errorf("a cookie re-sent with an expiry two days later was not kept")
	}
	if got := fileText(t, path); strings.Contains(got, "\t2000000000\t__Secure-1PSIDCC\t") {
		t.Errorf("cookies.txt still holds the old expiry:\n%s", got)
	}
}

// sessionBSeed is renewalSeed after a merge brought another LOGIN_INFO in.
var sessionBSeed = strings.NewReplacer("login-A", "login-B", "sidcc-old", "sidcc-B").Replace(renewalSeed)

// While a writer holds the flush and its file holds another session, the
// waiting renewals are neither applied nor dropped: a rollback to their
// session gets them back.
//
// Mutation: drop the `r.held == 0` guard in overlayRenewalsLocked (the jar
// and the file lose the renewal after the rollback).
func TestHeldRenewalSurvivesAnotherSessionsFile(t *testing.T) {
	jar, path, _ := renewalJar(t)
	release := jar.HoldRenewalFlush()
	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200, sidccRenewal))

	// The writer's merge brings another session in.
	if err := os.WriteFile(path, []byte(sessionBSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "sidcc-B" {
		t.Errorf("jar __Secure-1PSIDCC = %q: session A's renewal was laid over session B", got)
	}
	// A reply to session B while the writer still holds: kept for neither.
	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200,
		"__Secure-1PSIDCC=sidcc-B-renewed; Domain=.youtube.com; Path=/; Max-Age=31536000"))

	// The writer rolls back.
	if err := os.WriteFile(path, []byte(renewalSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	if got := jar.GetCookieFor(PlatformYouTube, "__Secure-1PSIDCC"); got != "sidcc-new" {
		t.Errorf("jar __Secure-1PSIDCC = %q after the rollback, want the renewal kept during the hold", got)
	}
	release()
	jar.FlushRenewals()
	if got := fileText(t, path); !strings.Contains(got, "\t__Secure-1PSIDCC\tsidcc-new") || strings.Contains(got, "sidcc-B") {
		t.Errorf("cookies.txt after the rollback and release:\n%s", got)
	}
}

// When the other session stays, the renewals kept for the first one are
// dropped at the flush after the release and never written into it.
func TestHeldRenewalIsDroppedWhenTheOtherSessionStays(t *testing.T) {
	jar, path, _ := renewalJar(t)
	release := jar.HoldRenewalFlush()
	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200, sidccRenewal))
	if err := os.WriteFile(path, []byte(sessionBSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}
	release()
	jar.FlushRenewals()
	if got := fileText(t, path); got != sessionBSeed {
		t.Errorf("session A's renewal was written into session B's file:\n%s", got)
	}
	jar.renew.mu.Lock()
	waiting := len(jar.renew.pending)
	jar.renew.mu.Unlock()
	if waiting != 0 {
		t.Errorf("%d renewals of the replaced session are still waiting", waiting)
	}
}

// A write of cookies.txt that fails is tried again a minute later without
// another renewal having to arrive.
//
// Mutation: drop the scheduleFlushLocked call from the failure branch.
func TestFailedFlushIsScheduledAgain(t *testing.T) {
	jar, path, _ := renewalJar(t)
	release := jar.HoldRenewalFlush() // so nothing is written before the file is broken
	jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200, sidccRenewal))
	// cookies.txt becomes a directory: unreadable, but not absent.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	release()
	jar.FlushRenewals()

	jar.renew.mu.Lock()
	waiting, armed := len(jar.renew.pending), jar.renew.timer != nil
	if jar.renew.timer != nil {
		jar.renew.timer.Stop()
	}
	jar.renew.mu.Unlock()
	if waiting == 0 || !armed {
		t.Errorf("after a failed write: %d renewals waiting, retry armed = %v; want the renewal kept and the retry armed", waiting, armed)
	}
}

// The names kept since the last browser pass are reported by endpoint and
// client, and the set starts over each time it is taken.
func TestTakeReplyRenewals(t *testing.T) {
	jar, _, _ := renewalJar(t)
	if at, names := jar.TakeReplyRenewals(); !at.IsZero() || names != "" {
		t.Fatalf("before any renewal: %v %q", at, names)
	}
	offer := func(endpoint, client, header string) {
		jar.OfferYouTubeReply(replyTo(t, "https://www.youtube.com/x", jar.GetCookieHeader(), 200, header).From(endpoint, client, ReplyNoVerdict))
	}
	offer("player", "TVHTML5", sidccRenewal)
	offer("player", "WEB_EMBEDDED_PLAYER", "SID=sid-renewed; Domain=.youtube.com; Path=/; Max-Age=31536000")
	offer("guide", "WEB", "__Secure-1PSIDCC=sidcc-newer; Domain=.youtube.com; Path=/; Max-Age=31536000")

	at, names := jar.TakeReplyRenewals()
	const want = "guide (WEB): __Secure-1PSIDCC; player (TVHTML5): __Secure-1PSIDCC; player (WEB_EMBEDDED_PLAYER): SID"
	if at.IsZero() || names != want {
		t.Errorf("TakeReplyRenewals = %v, %q\nwant a time and %q", at, names, want)
	}
	if _, again := jar.TakeReplyRenewals(); again != "" {
		t.Errorf("the set did not start over: %q", again)
	}
}
