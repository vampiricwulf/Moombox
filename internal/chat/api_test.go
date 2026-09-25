package chat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseMessageRunsTextOnly(t *testing.T) {
	msg := map[string]any{
		"runs": []any{
			map[string]any{"text": "hello "},
			map[string]any{"text": "world"},
		},
	}
	parts := parseMessageRuns(msg)
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(parts))
	}
	for i, want := range []string{"hello ", "world"} {
		if parts[i].Type != "text" || parts[i].Text != want {
			t.Errorf("parts[%d] = %+v, want Type=text Text=%q", i, parts[i], want)
		}
	}
}

func TestParseMessageRunsHyperlink(t *testing.T) {
	// A text run with a direct urlEndpoint link.
	msg := map[string]any{
		"runs": []any{
			map[string]any{
				"text": "https://example.com/page",
				"navigationEndpoint": map[string]any{
					"urlEndpoint": map[string]any{
						"url": "https://example.com/page",
					},
				},
			},
		},
	}
	parts := parseMessageRuns(msg)
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	if parts[0].URL != "https://example.com/page" {
		t.Errorf("URL: want %q, got %q", "https://example.com/page", parts[0].URL)
	}
}

func TestParseMessageRunsHyperlinkRedirectFallback(t *testing.T) {
	// When urlEndpoint is absent, fall back to commandMetadata (YouTube's
	// redirect-wrapped variant, still usable for display/analytics).
	msg := map[string]any{
		"runs": []any{
			map[string]any{
				"text": "click here",
				"navigationEndpoint": map[string]any{
					"commandMetadata": map[string]any{
						"webCommandMetadata": map[string]any{
							"url": "https://www.youtube.com/redirect?q=https%3A%2F%2Fexample.com",
						},
					},
				},
			},
		},
	}
	parts := parseMessageRuns(msg)
	if len(parts) != 1 || parts[0].URL == "" {
		t.Fatalf("expected URL extracted from commandMetadata fallback, got %+v", parts)
	}
}

func TestParseMessageRunsBoldItalic(t *testing.T) {
	msg := map[string]any{
		"runs": []any{
			map[string]any{"text": "plain"},
			map[string]any{"text": "bold", "bold": true},
			map[string]any{"text": "italic", "italics": true},
			map[string]any{"text": "both", "bold": true, "italics": true},
		},
	}
	parts := parseMessageRuns(msg)
	if len(parts) != 4 {
		t.Fatalf("want 4 parts, got %d", len(parts))
	}
	want := []struct {
		text   string
		bold   bool
		italic bool
	}{
		{"plain", false, false},
		{"bold", true, false},
		{"italic", false, true},
		{"both", true, true},
	}
	for i, w := range want {
		if parts[i].Text != w.text || parts[i].Bold != w.bold || parts[i].Italic != w.italic {
			t.Errorf("parts[%d] = %+v, want Text=%q Bold=%v Italic=%v",
				i, parts[i], w.text, w.bold, w.italic)
		}
	}
}

func TestParseMessageRunsEmojiSurvivesE5Changes(t *testing.T) {
	// Regression: emoji runs must keep working after the E5 changes.
	msg := map[string]any{
		"runs": []any{
			map[string]any{
				"emoji": map[string]any{
					"emojiId":       "abc123",
					"isCustomEmoji": true,
					"image": map[string]any{
						"thumbnails": []any{
							map[string]any{"url": "https://example/small.png", "width": float64(24), "height": float64(24)},
							map[string]any{"url": "https://example/large.png", "width": float64(48), "height": float64(48)},
						},
					},
				},
			},
		},
	}
	parts := parseMessageRuns(msg)
	if len(parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(parts))
	}
	if parts[0].Type != "emoji" || parts[0].EmojiID != "abc123" {
		t.Errorf("unexpected part: %+v", parts[0])
	}
	if parts[0].EmojiURL != "https://example/large.png" {
		t.Errorf("expected largest thumbnail, got %q", parts[0].EmojiURL)
	}
	if !parts[0].IsCustomEmoji {
		t.Error("IsCustomEmoji lost")
	}
}

