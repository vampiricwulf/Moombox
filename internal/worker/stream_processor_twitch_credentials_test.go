package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/cookies"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// These tests pin the wiring end of the Twitch IRC handshake's credential
// pair. internal/twitch owns the rule that PASS and NICK agree; this file owns
// the rule that the worker never hands the downloader half a pair.
//
// Every credential below is synthetic and none is ever logged.

// writeTwitchSessionCookies writes a Netscape cookie file holding one synthetic
// Twitch session: the auth-token and the login it belongs to.
func writeTwitchSessionCookies(t *testing.T, path, token, login string) {
	t.Helper()
	rows := []string{
		strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "auth-token", token}, "\t"),
		strings.Join([]string{".twitch.tv", "TRUE", "/", "TRUE", "0", "login", login}, "\t"),
	}
	content := "# Netscape HTTP Cookie File\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTwitchChatCredentialsReturnsBothHalves is the claim: a configured Auth
// yields ONE getter that produces BOTH halves, each read from its own cookie.
//
// Asserting only that the getter is non-nil is the failure mode. A wiring that
// supplied a token and no login produces `PASS oauth:<token>` beside the
// anonymous `justinfan<random>` nickname, which Twitch refuses or silently
// downgrades — so both values are pulled and both are checked.
func TestTwitchChatCredentialsReturnsBothHalves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchSessionCookies(t, path, "synthetic-token", "syntheticaccount")

	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	credentials := twitchChatCredentials(twitch.NewAuth(jar, nopWorkerLogger{}))
	if credentials == nil {
		t.Fatal("credential getter is nil for a configured Auth")
	}
	token, login := credentials()
	if token != "synthetic-token" {
		t.Errorf("token = %q, want %q", token, "synthetic-token")
	}
	if login != "syntheticaccount" {
		t.Errorf("login = %q, want %q — the IRC handshake would send the anonymous justinfan "+
			"nickname alongside a real OAuth token", login, "syntheticaccount")
	}
}

// TestTwitchChatCredentialsTrackTheJar: the getter is a METHOD VALUE, so a
// cookie re-import underneath a running job moves both halves together. A
// snapshot would leave the next reconnect presenting one session's credential
// under another session's identity.
func TestTwitchChatCredentialsTrackTheJar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.txt")
	writeTwitchSessionCookies(t, path, "token-before", "accountbefore")

	jar := cookies.NewCookieJar()
	if err := jar.Load(path); err != nil {
		t.Fatal(err)
	}

	credentials := twitchChatCredentials(twitch.NewAuth(jar, nopWorkerLogger{}))
	if token, login := credentials(); token != "token-before" || login != "accountbefore" {
		t.Fatalf("credentials = (%q, %q), want (%q, %q)",
			token, login, "token-before", "accountbefore")
	}

	writeTwitchSessionCookies(t, path, "token-after", "accountafter")
	if err := jar.Reload(); err != nil {
		t.Fatal(err)
	}

	token, login := credentials()
	if token != "token-after" {
		t.Errorf("token = %q after a re-import, want %q", token, "token-after")
	}
	if login != "accountafter" {
		t.Errorf("login = %q after a re-import, want %q — the credential moved and the identity "+
			"did not", login, "accountafter")
	}
}

// TestTwitchChatCredentialsNilAuthIsFullyAnonymous: no half without the other.
// A service constructed before cookies are wired yields a nil getter, which the
// downloader reads as the anonymous login on both lines — never a token with no
// identity behind it, and never an identity with no token.
func TestTwitchChatCredentialsNilAuthIsFullyAnonymous(t *testing.T) {
	if credentials := twitchChatCredentials(nil); credentials != nil {
		token, login := credentials()
		t.Errorf("credential getter is non-nil for a nil Auth (yields %q, %q)", token, login)
	}
}

// --- the downgrade notice ---

// downgradeJob is the job every notice test renders against: a realistic live
// Twitch row, and nothing on it that a credential could hide in.
func downgradeJob() *database.Job {
	return &database.Job{
		ID:           "job-1234",
		VideoID:      "tw_testchan",
		URL:          "https://twitch.tv/testchan",
		ChannelName:  "TestChan",
		ThumbnailURL: "https://static-cdn.example/preview.jpg",
		Platform:     "twitch",
	}
}

// notice is one delivered notification, flattened.
type notice struct {
	title       string
	description string
	ntype       notifications.NotificationType
	fields      []notifications.Field
	opts        notifications.SendOptions
}

