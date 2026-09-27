package notifications

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodePayload unmarshals what buildPayload produced, so every assertion is
// against the JSON Discord actually receives rather than against the Go
// structs. A field that loses its tag, or gains an `omitempty` it should not
// have, is invisible to a struct-level check and obvious here.
func decodePayload(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, body)
	}
	return got
}

func firstEmbed(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	embeds, ok := p["embeds"].([]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("want exactly 1 embed, got %v", p["embeds"])
	}
	e, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("embed is not an object: %v", embeds[0])
	}
	return e
}

// TestPayloadCarriesTheAuthorBlock pins the channel identity an operator reads
// first. Job.ChannelAvatarURL has existed since the rewrite and reached no
// embed: discordEmbed had no author field at all, so every job notification
// identified its channel only inside a "Channel" field halfway down.
//
// THE MUTANT: dropping the `author` key, or serialising it when Name is empty
// (Discord rejects an author object with no name — a permanent 400).
func TestPayloadCarriesTheAuthorBlock(t *testing.T) {
	body, err := buildPayload(One("T", "D", 0x1, nil, SendOptions{
		Author: &Author{
			Name:    "Some Channel",
			IconURL: "https://example.invalid/avatar.jpg",
			URL:     "https://example.invalid/channel",
		},
	}))
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	e := firstEmbed(t, decodePayload(t, body))
	author, ok := e["author"].(map[string]any)
	if !ok {
		t.Fatalf("no author object in the embed: %v", e)
	}
	if author["name"] != "Some Channel" {
		t.Errorf("author.name = %v, want %q", author["name"], "Some Channel")
	}
	if author["icon_url"] != "https://example.invalid/avatar.jpg" {
		t.Errorf("author.icon_url = %v", author["icon_url"])
	}
	if author["url"] != "https://example.invalid/channel" {
		t.Errorf("author.url = %v", author["url"])
	}

	t.Run("an author with no name is omitted entirely", func(t *testing.T) {
		body, err := buildPayload(One("T", "D", 0x1, nil, SendOptions{Author: &Author{IconURL: "https://example.invalid/a.jpg"}}))
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		if _, present := firstEmbed(t, decodePayload(t, body))["author"]; present {
			t.Error("a nameless author was serialised — Discord rejects it with a permanent 400")
		}
	})
}

// TestFooterNamesThePlatformAndJob pins the footer contract. "Moombox Go" was
// a rewrite-era suffix that told an operator nothing; with several channels and
// several jobs in one Discord channel, the platform and the job id are what
// make an embed identifiable at a glance — and JobID is the key Arc N3's
// edit-in-place mode reads.
func TestFooterNamesThePlatformAndJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts SendOptions
		want string
	}{
		{"neither", SendOptions{}, "Moombox"},
		{"platform and job", SendOptions{Platform: "youtube", JobID: "yt_abc"}, "Moombox · youtube · yt_abc"},
		{"platform only", SendOptions{Platform: "twitch"}, "Moombox · twitch"},
		{"job only", SendOptions{JobID: "yt_abc"}, "Moombox · yt_abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := buildPayload(One("T", "D", 0x1, nil, tc.opts))
			if err != nil {
				t.Fatalf("buildPayload: %v", err)
			}
			footer, ok := firstEmbed(t, decodePayload(t, body))["footer"].(map[string]any)
			if !ok {
				t.Fatalf("no footer in the embed")
			}
			if footer["text"] != tc.want {
				t.Errorf("footer.text = %v, want %q", footer["text"], tc.want)
			}
		})
	}
}

