package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestValidatePublicURL is network.public_url's whole contract in one table.
// The key is read at SEND time by the notification manager and pasted into
// every job embed's title link, so a value that parses but is not a browsable
// absolute origin produces a dead link in someone's Discord channel rather
// than an error anyone sees. Each rejection below is a link that would have
// been dead: a relative path has no host to resolve against, a query or a
// fragment collides with the "#job=<id>" the manager appends, and embedded
// credentials would be republished to every reader of the channel.
func TestValidatePublicURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
		why     string
	}{
		{"unset", "", "", false, "empty is the documented 'no dashboard link' case, not an error"},
		{"whitespace only", "   ", "", false, "a blank field from either editor means unset"},
		{"https host", "https://moombox.example.com", "https://moombox.example.com", false, "the ordinary case"},
		{"http host", "http://192.168.1.10:774", "http://192.168.1.10:774", false, "a LAN install over plain http is legitimate"},
		{"trailing slash trimmed", "https://x.example/", "https://x.example", false,
			"the manager appends \"/#job=\", so a stored trailing slash would produce \"//#job=\""},
		{"sub-path trailing slash trimmed", "https://x.example/moombox/", "https://x.example/moombox", false,
			"a reverse-proxy sub-path is legitimate; only the trailing slash goes"},
		{"surrounding space trimmed", "  https://x.example  ", "https://x.example", false, "a paste carries spaces"},
		{"no scheme", "moombox.example.com", "", true, "with no scheme there is no origin to link to"},
		{"relative", "/moombox", "", true, "a path alone cannot be the base of an absolute embed link"},
		{"ftp scheme", "ftp://x.example", "", true, "only http and https render as links in a Discord embed"},
		{"no host", "https://", "", true, "a scheme with no authority resolves to nothing"},
		{"query", "https://x.example/?a=b", "", true, "a query would sit before the #job fragment and change the request"},
		{"fragment", "https://x.example/#top", "", true, "the fragment slot is what the deep link uses"},
		{"userinfo", "https://user:pw@x.example", "", true,
			"credentials in the base URL would be republished into every embed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidatePublicURL(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidatePublicURL(%q) err = %v, wantErr %v — %s", tc.in, err, tc.wantErr, tc.why)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("ValidatePublicURL(%q) = %q, want %q — %s", tc.in, got, tc.want, tc.why)
			}
		})
	}
}

// TestPublicURLValidateAndNormalizeAgree pins both arms of the one
// implementation (DECISIONS #9): Validate REPORTS without mutating, Normalize
// REPLACES. A bad public_url normalises to "" (unset) rather than to some
// other URL — there is no sensible default host to invent.
func TestPublicURLValidateAndNormalizeAgree(t *testing.T) {
	reported := Defaults()
	reported.Network.PublicURL = "ftp://nope"
	errs := Validate(reported)
	mentioned := false
	for _, err := range errs {
		if strings.Contains(err.Error(), "network.public_url") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("Validate did not mention network.public_url for %q (errs: %v)", reported.Network.PublicURL, errs)
	}
	if reported.Network.PublicURL != "ftp://nope" {
		t.Errorf("Validate mutated the config: public_url = %q", reported.Network.PublicURL)
	}

	normalised := Defaults()
	normalised.Network.PublicURL = "ftp://nope"
	Normalize(normalised)
	if normalised.Network.PublicURL != "" {
		t.Errorf("Normalize left public_url = %q, want \"\" — an unusable base URL must become unset, "+
			"not stay in the config where the manager would paste it into embeds", normalised.Network.PublicURL)
	}

	// A canonicalisable value is rewritten, not rejected.
	tidy := Defaults()
	tidy.Network.PublicURL = "https://x.example/"
	if errs := Validate(tidy); len(errs) != 0 {
		t.Errorf("Validate reported %v for a merely untidy public_url — a trailing slash is normalisable, not invalid", errs)
	}
	Normalize(tidy)
	if tidy.Network.PublicURL != "https://x.example" {
		t.Errorf("Normalize left public_url = %q, want the trailing slash trimmed", tidy.Network.PublicURL)
	}
}

// TestParseMention pins the four forms the owner ruled for plus the one
// legacy spelling Discord clients still emit. The canonical form is what gets
// stored, so <@!123> and <@123> never round-trip as two different targets.
func TestParseMention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        string
		canonical string
		form      MentionForm
		id        string
		wantErr   bool
	}{
		{"unset", "", "", MentionNone, "", false},
		{"role", "<@&123456789012345678>", "<@&123456789012345678>", MentionRole, "123456789012345678", false},
		{"user", "<@123456789012345678>", "<@123456789012345678>", MentionUser, "123456789012345678", false},
		{"legacy nickname user", "<@!123456789012345678>", "<@123456789012345678>", MentionUser, "123456789012345678", false},
		{"everyone", "@everyone", "@everyone", MentionEveryone, "", false},
		{"here", "@here", "@here", MentionHere, "", false},
		{"spaces trimmed", "  @here  ", "@here", MentionHere, "", false},
		{"bare id", "123456789012345678", "", MentionNone, "", true},
		{"plain name", "@ops-team", "", MentionNone, "", true},
		{"non-numeric role", "<@&abc>", "", MentionNone, "", true},
		{"unclosed", "<@&123", "", MentionNone, "", true},
		{"id too long", "<@&123456789012345678901>", "", MentionNone, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canonical, form, id, err := ParseMention(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseMention(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if canonical != tc.canonical || form != tc.form || id != tc.id {
				t.Errorf("ParseMention(%q) = (%q, %v, %q), want (%q, %v, %q)",
					tc.in, canonical, form, id, tc.canonical, tc.form, tc.id)
			}
		})
	}
}

