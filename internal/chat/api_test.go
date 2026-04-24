package chat

import "testing"

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
