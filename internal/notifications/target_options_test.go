package notifications

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// TestBuildTargetsSkipsDisabled pins the mute switch. A disabled target must
// leave the delivery list entirely — not deliver-and-discard — so HasTargets
// reports false when everything is muted and the HasTargets guards in the
// monitor and disk paths stop building embeds nobody will read.
func TestBuildTargetsSkipsDisabled(t *testing.T) {
	cfg := &config.MoomboxConfig{Notifications: []config.NotificationConfig{
		{URL: "discord://1/aaa", Enabled: boolPtr(false)},
		{URL: "discord://2/bbb"},
	}}
	targets := buildTargets(cfg, testLogger{})
	if len(targets) != 1 {
		t.Fatalf("buildTargets returned %d targets, want 1 — the disabled entry was not skipped", len(targets))
	}

	allOff := &config.MoomboxConfig{Notifications: []config.NotificationConfig{
		{URL: "discord://1/aaa", Enabled: boolPtr(false)},
	}}
	m := NewManager(allOff, testLogger{})
	if m.HasTargets() {
		t.Error("HasTargets reported true with every target disabled — all FOUR producer guards " +
			"(cmd/moombox/helpers.go update-available, cmd/moombox/main.go disk, and both " +
			"cmd/moombox/monitor_callbacks.go Stream Found sites) would keep building embeds " +
			"that go nowhere")
	}
}

// TestBuildTargetsDisabledDoesNotShadowItsTwin guards the interaction with
// target dedupe: two spellings of ONE webhook collapse on the resolved URL,
// and the disabled spelling must not take the survivor's slot.
func TestBuildTargetsDisabledDoesNotShadowItsTwin(t *testing.T) {
	// The two entries carry DIFFERENT filters on purpose: with both unfiltered
	// the surviving target's filter is nil either way, and the assertion could
	// not tell a skip-before-dedupe from a skip-after-dedupe.
	cfg := &config.MoomboxConfig{Notifications: []config.NotificationConfig{
		{URL: "discord://1/aaa", Enabled: boolPtr(false), Events: []string{"error"}},
		{URL: "https://discord.com/api/webhooks/1/aaa", Events: []string{"finished"}},
	}}
	targets := buildTargets(cfg, testLogger{})
	if len(targets) != 1 {
		t.Fatalf("buildTargets returned %d targets, want 1", len(targets))
	}
	if !targets[0].events["finished"] || targets[0].events["error"] {
		t.Errorf("the surviving target's filter is %v — it inherited the disabled entry's filter, "+
			"so the skip runs after the dedupe instead of before it", targets[0].events)
	}
}

// TestMentionFor is the whole mention decision: who pings, for what, and the
// three ways to say "nobody". The alias row matters because a target that
// asked to be pinged for disk_warning must be pinged for the MORE urgent
// disk_critical — silently losing the escalation is the worst outcome here.
func TestMentionFor(t *testing.T) {
	// Through newTargetQueue, not the bare notificationTarget: mentionFor is a
	// *targetQueue method because that is the only object Send iterates, and a
	// test that called it on the build-time value would not prove the fields
	// survive newTargetQueue's copy.
	mk := func(nc config.NotificationConfig) *targetQueue {
		nc.URL = "discord://1/aaa"
		got := buildTargets(&config.MoomboxConfig{Notifications: []config.NotificationConfig{nc}}, testLogger{})
		if len(got) != 1 {
			t.Fatalf("buildTargets returned %d targets", len(got))
		}
		return newTargetQueue(got[0], testLogger{}, nil, nil)
	}
	never := []string{}
	only := []string{"finished"}
	warnOnly := []string{"disk_warning"}
	pauseOnly := []string{"connectivity_pause"}

	for _, tc := range []struct {
		name  string
		nc    config.NotificationConfig
		event string
		want  string
		why   string
	}{
		{"no mention configured", config.NotificationConfig{}, "error", "",
			"a target with no mention key never pings"},
		{"default list, error", config.NotificationConfig{Mention: "@here"}, "error", "@here",
			"error is in the ruling's default six"},
		{"default list, finished", config.NotificationConfig{Mention: "@here"}, "finished", "",
			"finished is not in the default six — a completed download must not ping anyone"},
		{"explicit never", config.NotificationConfig{Mention: "@here", MentionEvents: &never}, "error", "",
			"an explicit empty list means never, even for error"},
		{"explicit list, hit", config.NotificationConfig{Mention: "@here", MentionEvents: &only}, "finished", "@here",
			"an explicit list is honoured as written"},
		{"explicit list, miss", config.NotificationConfig{Mention: "@here", MentionEvents: &only}, "error", "",
			"error is not in this operator's list"},
		{"alias escalation", config.NotificationConfig{Mention: "<@&123456789012345678>", MentionEvents: &warnOnly},
			"disk_critical", "<@&123456789012345678>",
			"disk_critical aliases to disk_warning; a target asking to be pinged for the warning must be " +
				"pinged for the critical"},
		{"retired-key alias", config.NotificationConfig{Mention: "@here", MentionEvents: &pauseOnly},
			"connectivity_resume", "@here",
			"N1 retired connectivity_pause and mapped connectivity_resume onto it; a config written " +
				"before that retirement must keep pinging on the embed that now carries the pause"},
		{"empty event never pings", config.NotificationConfig{Mention: "@here"}, "", "",
			"an empty Event bypasses every filter by design; it must not therefore ping everyone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := mk(tc.nc).mentionFor(tc.event); got != tc.want {
				t.Errorf("mentionFor(%q) = %q, want %q — %s", tc.event, got, tc.want, tc.why)
			}
		})
	}
}

