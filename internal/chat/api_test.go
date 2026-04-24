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

	api := NewChatAPI("", "", "")
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

	api := NewChatAPI("", "", "")
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