// rendered joins every byte a target would receive — title, description, each
// field's NAME and VALUE, and the options — into one string.
//
// Every part, because a credential leak does not get to choose where it lands:
// asserting on the description alone passes a build that puts the login in a
// field label, and asserting on the fields alone passes one that puts it in the
// embed's link URL.
func (n notice) rendered() string {
	parts := []string{n.title, n.description, n.opts.URL, n.opts.Event, n.opts.Thumbnail, n.opts.Image}
	for _, f := range n.fields {
		parts = append(parts, f.Name, f.Value)
	}
	return strings.Join(parts, "\x00")
}

func (n notice) field(name string) (string, bool) {
	for _, f := range n.fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

// recordingNotifier is the seam. notifySend is Manager.Send's shape, and the
// production wiring passes the method itself, so what this records is what a
// configured Discord target would be handed.
//
// A real *notifications.Manager cannot stand in here: its only constructor
// builds targets from config URLs, and every accepted URL is a live Discord
// webhook. Recording at the Send boundary is as close to the manager as a test
// can get without posting to the internet.
type recordingNotifier struct {
	sent []notice
}

func (r *recordingNotifier) send(title, description string, ntype notifications.NotificationType,
	fields []notifications.Field, opts notifications.SendOptions,
) {
	r.sent = append(r.sent, notice{title, description, ntype, fields, opts})
}

// TestTwitchChatDowngradeSendsOneNoticeNamingTheChannel is the delivered shape.
//
// The title is asserted EXACTLY, and that is the junction this test exists to
// close: "a notification was sent" is satisfied by any of the dozen other Sends
// this package makes about a Twitch job — the download-starting embed, an
// outage pause, a quality split. Only the exact title proves THIS notice
// reached the operator.
func TestTwitchChatDowngradeSendsOneNoticeNamingTheChannel(t *testing.T) {
	rec := &recordingNotifier{}
	sendTwitchChatDowngrade(rec.send, downgradeJob(), "TestChan", twitch.AuthDowngradeNoLoginCookie)

	if len(rec.sent) != 1 {
		t.Fatalf("sent %d notifications, want exactly 1", len(rec.sent))
	}
	got := rec.sent[0]
	if got.title != "Twitch chat is anonymous for TestChan" {
		t.Errorf("title = %q, want %q", got.title, "Twitch chat is anonymous for TestChan")
	}
	// A degradation, not a failure: the capture is still running and still
	// produces a usable archive. Dressing it as an error trains an operator to
	// ignore the ones that are.
	if got.ntype != notifications.TypeWarning {
		t.Errorf("type = %v, want TypeWarning", got.ntype)
	}
	if got.opts.Event != "auth" {
		t.Errorf("event = %q, want %q — an operator filtering for credential trouble is filtering "+
			"for this too", got.opts.Event, "auth")
	}
	if v, ok := got.field("Reason"); !ok || v != twitch.AuthDowngradeNoLoginCookie {
		t.Errorf("Reason field = %q (present=%v), want %q", v, ok, twitch.AuthDowngradeNoLoginCookie)
	}
	if v, ok := got.field("Job"); !ok || v != "job-1234" {
		t.Errorf("Job field = %q (present=%v), want %q", v, ok, "job-1234")
	}
	if v, ok := got.field("Channel"); !ok || v != "TestChan" {
		t.Errorf("Channel field = %q (present=%v), want %q", v, ok, "TestChan")
	}
	// The description is pinned EXACTLY, and the second half of it is why.
	//
	// Chat is where a broken Twitch credential is detected, not the extent of
	// what it costs. The playback access token is fetched once per capture and
	// the signed playlist is polled without re-checking it, so this download
	// finishes on the entitlements it already holds — but the next one requests
	// its token anonymously, gets served stitched ads (skipped, leaving a
	// timestamp jump in the archive, logged only at Info) and is refused
	// outright on subscriber-only content. An operator told "chat only" reads
	// "no rush" and finds the holes hours later with nothing in the log above
	// Info to explain them. A Contains() check on the remedy passes a build that
	// quietly drops that sentence, so this asserts the whole string.
	wantDescription := "The cookie file has a Twitch auth-token but no login cookie beside it. " +
		"Chat is still being recorded, but subscriber-only messages and badges will be missing " +
		"for this job. This download is unaffected — its playback token was already issued — but " +
		"the NEXT capture will start anonymous: expect ad-break gaps in the archive, and outright " +
		"failure on subscriber-only content, until the cookies are fixed. Re-export cookies from " +
		"a browser signed in to Twitch, or run R F (Force Cookie Refresh)."
	if got.description != wantDescription {
		t.Errorf("description =\n  %q\nwant\n  %q", got.description, wantDescription)
	}
}

// TestTwitchChatDowngradeNoticeCarriesNoCredential is the constraint that
// outranks everything else here: this payload leaves the machine.
//
// It is checked on the WHOLE rendered payload for every reason in the
// vocabulary, including one the mapping does not know. The structural guarantee
// behind it is that twitchChatDowngradeNotice takes only a job, a channel name,
// and a reason token — there is no path from it to the cookie jar, the
// credential getter, or a chat line — and internal/twitch pins the other half:
// the reason token itself never carries a credential.
func TestTwitchChatDowngradeNoticeCarriesNoCredential(t *testing.T) {
	// The synthetic pair the Twitch-side tests drive these same code paths
	// with. Neither may appear anywhere in a rendered notice.
	const (
		fixtureToken = "token-one"
		fixtureLogin = "archiveraccount"
	)
	for _, reason := range []string{
		twitch.AuthDowngradeLoginRefused,
		twitch.AuthDowngradeLoginUnacknowledged,
		twitch.AuthDowngradeNoLoginCookie,
		twitch.AuthDowngradeUnusableLoginCookie,
		"a-reason-added-upstream-without-a-case-here",
	} {
		t.Run(reason, func(t *testing.T) {
			title, description, fields, opts := twitchChatDowngradeNotice(downgradeJob(), "TestChan", reason)
			n := notice{title: title, description: description, fields: fields, opts: opts}

			for _, secret := range []string{fixtureToken, fixtureLogin} {
				if strings.Contains(n.rendered(), secret) {
					t.Errorf("the rendered notice carried a credential (%q): %q", secret, n.rendered())
				}
			}
			// An unknown reason must still describe the degradation. A notice
			// that names no problem is worse than the log line it replaces.
			if !strings.Contains(description, "subscriber-only messages") {
				t.Errorf("description = %q, want it to name what is being lost", description)
			}
			// And EVERY reason, known or not, must carry the video half of the
			// cost — the part that decides whether the operator re-exports now
			// or next week. It lives in a shared tail today; a per-reason
			// rewrite that kept it for only some is what this catches.
			if !strings.Contains(description, "NEXT capture will start anonymous") {
				t.Errorf("description = %q, want it to name what the NEXT capture loses", description)
			}
			if title == "" || len(fields) == 0 {
				t.Errorf("notice = (%q, %v), want a rendered payload", title, fields)
			}
		})
	}
}

// TestTwitchChatDowngradeReasonsAreDistinct: one state, one sentence, for
// every member of the vocabulary.
//
// Collapsing them to one generic line passes every other test in this file, and
// costs the operator the only part of the notice that says WHICH remedy applies
// — a refused login is a dead cookie, an unusable one is a hand-edited file.
//
// The playback-token route is in the list although nothing sends a chat notice
// for it today (the HLS side marks the platform and logs, it does not notify).
// That is what makes the mirror's coverage of the WHOLE vocabulary a
// requirement rather than a comment: a later caller that does route it here
// must not land on the generic sentence.
func TestTwitchChatDowngradeReasonsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	generic := twitchChatDowngradeReason("a-reason-added-upstream-without-a-case-here")
	for _, reason := range []string{
		twitch.AuthDowngradeLoginRefused,
		twitch.AuthDowngradeLoginUnacknowledged,
		twitch.AuthDowngradeNoLoginCookie,
		twitch.AuthDowngradeUnusableLoginCookie,
		twitch.AuthDowngradePlaybackTokenAnonymous,
	} {
		sentence := twitchChatDowngradeReason(reason)
		if sentence == "" {
			t.Errorf("reason %q renders no sentence", reason)
		}
		if sentence == generic {
			t.Errorf("reason %q renders the generic fallback — it has no arm of its own", reason)
		}
		if prev, dup := seen[sentence]; dup {
			t.Errorf("reasons %q and %q render the same sentence %q", prev, reason, sentence)
		}
		seen[sentence] = reason
	}
}