func TestParseMessageRunsEmptyRuns(t *testing.T) {
	msg := map[string]any{"runs": []any{}}
	parts := parseMessageRuns(msg)
	if len(parts) != 0 {
		t.Errorf("expected empty parts, got %+v", parts)
	}
}

func TestParseMessageRunsMissingRunsKey(t *testing.T) {
	// Malformed message without "runs" key shouldn't panic.
	msg := map[string]any{}
	parts := parseMessageRuns(msg)
	if len(parts) != 0 {
		t.Errorf("expected empty parts on missing 'runs', got %+v", parts)
	}
}

func TestExtractNavURLPreferUrlEndpoint(t *testing.T) {
	nav := map[string]any{
		"urlEndpoint": map[string]any{"url": "https://direct.example"},
		"commandMetadata": map[string]any{
			"webCommandMetadata": map[string]any{"url": "https://redirect.example"},
		},
	}
	if got := extractNavURL(nav); got != "https://direct.example" {
		t.Errorf("expected urlEndpoint to win, got %q", got)
	}
}

func TestExtractNavURLFallsBackToCommandMetadata(t *testing.T) {
	nav := map[string]any{
		"commandMetadata": map[string]any{
			"webCommandMetadata": map[string]any{"url": "https://fallback.example"},
		},
	}
	if got := extractNavURL(nav); got != "https://fallback.example" {
		t.Errorf("expected commandMetadata fallback, got %q", got)
	}
}

func TestExtractNavURLEmptyReturnsEmpty(t *testing.T) {
	if got := extractNavURL(map[string]any{}); got != "" {
		t.Errorf("expected empty string for empty nav, got %q", got)
	}
}

// --- T5 authentication fast-fail ---

func TestFetchChatReturnsErrAuthRequiredOn401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	api := NewChatAPI("", "", nil)
	_, err := api.fetchChat(context.Background(), server.URL, "some-continuation-token")
	if err == nil {
		t.Fatal("expected error from 401 response, got nil")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("expected errors.Is(err, ErrAuthRequired), got %v", err)
	}
}

func TestFetchChatOtherStatusDoesNotMapToAuth(t *testing.T) {
	// A 500 must NOT be misclassified as auth failure — that would trigger
	// the loop's fast-fail path on transient server errors.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	api := NewChatAPI("", "", nil)
	_, err := api.fetchChat(context.Background(), server.URL, "some-continuation-token")
	if err == nil {
		t.Fatal("expected error from 500 response, got nil")
	}
	if errors.Is(err, ErrAuthRequired) {
		t.Errorf("500 was misclassified as auth failure: %v", err)
	}
}

// --- Q2 parseResponse sub-extractor tests ---

func TestExtractNextContinuationFindsToken(t *testing.T) {
	liveChatCont := map[string]any{
		"continuations": []any{
			map[string]any{
				"timedContinuationData": map[string]any{
					"continuation": "next-token",
					"timeoutMs":    float64(5000),
				},
			},
		},
	}
	token, timeout := extractNextContinuation(liveChatCont, -1)
	if token != "next-token" {
		t.Errorf("token: want %q, got %q", "next-token", token)
	}
	if timeout != 5000 {
		t.Errorf("timeout: want 5000, got %d", timeout)
	}
}

func TestExtractNextContinuationEmptyArrayReturnsDefault(t *testing.T) {
	liveChatCont := map[string]any{"continuations": []any{}}
	token, timeout := extractNextContinuation(liveChatCont, -1)
	if token != "" {
		t.Errorf("expected empty token, got %q", token)
	}
	if timeout != -1 {
		t.Errorf("expected default timeout=-1, got %d", timeout)
	}
}

func TestExtractNextContinuationStopsAtFirstToken(t *testing.T) {
	liveChatCont := map[string]any{
		"continuations": []any{
			map[string]any{
				"timedContinuationData": map[string]any{
					"continuation": "first",
					"timeoutMs":    float64(1000),
				},
			},
			map[string]any{
				"timedContinuationData": map[string]any{
					"continuation": "second",
					"timeoutMs":    float64(2000),
				},
			},
		},
	}
	token, timeout := extractNextContinuation(liveChatCont, -1)
	if token != "first" || timeout != 1000 {
		t.Errorf("expected first token/timeout, got %q/%d", token, timeout)
	}
}