// TestMentionAllowedPerForm is spec §3.2's other half. Discord ignores a
// content mention unless allowed_mentions names it, so a target whose role
// ping renders as grey text is indistinguishable from a delivery failure —
// and the webhook default (`{"parse": ["users"]}`) would swallow a role ping
// silently.
//
// The EMPTY `Parse` on the role and user rows is load-bearing, not
// incidental. AllowedMentions.Parse is `json:"parse"` WITHOUT omitempty
// (internal/notifications/manager.go) precisely so the list is always on the
// wire: an omitted or null parse re-widens a role ping into the webhook
// default. MentionParse (internal/notifications/discord.go) returns
// []string{}; a hand-built literal would return nil, which marshals to
// `"parse": null` — a different wire form, and reflect.DeepEqual separates
// them. That is why this test builds nothing itself.
func TestMentionAllowedPerForm(t *testing.T) {
	for _, tc := range []struct {
		mention string
		want    AllowedMentions
	}{
		{"<@&123456789012345678>", AllowedMentions{Parse: []string{}, Roles: []string{"123456789012345678"}}},
		{"<@123456789012345678>", AllowedMentions{Parse: []string{}, Users: []string{"123456789012345678"}}},
		{"@everyone", AllowedMentions{Parse: []string{"everyone"}}},
		{"@here", AllowedMentions{Parse: []string{"everyone"}}},
	} {
		built := buildTargets(&config.MoomboxConfig{Notifications: []config.NotificationConfig{
			{URL: "discord://1/aaa", Mention: tc.mention},
		}}, testLogger{})[0]
		_, got := newTargetQueue(built, testLogger{}, nil, nil).mentionFor("error")
		if got == nil || !reflect.DeepEqual(*got, tc.want) {
			t.Errorf("mentionFor for %s = %+v, want %+v", tc.mention, got, tc.want)
		}
	}
}

// recordOpts is the shared recorder for the three Send tests below: a
// senderFunc that appends every delivered Message under a mutex (the queue
// delivers from its own goroutine) and the slice it fills.
//
// The whole Message, not just its embed's SendOptions: the mention travels at
// the MESSAGE level now (Discord applies content and allowed_mentions per
// message), so recording only the opts would drop the very field
// TestSendAttachesMention asserts on. The per-embed options are one hop away,
// at .Embeds[0].Opts.
func recordOpts() (*[]Message, *sync.Mutex, senderFunc) {
	var mu sync.Mutex
	got := &[]Message{}
	return got, &mu, senderFunc(func(msg Message) error {
		mu.Lock()
		defer mu.Unlock()
		*got = append(*got, msg)
		return nil
	})
}