// TestTwitchChatDowngradeWithoutANotifierIsSilent: notifications are optional
// and most installs have none configured. The callback must be inert rather
// than fatal — a nil-manager panic here would take down the IRC session
// goroutine that fired it, turning a REPORT about degraded chat into no chat.
//
// The composition under test is the PRODUCTION one: a StreamProcessor with no
// notifier, resolved through the same notifierSend the construction site
// passes. Substituting a hand-written nil resolver here would leave
// notifierSend's nil arm — the only thing standing between a disabled-
// notifications install and a nil-pointer dereference — untested.
func TestTwitchChatDowngradeWithoutANotifierIsSilent(t *testing.T) {
	// A processor with no notifier is the disabled-notifications install.
	sp := &StreamProcessor{}
	if send := sp.notifierSend(); send != nil {
		t.Error("notifierSend returned a sender for a processor with no manager")
	}
	callback := twitchChatDowngradeCallback(
		sp.notifierSend, func() func(string) { return nil }, downgradeJob(), "TestChan")
	if callback == nil {
		t.Fatal("callback is nil — the downloader would report to nothing")
	}
	callback(twitch.AuthDowngradeLoginRefused)

	// And the send seam itself, for a caller that resolves the manager to a nil
	// func rather than checking the manager.
	sendTwitchChatDowngrade(nil, downgradeJob(), "TestChan", twitch.AuthDowngradeLoginRefused)
}