func TestSelectRendererPicksFirstMatch(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"text", "liveChatTextMessageRenderer"},
		{"paid", "liveChatPaidMessageRenderer"},
		{"sticker", "liveChatPaidStickerRenderer"},
		{"membership", "liveChatMembershipItemRenderer"},
		{"gift purchase", "liveChatSponsorshipsGiftPurchaseAnnouncementRenderer"},
		{"gift redemption", "liveChatSponsorshipsGiftRedemptionAnnouncementRenderer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := map[string]any{tt.key: map[string]any{"id": "x"}}
			r := selectRenderer(item)
			if r == nil {
				t.Fatalf("expected non-nil renderer for %s", tt.key)
			}
			if id, _ := r["id"].(string); id != "x" {
				t.Errorf("wrong renderer selected: %v", r)
			}
		})
	}
}

func TestSelectRendererReturnsNilForUnknown(t *testing.T) {
	item := map[string]any{"liveChatFutureRenderer": map[string]any{"id": "x"}}
	if r := selectRenderer(item); r != nil {
		t.Errorf("expected nil for unknown renderer type, got %v", r)
	}
}

func TestExtractAllChatContinuationHappyPath(t *testing.T) {
	header := map[string]any{
		"liveChatHeaderRenderer": map[string]any{
			"viewSelector": map[string]any{
				"sortFilterSubMenuRenderer": map[string]any{
					"subMenuItems": []any{
						map[string]any{"title": "Top Chat"},
						map[string]any{
							"continuation": map[string]any{
								"reloadContinuationData": map[string]any{
									"continuation": "all-chat-token",
								},
							},
						},
					},
				},
			},
		},
	}
	token := extractAllChatContinuation(header)
	if token != "all-chat-token" {
		t.Errorf("expected all-chat-token, got %q", token)
	}
}

func TestExtractAllChatContinuationMissingSecondItem(t *testing.T) {
	header := map[string]any{
		"liveChatHeaderRenderer": map[string]any{
			"viewSelector": map[string]any{
				"sortFilterSubMenuRenderer": map[string]any{
					"subMenuItems": []any{map[string]any{"title": "Top Chat"}},
				},
			},
		},
	}
	if token := extractAllChatContinuation(header); token != "" {
		t.Errorf("expected empty on missing subMenuItems[1], got %q", token)
	}
}

func TestExtractReplayOffsetStringForm(t *testing.T) {
	api := &ChatAPI{}
	raw := map[string]any{"videoOffsetTimeMsec": "12345"}
	offset, has := api.extractReplayOffset(raw)
	if !has || offset != 12345 {
		t.Errorf("string form: want 12345/true, got %d/%v", offset, has)
	}
}

func TestExtractReplayOffsetFloatForm(t *testing.T) {
	api := &ChatAPI{}
	raw := map[string]any{"videoOffsetTimeMsec": float64(9876)}
	offset, has := api.extractReplayOffset(raw)
	if !has || offset != 9876 {
		t.Errorf("float form: want 9876/true, got %d/%v", offset, has)
	}
}

func TestExtractReplayOffsetMissing(t *testing.T) {
	api := &ChatAPI{}
	raw := map[string]any{}
	if _, has := api.extractReplayOffset(raw); has {
		t.Error("missing field should return has=false")
	}
}

func TestExtractReplayOffsetUnparseableString(t *testing.T) {
	api := &ChatAPI{}
	raw := map[string]any{"videoOffsetTimeMsec": "not-a-number"}
	if _, has := api.extractReplayOffset(raw); has {
		t.Error("unparseable string should return has=false")
	}
}