// TestResolveMentionEvents is the three-way rule the *[]string encoding
// exists for. Getting the middle row wrong is the bug that matters: an
// operator who unticks every mention chip would start receiving the DEFAULT
// six pings instead of none.
func TestResolveMentionEvents(t *testing.T) {
	empty := []string{}
	explicit := []string{"finished"}
	for _, tc := range []struct {
		name string
		n    NotificationConfig
		want []string
		why  string
	}{
		{"no mention", NotificationConfig{}, nil, "with nothing to ping, the filter is irrelevant"},
		{"no mention but a list", NotificationConfig{MentionEvents: &explicit}, nil,
			"a filter without a mention still pings nobody"},
		{"mention, absent list", NotificationConfig{Mention: "@here"}, DefaultMentionEvents(),
			"the ruling's default list applies when the key was never written"},
		{"mention, explicit empty", NotificationConfig{Mention: "@here", MentionEvents: &empty}, []string{},
			"an explicit empty list means NEVER — it must not fall back to the defaults"},
		{"mention, explicit list", NotificationConfig{Mention: "@here", MentionEvents: &explicit}, explicit,
			"an explicit list is used as written"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.n.ResolveMentionEvents()
			if !slices.Equal(got, tc.want) {
				t.Errorf("ResolveMentionEvents() = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestDefaultMentionEventsIsTheRuling pins the exact list from the owner's
// 2026-09-27 Q5 answer, and that callers get a copy they cannot corrupt for
// every other target in the process.
func TestDefaultMentionEventsIsTheRuling(t *testing.T) {
	want := []string{"error", "auth", "disk_critical", "update_failed", "crash_recovered", "sidecar_down"}
	if got := DefaultMentionEvents(); !slices.Equal(got, want) {
		t.Errorf("DefaultMentionEvents() = %v, want %v (owner ruling Q5, 2026-09-27)", got, want)
	}
	mine := DefaultMentionEvents()
	mine[0] = "clobbered"
	if DefaultMentionEvents()[0] != "error" {
		t.Error("DefaultMentionEvents returned the package slice — one caller's edit would change every target's defaults")
	}
}

// TestNotificationIsEnabled mirrors ChannelConfig.IsEnabled: absent means on,
// so every config written before this key keeps delivering.
func TestNotificationIsEnabled(t *testing.T) {
	var absent NotificationConfig
	if !absent.IsEnabled() {
		t.Error("a target with no enabled key must deliver — otherwise the key's arrival silences every existing install")
	}
	off := NotificationConfig{Enabled: boolPtr(false)}
	if off.IsEnabled() {
		t.Error("enabled = false must not deliver")
	}
	on := NotificationConfig{Enabled: boolPtr(true)}
	if !on.IsEnabled() {
		t.Error("enabled = true must deliver")
	}
}

// TestMentionEventsShapeNormalises covers what validateOrNormalize does to a
// hand-edited list: blanks and duplicates go, the surviving order is kept, and
// an explicitly empty list stays explicitly empty (it means "never"). The
// VOCABULARY is deliberately not checked here — internal/notifications imports
// internal/config, so config cannot see KnownEvents without an import cycle;
// the alias-aware check lives in buildTargets and in the web save path.
func TestMentionEventsShapeNormalises(t *testing.T) {
	messy := []string{"error", "", "  auth  ", "error"}
	cfg := Defaults()
	cfg.Notifications = []NotificationConfig{{
		URL:           "discord://1/abc",
		Mention:       "@here",
		MentionEvents: &messy,
	}}
	Normalize(cfg)
	got := cfg.Notifications[0].MentionEvents
	if got == nil {
		t.Fatal("Normalize dropped the pointer — an explicit list became the default list")
	}
	if !slices.Equal(*got, []string{"error", "auth"}) {
		t.Errorf("mention_events normalised to %v, want [error auth]", *got)
	}

	empty := []string{}
	keep := Defaults()
	keep.Notifications = []NotificationConfig{{URL: "discord://1/abc", Mention: "@here", MentionEvents: &empty}}
	Normalize(keep)
	if keep.Notifications[0].MentionEvents == nil {
		t.Error("Normalize turned an explicit empty mention_events into absent — \"never ping\" became \"ping on the default six\"")
	}
}

// TestNotificationMentionValidateAndNormalize pins the pair for the mention
// token itself: Validate names the field, Normalize clears an unusable value
// (a stored token Discord cannot resolve renders as literal text in the
// message and pings nobody, which is worse than no content line at all).
func TestNotificationMentionValidateAndNormalize(t *testing.T) {
	bad := Defaults()
	bad.Notifications = []NotificationConfig{{URL: "discord://1/abc", Mention: "@ops-team"}}
	errs := Validate(bad)
	mentioned := false
	for _, err := range errs {
		if strings.Contains(err.Error(), "notifications[0].mention") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("Validate did not name notifications[0].mention for %q (errs: %v)", "@ops-team", errs)
	}
	if bad.Notifications[0].Mention != "@ops-team" {
		t.Error("Validate mutated the config")
	}

	Normalize(bad)
	if bad.Notifications[0].Mention != "" {
		t.Errorf("Normalize left mention = %q, want cleared", bad.Notifications[0].Mention)
	}

	tidy := Defaults()
	tidy.Notifications = []NotificationConfig{{URL: "discord://1/abc", Mention: " <@!123456789012345678> "}}
	Normalize(tidy)
	if tidy.Notifications[0].Mention != "<@123456789012345678>" {
		t.Errorf("Normalize left mention = %q, want the canonical <@id> form", tidy.Notifications[0].Mention)
	}
}

// TestNotificationKeysRoundTripThroughSave is the end-to-end encoding proof:
// Save runs Validate and refuses a failing config, and the three-way
// mention_events state has to survive a real file. The middle target is the
// one that matters — with a plain []string it would come back as the default
// list.
func TestNotificationKeysRoundTripThroughSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	never := []string{}
	some := []string{"finished"}
	cfg := Defaults()
	cfg.Network.PublicURL = "https://moombox.example.com"
	cfg.Notifications = []NotificationConfig{
		{URL: "discord://1/aaa", Mention: "<@&123456789012345678>"},
		{URL: "discord://2/bbb", Mention: "@here", MentionEvents: &never},
		{URL: "discord://3/ccc", Enabled: boolPtr(false), MentionEvents: &some, Mention: "@everyone"},
	}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "mention_events = []") {
		t.Errorf("the explicitly-empty mention_events was not written; file:\n%s", raw)
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.Network.PublicURL != "https://moombox.example.com" {
		t.Errorf("public_url round-tripped as %q", back.Network.PublicURL)
	}
	if back.Notifications[0].MentionEvents != nil {
		t.Error("target 0 gained a mention_events key it never had")
	}
	if e := back.Notifications[1].MentionEvents; e == nil || len(*e) != 0 {
		t.Errorf("target 1's explicit \"never\" came back as %v — the three-way encoding is broken", e)
	}
	if back.Notifications[2].IsEnabled() {
		t.Error("target 2's enabled = false did not survive the round trip")
	}
}

// TestPublicURLIssueCarriesNoUserinfo pins the userinfo half of the secrets
// sweep: a hand-edited network.public_url = "https://user:password@host" is
// refused for its userinfo, Load records the refusal in NormalizedOnLoad, and
// the boot logs every entry there as a Warn (cmd/moombox/services.go) — with
// the value quoted back whole, password and all. A value url.Parse refuses
// was quoted a second time inside its parse error. The same error text is
// the settings API's field error and the TUI form's message.
//
// Mutants (run):
//   - validateOrNormalize quoting cfg.Network.PublicURL instead of
//     redact.URLUserinfo(...): both rows' boot issue carries the password.
//   - ValidatePublicURL wrapping url.Parse's *url.Error whole: the
//     "unparseable" row's issue and error carry it (inside the parse error).
func TestPublicURLIssueCarriesNoUserinfo(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"refused for its userinfo", "https://moombox:hunter2SECRET@dash.example.com/moombox"},
		{"unparseable", "https://moombox:hunter2SECRET@dash example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("[network]\npublic_url = \""+tc.value+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			var issue string
			for _, s := range cfg.NormalizedOnLoad {
				if strings.Contains(s, "network.public_url") {
					issue = s
				}
			}
			if issue == "" {
				t.Fatalf("no network.public_url entry in NormalizedOnLoad %q", cfg.NormalizedOnLoad)
			}
			if strings.Contains(issue, "SECRET") {
				t.Errorf("the boot Warn's issue carries the password: %q", issue)
			}
			if !strings.Contains(issue, "<redacted>@dash") {
				t.Errorf("issue = %q, want the value quoted with its userinfo cut", issue)
			}
			if cfg.Network.PublicURL != "" {
				t.Errorf("public_url = %q, want it cleared", cfg.Network.PublicURL)
			}
			if _, err := ValidatePublicURL(tc.value); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Errorf("ValidatePublicURL(%q) = %v, want an error that carries no password", tc.value, err)
			}
		})
	}
}