// mentioningTarget is the built target the mention tests deliver through.
func mentioningTarget(t *testing.T, s sender, nc config.NotificationConfig) notificationTarget {
	t.Helper()
	nc.URL = "discord://1/aaa"
	built := buildTargets(&config.MoomboxConfig{Notifications: []config.NotificationConfig{nc}}, testLogger{})
	if len(built) != 1 {
		t.Fatalf("buildTargets returned %d targets", len(built))
	}
	built[0].sender = s // keep the resolved key and the mention, swap the transport
	return built[0]
}

// TestSendAttachesMention proves the decision reaches the delivered send, and
// that a target whose filter excludes the event gets no content line at all.
func TestSendAttachesMention(t *testing.T) {
	only := []string{"error"}
	got, mu, rec := recordOpts()
	m := newTestManager(t, time.Second, mentioningTarget(t, rec, config.NotificationConfig{
		Mention:       "<@&123456789012345678>",
		MentionEvents: &only,
	}))

	m.Send("Job Failed", "d", TypeError, nil, SendOptions{Event: "error"})
	m.Send("Download Finished", "d", TypeSuccess, nil, SendOptions{Event: "finished"})
	m.Wait() // closeDrain + block until the queue goroutine has delivered both

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Fatalf("recorded %d sends, want 2", len(*got))
	}
	if (*got)[0].Mention != "<@&123456789012345678>" {
		t.Errorf("the error send carried Mention = %q, want the configured role", (*got)[0].Mention)
	}
	if (*got)[0].MentionAllowed == nil {
		t.Error("the error send carried no allowed_mentions — Discord would render the role as plain " +
			"text and ping nobody")
	}
	if (*got)[1].Mention != "" {
		t.Errorf("the finished send carried Mention = %q, want none — it is not in the mention filter", (*got)[1].Mention)
	}
	if (*got)[1].MentionAllowed != nil {
		t.Error("the finished send carried an allowed_mentions object with no content mention to match it")
	}
}

// TestJobDeepLink pins the link shape the SPA parses, including the defensive
// trailing-slash trim (the stored value is normalised, but a value that
// reached the manager through a path that skipped Normalize must not produce
// a double slash).
func TestJobDeepLink(t *testing.T) {
	for _, tc := range []struct{ pub, id, want string }{
		{"", "j1", ""},
		{"https://x.example", "", ""},
		{"https://x.example", "j1", "https://x.example/#job=j1"},
		{"https://x.example/", "j1", "https://x.example/#job=j1"},
		{"https://x.example/moombox", "j1", "https://x.example/moombox/#job=j1"},
		{"https://x.example", "a b", "https://x.example/#job=a%20b"},
	} {
		if got := JobDeepLink(tc.pub, tc.id); got != tc.want {
			t.Errorf("JobDeepLink(%q, %q) = %q, want %q", tc.pub, tc.id, got, tc.want)
		}
	}
}

// TestSendRewritesJobURL is ruling Q6: with public_url set, a job embed's
// TITLE goes to the dashboard and the platform page moves to the author line.
// The copy assertion is the one that would bite in production — Author is a
// pointer the caller still owns and every target shares, so an in-place write
// would leak one target's rewrite into the next.
func TestSendRewritesJobURL(t *testing.T) {
	got, mu, rec := recordOpts()
	author := &Author{Name: "Some Channel", URL: "https://youtube.com/@some", IconURL: "https://i/a.jpg"}
	// TWO targets, distinct keys: applyTargets dedupes on key, and the whole
	// point of the copy assertion is that target 1's rewrite must not have
	// mutated the Author target 2 then sees.
	m := newTestManager(t, time.Second,
		notificationTarget{sender: rec, key: "k1"},
		notificationTarget{sender: rec, key: "k2"},
	)
	m.publicURL = "https://moombox.example.com"

	m.Send("Download Finished", "d", TypeSuccess, nil, SendOptions{
		Event:  "finished",
		JobID:  "job-1",
		URL:    "https://youtube.com/watch?v=abc",
		Author: author,
	})
	m.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Fatalf("recorded %d sends, want 2", len(*got))
	}
	for i, sent := range *got {
		o := sent.Embeds[0].Opts
		if o.URL != "https://moombox.example.com/#job=job-1" {
			t.Errorf("send %d: title URL = %q, want the dashboard deep link", i, o.URL)
		}
		if o.Author == nil || o.Author.URL != "https://youtube.com/watch?v=abc" {
			t.Errorf("send %d: author URL = %v, want the platform page it displaced", i, o.Author)
		}
		if o.Author != nil && o.Author.Name != "Some Channel" {
			t.Errorf("send %d: the author's name was lost in the rewrite", i)
		}
	}
	if author.URL != "https://youtube.com/@some" {
		t.Errorf("the caller's Author was mutated in place (URL = %q) — it is shared across targets "+
			"and with the producer", author.URL)
	}
}