func TestParseNegativeTimestampText(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"-0:05", -5_000, true},
		{"-1:23", -83_000, true},
		{"-1:02:03", -3_723_000, true},
		{"0:05", 0, false}, // not negative — not our business
		{"-5", 0, false},   // no colon — not a relative time
		{"-x:y", 0, false},
		{"", 0, false},
		{"-1:02:03:04", 0, false}, // 4 components — only M:SS / H:MM:SS are valid
		{"-1:-5", 0, false},       // per-component n < 0 guard
	}
	for _, c := range cases {
		got, ok := parseNegativeTimestampText(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseNegativeTimestampText(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func replayAction(offset any, timestampText string) map[string]any {
	renderer := map[string]any{
		"id":            "pre1",
		"timestampUsec": "1700000000000000",
		"message":       map[string]any{"runs": []any{map[string]any{"text": "hi"}}},
		"authorName":    map[string]any{"simpleText": "U"},
	}
	if timestampText != "" {
		renderer["timestampText"] = map[string]any{"simpleText": timestampText}
	}
	return map[string]any{
		"replayChatItemAction": map[string]any{
			"videoOffsetTimeMsec": offset,
			"actions": []any{map[string]any{
				"addChatItemAction": map[string]any{
					"item": map[string]any{"liveChatTextMessageRenderer": renderer},
				},
			}},
		},
	}
}

// YouTube reports videoOffsetTimeMsec = 0 for every message sent before the
// stream started and keeps the real relative time only in timestampText
// ("-2:30"). Recover it so waiting-room chat is not piled onto 0:00.
func TestParseActionReplayZeroOffsetRecoversNegativeFromTimestampText(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(replayAction("0", "-2:30"))
	if msg == nil {
		t.Fatal("parseAction returned nil")
	}
	if !msg.HasOffset || msg.OffsetMs != -150_000 {
		t.Errorf("OffsetMs = %d (hasOffset %v), want -150000", msg.OffsetMs, msg.HasOffset)
	}
}

func TestParseActionReplayZeroOffsetWithoutMinusStaysZero(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(replayAction("0", "0:00"))
	if msg == nil || !msg.HasOffset || msg.OffsetMs != 0 {
		t.Fatalf("want offset 0 with hasOffset, got %+v", msg)
	}
}

func TestParseActionReplayPositiveOffsetIgnoresTimestampText(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(replayAction("12345", "-2:30"))
	if msg == nil || msg.OffsetMs != 12345 {
		t.Fatalf("a non-zero replay offset is authoritative; got %+v", msg)
	}
}

// membershipItem builds a liveChatMembershipItemRenderer action in the shape
// YouTube ships. A NEW member carries headerSubtext and no message at all; a
// MILESTONE carries headerPrimaryText ("Member for 6 months"), a headerSubtext
// that is only the tier name, and the member's own message. Confirmed against
// youtubei.js' LiveChatMembershipItem (header_primary_text optional,
// header_subtext always, message optional).
func membershipItem(primary, subtext, message string) map[string]any {
	r := map[string]any{
		"id":            "memb-1",
		"timestampUsec": "1700000000000000",
		"authorName":    map[string]any{"simpleText": "newfan"},
	}
	if primary != "" {
		r["headerPrimaryText"] = map[string]any{"runs": []any{
			map[string]any{"text": primary},
		}}
	}
	if subtext != "" {
		r["headerSubtext"] = map[string]any{"simpleText": subtext}
	}
	if message != "" {
		r["message"] = map[string]any{"runs": []any{map[string]any{"text": message}}}
	}
	return map[string]any{"addChatItemAction": map[string]any{
		"item": map[string]any{"liveChatMembershipItemRenderer": r},
	}}
}

// TestMembershipHeaderTextIsArchived: until this change parseAction tested for
// the membership renderer's PRESENCE and threw it away, so a new-member event
// archived as a message with an empty message array and no text anywhere — the
// sidebar had a timestamp and a name to render and nothing else. The header
// line is the event.
//
// Mutants this kills: dropping the MembershipText assignment (every want below
// comes back ""); reading headerSubtext first (the milestone row reports
// "Member" instead of "Member for 6 months"); folding the line into Message
// instead (the message assertions fail, and with them the overlay-silence
// guarantee K4 rests on).
func TestMembershipHeaderTextIsArchived(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	cases := []struct {
		name        string
		primary     string
		subtext     string
		message     string
		wantText    string
		wantMessage string
	}{
		{"new member", "", "Welcome to Member!", "", "Welcome to Member!", ""},
		{"milestone", "Member for 6 months", "Member", "thanks!", "Member for 6 months", "thanks!"},
		{"milestone with no message", "Member for 2 months", "Member", "", "Member for 2 months", ""},
		{"neither header", "", "", "just talking", "", "just talking"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := api.parseAction(membershipItem(c.primary, c.subtext, c.message))
			if msg == nil {
				t.Fatal("parseAction returned nil for a membership item")
			}
			if !msg.IsMembership {
				t.Error("IsMembership must stay set")
			}
			if msg.MembershipText != c.wantText {
				t.Errorf("MembershipText = %q, want %q", msg.MembershipText, c.wantText)
			}
			var gotMessage string
			for _, p := range msg.Message {
				gotMessage += p.Text
			}
			if gotMessage != c.wantMessage {
				t.Errorf("Message = %q, want %q — the header line must NOT be folded into it", gotMessage, c.wantMessage)
			}
		})
	}
}

// TestRenderedTextReadsBothTextShapes: YouTube writes a text field either as
// {"simpleText": …} or as {"runs": [{"text": …}, …]} and uses both within one
// membership renderer, so the reader has to take either. Emoji runs (which
// carry no "text") contribute nothing rather than an "undefined".
//
// Mutants this kills: handling only simpleText (the runs rows come back "");
// handling only runs (the simpleText row comes back ""); concatenating a run's
// whole map rather than its text.
func TestRenderedTextReadsBothTextShapes(t *testing.T) {
	cases := []struct {
		name  string
		field any
		want  string
	}{
		{"simple", map[string]any{"simpleText": "Welcome!"}, "Welcome!"},
		{"runs", map[string]any{"runs": []any{
			map[string]any{"text": "Member for "},
			map[string]any{"text": "6"},
			map[string]any{"text": " months"},
		}}, "Member for 6 months"},
		{"simple wins over empty runs", map[string]any{
			"simpleText": "Welcome!", "runs": []any{},
		}, "Welcome!"},
		{"a run with no text contributes nothing", map[string]any{"runs": []any{
			map[string]any{"text": "a"},
			map[string]any{"emoji": map[string]any{"emojiId": "x"}},
			map[string]any{"text": "b"},
		}}, "ab"},
		{"absent", nil, ""},
		{"wrong type", "Welcome!", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderedText(c.field); got != c.want {
				t.Errorf("renderedText = %q, want %q", got, c.want)
			}
		})
	}
}

// giftPurchaseItem builds a liveChatSponsorshipsGiftPurchaseAnnouncementRenderer
// in the shape YouTube ships: the id, timestampUsec and
// authorExternalChannelId at the top, and the author, badges and primaryText
// ("Gifted 5 memberships") one level down in
// header.liveChatSponsorshipsHeaderRenderer. There is no timestampText.
func giftPurchaseItem() map[string]any {
	return map[string]any{"addChatItemAction": map[string]any{
		"item": map[string]any{
			"liveChatSponsorshipsGiftPurchaseAnnouncementRenderer": map[string]any{
				"id":                      "gift-1",
				"timestampUsec":           "1700000000000000",
				"authorExternalChannelId": "UCgifter",
				"header": map[string]any{
					"liveChatSponsorshipsHeaderRenderer": map[string]any{
						"authorName": map[string]any{"simpleText": "gifter"},
						"authorBadges": []any{map[string]any{
							"liveChatAuthorBadgeRenderer": map[string]any{
								"tooltip": "Moderator",
								"icon":    map[string]any{"iconType": "MODERATOR"},
							},
						}},
						"primaryText": map[string]any{"runs": []any{
							map[string]any{"text": "Gifted "},
							map[string]any{"text": "5"},
							map[string]any{"text": " memberships"},
						}},
					},
				},
			},
		},
	}}
}

// giftRedemptionItem builds the recipient's side: flat, with the line in
// `message` and the author where parseMessageRenderer already looks.
func giftRedemptionItem() map[string]any {
	return map[string]any{"addChatItemAction": map[string]any{
		"item": map[string]any{
			"liveChatSponsorshipsGiftRedemptionAnnouncementRenderer": map[string]any{
				"id":            "redeem-1",
				"timestampUsec": "1700000000000000",
				"timestampText": map[string]any{"simpleText": "1:02:03"},
				"authorName":    map[string]any{"simpleText": "lucky"},
				"message": map[string]any{"runs": []any{
					map[string]any{"text": "was gifted a membership by gifter"},
				}},
			},
		},
	}}
}

// TestGiftPurchaseIsArchivedWithItsHeaderFields: the purchase renderer was not
// in selectRenderer's roster at all, so every gifted membership Moombox has
// ever archived is missing. Adding the key alone is not enough — the author,
// the badges and the line all live one level down, so a raw read yields
// author "Unknown" and no text.
//
// Mutants this kills: adding the roster key without the hoist (AuthorName
// comes back "Unknown" and MembershipText ""); hoisting authorName but not
// authorBadges (the moderator badge is gone); losing id or timestampUsec in
// the hoist (the ID falls back to a random one and the offset arithmetic loses
// its clock).
func TestGiftPurchaseIsArchivedWithItsHeaderFields(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(giftPurchaseItem())
	if msg == nil {
		t.Fatal("a gift purchase announcement was dropped")
	}
	if !msg.IsMembership {
		t.Error("a gift purchase is a membership event")
	}
	if msg.MembershipText != "Gifted 5 memberships" {
		t.Errorf("MembershipText = %q, want %q", msg.MembershipText, "Gifted 5 memberships")
	}
	if msg.AuthorName != "gifter" {
		t.Errorf("AuthorName = %q, want %q — the name is in the nested header", msg.AuthorName, "gifter")
	}
	if msg.ID != "gift-1" {
		t.Errorf("ID = %q, want gift-1 — the hoist must keep the outer id", msg.ID)
	}
	if msg.TimestampUsec != "1700000000000000" {
		t.Errorf("TimestampUsec = %q, want the outer one", msg.TimestampUsec)
	}
	if msg.AuthorChannelID != "UCgifter" {
		t.Errorf("AuthorChannelID = %q, want UCgifter", msg.AuthorChannelID)
	}
	var hasMod bool
	for _, b := range msg.AuthorBadges {
		if b == "moderator" {
			hasMod = true
		}
	}
	if !hasMod {
		t.Errorf("AuthorBadges = %v, want the nested header's moderator badge", msg.AuthorBadges)
	}
	if len(msg.Message) != 0 {
		t.Errorf("Message = %v, want empty — a purchase has no typed message", msg.Message)
	}
}

// TestGiftRedemptionIsArchived: the recipient's side is flat, so the roster
// entry is the whole fix — its line is in `message`, which parseMessageRenderer
// already reads, and it has no header text of its own.
//
// Mutant this kills: dropping the roster key (parseAction returns nil and the
// event is invisible); routing it through the purchase hoist (author and
// message both come back empty).
func TestGiftRedemptionIsArchived(t *testing.T) {
	api := NewChatAPI("k", "", nil)
	msg := api.parseAction(giftRedemptionItem())
	if msg == nil {
		t.Fatal("a gift redemption announcement was dropped")
	}
	if !msg.IsMembership {
		t.Error("a gift redemption is a membership event")
	}
	if msg.AuthorName != "lucky" {
		t.Errorf("AuthorName = %q, want lucky", msg.AuthorName)
	}
	if msg.MembershipText != "" {
		t.Errorf("MembershipText = %q, want empty — the redemption's line is its message", msg.MembershipText)
	}
	var got string
	for _, p := range msg.Message {
		got += p.Text
	}
	if got != "was gifted a membership by gifter" {
		t.Errorf("Message = %q, want the redemption line", got)
	}
}