// TestTwitchChatDowngradeCallbackDeliversWhatTheDownloaderReports pins the
// wiring end: the reason the downloader hands over is the reason that reaches
// the target, unmodified.
//
// It drives the CALLBACK — the closure the downloader actually calls — and not
// the send helper underneath it. That distinction is the test: the closure is
// the one place on this path where a reason can be swallowed, rewritten, or
// replaced by a constant, and a test that called sendTwitchChatDowngrade
// directly would step over exactly that code and survive all three.
func TestTwitchChatDowngradeCallbackDeliversWhatTheDownloaderReports(t *testing.T) {
	for _, reason := range []string{
		twitch.AuthDowngradeLoginRefused,
		twitch.AuthDowngradeLoginUnacknowledged,
		twitch.AuthDowngradeNoLoginCookie,
		twitch.AuthDowngradeUnusableLoginCookie,
	} {
		t.Run(reason, func(t *testing.T) {
			rec := &recordingNotifier{}
			callback := twitchChatDowngradeCallback(
				func() notifySend { return rec.send },
				func() func(string) { return nil },
				downgradeJob(), "TestChan")

			callback(reason)

			if len(rec.sent) != 1 {
				t.Fatalf("sent %d notifications, want exactly 1", len(rec.sent))
			}
			if v, _ := rec.sent[0].field("Reason"); v != reason {
				t.Errorf("Reason field = %q, want %q — the callback did not pass on what the "+
					"downloader reported", v, reason)
			}
			if rec.sent[0].title != "Twitch chat is anonymous for TestChan" {
				t.Errorf("title = %q, want the downgrade notice's own title", rec.sent[0].title)
			}
		})
	}
}

// TestTwitchChatDowngradeCallbackResolvesTheSenderPerFire: the lookup runs on
// every call, not once when the callback is built.
//
// A job runs for hours and its callback is built at the start of it. Capturing
// the sender at build time would mean a notifier wired, reloaded, or removed
// during the stream is not the one this notice goes to — and on the removal
// side, a captured sender outliving its manager is the shape that turns a
// disabled-notifications setting into a delivery anyway.
func TestTwitchChatDowngradeCallbackResolvesTheSenderPerFire(t *testing.T) {
	rec := &recordingNotifier{}
	var send notifySend // nothing wired yet, exactly as at worker construction
	callback := twitchChatDowngradeCallback(
		func() notifySend { return send },
		func() func(string) { return nil },
		downgradeJob(), "TestChan")

	callback(twitch.AuthDowngradeLoginRefused)
	if len(rec.sent) != 0 {
		t.Fatalf("sent %d notifications before a notifier existed, want 0", len(rec.sent))
	}

	send = rec.send
	callback(twitch.AuthDowngradeNoLoginCookie)
	if len(rec.sent) != 1 {
		t.Fatalf("sent %d notifications, want 1 — the callback captured the sender it was built "+
			"with instead of resolving the current one", len(rec.sent))
	}
}

// nopWorkerLogger satisfies twitch.NewAuth's anonymous logger interface.
type nopWorkerLogger struct{}

func (nopWorkerLogger) Debug(string, ...any) {}
func (nopWorkerLogger) Info(string, ...any)  {}
func (nopWorkerLogger) Warn(string, ...any)  {}
func (nopWorkerLogger) Error(string, ...any) {}

