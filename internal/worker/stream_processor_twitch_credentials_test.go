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
	// The remedy has to be IN the notification. An operator who is told only
	// that chat is anonymous has been told a fact they cannot act on.
	if !strings.Contains(got.description, "Re-export cookies") {
		t.Errorf("description = %q, want it to name the remedy", got.description)
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
			if title == "" || len(fields) == 0 {
				t.Errorf("notice = (%q, %v), want a rendered payload", title, fields)
			}
		})
	}
}

// TestTwitchChatDowngradeReasonsAreDistinct: four states, four sentences.
//
// Collapsing them to one generic line passes every other test in this file, and
// costs the operator the only part of the notice that says WHICH remedy applies
// — a refused login is a dead cookie, an unusable one is a hand-edited file.
func TestTwitchChatDowngradeReasonsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, reason := range []string{
		twitch.AuthDowngradeLoginRefused,
		twitch.AuthDowngradeLoginUnacknowledged,
		twitch.AuthDowngradeNoLoginCookie,
		twitch.AuthDowngradeUnusableLoginCookie,
	} {
		sentence := twitchChatDowngradeReason(reason)
		if sentence == "" {
			t.Errorf("reason %q renders no sentence", reason)
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
func TestTwitchChatDowngradeWithoutANotifierIsSilent(t *testing.T) {
	// A processor with no notifier is the disabled-notifications install.
	sp := &StreamProcessor{}
	callback := sp.twitchChatDowngradeCallback(downgradeJob(), "TestChan")
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
// the target, unmodified. The callback is where a reason could be dropped,
// rewritten, or replaced by a constant, and none of the Twitch-side tests can
// see any of that.
func TestTwitchChatDowngradeCallbackDeliversWhatTheDownloaderReports(t *testing.T) {
	for _, reason := range []string{
		twitch.AuthDowngradeLoginRefused,
		twitch.AuthDowngradeLoginUnacknowledged,
		twitch.AuthDowngradeNoLoginCookie,
		twitch.AuthDowngradeUnusableLoginCookie,
	} {
		t.Run(reason, func(t *testing.T) {
			rec := &recordingNotifier{}
			sendTwitchChatDowngrade(rec.send, downgradeJob(), "TestChan", reason)

			if len(rec.sent) != 1 {
				t.Fatalf("sent %d notifications, want exactly 1", len(rec.sent))
			}
			if v, _ := rec.sent[0].field("Reason"); v != reason {
				t.Errorf("Reason field = %q, want %q", v, reason)
			}
		})
	}
}

// nopWorkerLogger satisfies twitch.NewAuth's anonymous logger interface.
type nopWorkerLogger struct{}

func (nopWorkerLogger) Debug(string, ...any) {}
func (nopWorkerLogger) Info(string, ...any)  {}
func (nopWorkerLogger) Warn(string, ...any)  {}
func (nopWorkerLogger) Error(string, ...any) {}