// TestMentionPayloadShape pins what N2b will drive. Per the Discord API docs
// (resources/message.mdx) allowed_mentions governs "mentions in the message
// content, or components" — embeds never mention on their own — and the webhook
// default is {"parse": ["users"]}. So a role ping needs BOTH content
// "<@&id>" AND allowed_mentions {roles: [id]}, and an empty parse list is what
// stops the default from silently widening a role ping into a user ping.
//
// THE MUTANT: setting content without allowed_mentions (the ping silently does
// nothing for a role), or leaving parse unset (the default re-enters).
func TestMentionPayloadShape(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mention   string
		wantRoles []any
		wantUsers []any
		wantParse []any
	}{
		{"role", "<@&123456789012345678>", []any{"123456789012345678"}, nil, []any{}},
		{"user", "<@987654321098765432>", nil, []any{"987654321098765432"}, []any{}},
		{"nickname user form", "<@!987654321098765432>", nil, []any{"987654321098765432"}, []any{}},
		{"everyone", "@everyone", nil, nil, []any{"everyone"}},
		{"here", "@here", nil, nil, []any{"everyone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := One("T", "D", 0x1, nil, SendOptions{})
			msg.Mention, msg.MentionAllowed = tc.mention, MentionParse(tc.mention)
			body, err := buildPayload(msg)
			if err != nil {
				t.Fatalf("buildPayload: %v", err)
			}
			p := decodePayload(t, body)
			if p["content"] != tc.mention {
				t.Errorf("content = %v, want %q", p["content"], tc.mention)
			}
			am, ok := p["allowed_mentions"].(map[string]any)
			if !ok {
				t.Fatalf("no allowed_mentions: %v", p)
			}
			eq := func(key string, want []any) {
				got, present := am[key]
				if want == nil {
					if present {
						t.Errorf("allowed_mentions.%s = %v, want absent", key, got)
					}
					return
				}
				gotList, _ := got.([]any)
				if len(gotList) != len(want) {
					t.Errorf("allowed_mentions.%s = %v, want %v", key, got, want)
					return
				}
				for i := range want {
					if gotList[i] != want[i] {
						t.Errorf("allowed_mentions.%s = %v, want %v", key, got, want)
						return
					}
				}
			}
			eq("roles", tc.wantRoles)
			eq("users", tc.wantUsers)
			if _, present := am["parse"]; !present {
				t.Errorf("allowed_mentions has no parse key — the webhook default {\"parse\":[\"users\"]} re-enters")
			}
			eq("parse", tc.wantParse)
		})
	}

	t.Run("no mention means no content and no allowed_mentions", func(t *testing.T) {
		body, err := buildPayload(One("T", "D", 0x1, nil, SendOptions{}))
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		p := decodePayload(t, body)
		if _, present := p["content"]; present {
			t.Errorf("content was serialised for a send with no mention: %v", p["content"])
		}
		if _, present := p["allowed_mentions"]; present {
			t.Errorf("allowed_mentions was serialised for a send with no mention")
		}
	})

	t.Run("a mention the target is not allowed for is not sent", func(t *testing.T) {
		msg := One("T", "D", 0x1, nil, SendOptions{})
		msg.Mention, msg.MentionAllowed = "@everyone", nil
		body, err := buildPayload(msg)
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		if _, present := decodePayload(t, body)["content"]; present {
			t.Error("a nil MentionAllowed still pinged — it is the per-event gate N2b sets from mention_events and it must be honoured here")
		}
	})

	// MentionParse is where the "unrecognised form is dropped" rule lives now
	// that the object travels on the Message: N2b resolves the configured text
	// through it, so a config string that is not a mention must resolve to nil
	// rather than to an unrestricted ping.
	t.Run("MentionParse rejects everything that is not a mention form", func(t *testing.T) {
		for _, junk := range []string{"", "everyone", "@chan", "<@&>", "<@>", "<@!>", "<#123>", "please ping <@&1>"} {
			if got := MentionParse(junk); got != nil {
				t.Errorf("MentionParse(%q) = %+v, want nil", junk, got)
			}
		}
		if got := MentionParse("<@&123>"); got == nil || len(got.Roles) != 1 || got.Roles[0] != "123" {
			t.Errorf("MentionParse(\"<@&123>\") = %+v, want Roles [123]", got)
		}
	})
}

// TestEffectiveTierDerivesFromTheEvent pins the overflow policy's input. A
// backfill sweep produces hundreds of `found`; an `error` must never lose its
// place in the queue to one. An explicit Tier wins so a caller can promote a
// normally-low event.
func TestEffectiveTierDerivesFromTheEvent(t *testing.T) {
	for _, tc := range []struct {
		opts SendOptions
		want Tier
	}{
		{SendOptions{Event: "found"}, TierLow},
		{SendOptions{Event: "added"}, TierLow},
		{SendOptions{Event: "scheduled"}, TierLow},
		{SendOptions{Event: "rescheduled"}, TierLow},
		{SendOptions{Event: "error"}, TierNormal},
		{SendOptions{Event: "auth"}, TierNormal},
		{SendOptions{Event: "disk_critical"}, TierNormal},
		{SendOptions{Event: "finished"}, TierNormal},
		{SendOptions{}, TierNormal},
		{SendOptions{Event: "found", Tier: TierNormal}, TierNormal},
		{SendOptions{Event: "error", Tier: TierLow}, TierLow},
	} {
		if got := effectiveTier(tc.opts); got != tc.want {
			t.Errorf("effectiveTier(event=%q, tier=%v) = %v, want %v", tc.opts.Event, tc.opts.Tier, got, tc.want)
		}
	}
}

// TestSendOptionsStillCarriesTheOldKeys guards against a rename sweeping away
// a field 36 producers set.
func TestSendOptionsStillCarriesTheOldKeys(t *testing.T) {
	body, err := buildPayload(One("T", "D", 0x1, nil, SendOptions{
		URL:       "https://example.invalid/watch",
		Thumbnail: "https://example.invalid/t.jpg",
		Image:     "https://example.invalid/i.jpg",
	}))
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	e := firstEmbed(t, decodePayload(t, body))
	if e["url"] != "https://example.invalid/watch" {
		t.Errorf("url = %v", e["url"])
	}
	for _, key := range []string{"thumbnail", "image"} {
		obj, ok := e[key].(map[string]any)
		if !ok || !strings.HasPrefix(obj["url"].(string), "https://example.invalid/") {
			t.Errorf("%s = %v", key, e[key])
		}
	}
}