// TestTwitchChatDowngradeCallbackMarksThePlatformAndNotifiesTheJob is Arc 10
// R1's wiring claim: ONE report, TWO consumers.
//
// The notification names the job ("Twitch chat is anonymous for X") and the
// mark names the platform. A change that replaced one with the other would
// pass any "something happened" assertion and lose half the behaviour: without
// the notice the operator is not told which capture is degraded; without the
// mark the platform stays green and no recovery is attempted.
//
// The mutation: dropping either call from the callback.
func TestTwitchChatDowngradeCallbackMarksThePlatformAndNotifiesTheJob(t *testing.T) {
	rec := &recordingNotifier{}
	var marked []string

	cb := twitchChatDowngradeCallback(
		func() notifySend { return rec.send },
		func() func(string) { return func(reason string) { marked = append(marked, reason) } },
		downgradeJob(), "TestChan")
	cb(twitch.AuthDowngradeNoLoginCookie)

	if len(marked) != 1 || marked[0] != twitch.AuthDowngradeNoLoginCookie {
		t.Errorf("the platform mark received %v, want exactly [%s]", marked, twitch.AuthDowngradeNoLoginCookie)
	}
	if len(rec.sent) != 1 {
		t.Fatalf("the notifier received %d notices, want exactly 1", len(rec.sent))
	}
	if rec.sent[0].title != "Twitch chat is anonymous for TestChan" {
		t.Errorf("the job notice no longer names the channel: %q", rec.sent[0].title)
	}
	// The mark carries the vocabulary token and nothing else — the same
	// no-credential property assertNoCredentialInReports pins on the notice
	// side, restated for the new consumer.
	if strings.ContainsAny(marked[0], " ;=") {
		t.Errorf("the mark received %q, which is not a bare vocabulary token", marked[0])
	}
}

// TestTwitchChatDowngradeCallbackResolvesTheMarkPerFire.
//
// The resolver is called at FIRE time, not captured at construction. A report
// can arrive hours into a capture, and the seam is wired during startup — a
// captured nil would silence the mark for the life of the process on any
// ordering where the downloader is built first.
//
// The mutation: `mark := resolveMark()` hoisted out of the returned closure.
func TestTwitchChatDowngradeCallbackResolvesTheMarkPerFire(t *testing.T) {
	rec := &recordingNotifier{}
	var marked []string
	var live func(string)

	cb := twitchChatDowngradeCallback(
		func() notifySend { return rec.send },
		func() func(string) { return live },
		downgradeJob(), "TestChan")

	cb(twitch.AuthDowngradeLoginRefused) // nothing wired yet: must not panic
	if len(marked) != 0 {
		t.Fatalf("marks = %v before anything was wired", marked)
	}

	live = func(reason string) { marked = append(marked, reason) }
	cb(twitch.AuthDowngradeLoginRefused)

	if len(marked) != 1 {
		t.Errorf("marks = %v, want one after the seam was wired — the resolver was captured, not called per fire", marked)
	}
}

// TestTwitchChatDowngradeCallbackWithNoMarkStillNotifies. An install with no
// refresh service wired is not an error, and the per-job notice is exactly the
// signal it still needs.
//
// The mutation: `if mark == nil { return }` placed before the send.
func TestTwitchChatDowngradeCallbackWithNoMarkStillNotifies(t *testing.T) {
	rec := &recordingNotifier{}

	cb := twitchChatDowngradeCallback(
		func() notifySend { return rec.send },
		func() func(string) { return nil },
		downgradeJob(), "TestChan")
	cb(twitch.AuthDowngradeUnusableLoginCookie)

	if len(rec.sent) != 1 {
		t.Errorf("the notifier received %d notices with no mark wired, want 1", len(rec.sent))
	}
}

// TestStreamProcessorTwitchAuthLossResolverIsNilSafeAndLive pins the
// production resolver, which is what the callback above is handed.
//
// The mutation: returning a non-nil zero func, or capturing sp.onTwitchAuthLoss
// at construction.
func TestStreamProcessorTwitchAuthLossResolverIsNilSafeAndLive(t *testing.T) {
	sp := &StreamProcessor{}
	if fn := sp.twitchAuthLossReporter(); fn != nil {
		t.Error("an unwired stream processor returned a non-nil auth-loss reporter")
	}

	var got []string
	sp.SetOnTwitchAuthLoss(func(reason string) { got = append(got, reason) })
	fn := sp.twitchAuthLossReporter()
	if fn == nil {
		t.Fatal("a wired stream processor returned a nil auth-loss reporter")
	}
	fn(twitch.AuthDowngradeLoginRefused)
	if len(got) != 1 || got[0] != twitch.AuthDowngradeLoginRefused {
		t.Errorf("the reporter delivered %v, want [%s]", got, twitch.AuthDowngradeLoginRefused)
	}
}