// TestSendLeavesURLAloneWithoutAuthorOrJobID pins the three no-rewrite arms.
// The no-Author arm is deliberate: with nowhere to put the platform page,
// rewriting the title would DELETE the only link to the video.
func TestSendLeavesURLAloneWithoutAuthorOrJobID(t *testing.T) {
	for _, tc := range []struct {
		name string
		pub  string
		opts SendOptions
	}{
		{"no public_url", "", SendOptions{Event: "finished", JobID: "j1", URL: "https://p/v",
			Author: &Author{Name: "c", URL: "https://p/c"}}},
		{"no job id", "https://x.example", SendOptions{Event: "disk_warning", URL: "https://p/v",
			Author: &Author{Name: "c", URL: "https://p/c"}}},
		{"no author", "https://x.example", SendOptions{Event: "finished", JobID: "j1", URL: "https://p/v"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, mu, rec := recordOpts()
			m := newTestManager(t, time.Second, notificationTarget{sender: rec, key: "k1"})
			m.publicURL = tc.pub
			m.Send("t", "d", TypeInfo, nil, tc.opts)
			m.Wait()
			mu.Lock()
			defer mu.Unlock()
			if len(*got) != 1 {
				t.Fatalf("recorded %d sends, want 1", len(*got))
			}
			if got0 := (*got)[0].Embeds[0].Opts; got0.URL != tc.opts.URL {
				t.Errorf("URL = %q, want %q left alone", got0.URL, tc.opts.URL)
			}
		})
	}
}

// TestReloadPicksUpPublicURL pins the hot-reload path the web route drives:
// the manager reads the value from the config it was reloaded with, so a
// Settings save changes the next embed's link with no restart.
func TestReloadPicksUpPublicURL(t *testing.T) {
	m := NewManager(&config.MoomboxConfig{}, testLogger{})
	if m.publicURL != "" {
		t.Fatalf("a fresh manager has publicURL = %q", m.publicURL)
	}
	cfg := &config.MoomboxConfig{}
	cfg.Network.PublicURL = "https://later.example"
	m.Reload(cfg)
	if m.publicURL != "https://later.example" {
		t.Errorf("Reload left publicURL = %q — a public_url save would need a restart", m.publicURL)
	}
}

// TestReloadCarriesTheMentionToASurvivingTarget is applyTargets' survivor arm.
// A target whose URL did not change KEEPS its queue — that is deliberate, so a
// save touching an unrelated section does not cost every webhook its backlog
// and its learned rate bucket — and the freshly built notificationTarget is
// discarded. setEvents exists so the FILTER still follows the save; without
// the matching setMention, changing only `mention` or `mention_events` is
// accepted by both UIs, written to the file, and then ignored until restart.
func TestReloadCarriesTheMentionToASurvivingTarget(t *testing.T) {
	const url = "discord://1/aaa"
	before := &config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: url}}}
	m := NewManager(before, testLogger{})
	t.Cleanup(func() { m.Wait() })

	m.targetsMu.RLock()
	q := m.targets[0]
	m.targetsMu.RUnlock()
	if got, _ := q.mentionFor("error"); got != "" {
		t.Fatalf("a target with no mention resolved %q", got)
	}

	after := &config.MoomboxConfig{Notifications: []config.NotificationConfig{{URL: url, Mention: "@here"}}}
	m.Reload(after)

	m.targetsMu.RLock()
	same := m.targets[0] == q
	q2 := m.targets[0]
	m.targetsMu.RUnlock()
	if !same {
		t.Fatal("the surviving target did not keep its queue — applyTargets' diff is broken, and this " +
			"test is no longer testing what it says")
	}
	if got, allowed := q2.mentionFor("error"); got != "@here" || allowed == nil {
		t.Errorf("after Reload the surviving target resolves (%q, %v) for error, want (\"@here\", non-nil) — "+
			"applyTargets kept the queue but not the new mention", got, allowed)
	}
}
