package utils

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestExtractVideoID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		// Bare ID
		{"dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Standard watch URLs
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"http://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://m.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// With extra params
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=30s", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLtest", "dQw4w9WgXcQ"},
		// Short URLs
		{"https://youtu.be/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"http://youtu.be/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Live URLs
		{"https://www.youtube.com/live/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Shorts URLs
		{"https://www.youtube.com/shorts/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// Embed URLs
		{"https://www.youtube.com/embed/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// V URLs
		{"https://www.youtube.com/v/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		// IDs with hyphens and underscores (must be exactly 11 chars)
		{"abc-_12-_AB", "abc-_12-_AB"},
		// Invalid inputs
		{"", ""},
		{"not-a-url", ""},
		{"https://example.com/watch?v=dQw4w9WgXcQ", ""},
		{"https://www.youtube.com/watch", ""},
		{"https://www.youtube.com/watch?v=tooshort", ""},
		{"https://www.youtube.com/watch?v=toolooooooong", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ExtractVideoID(tt.input)
			if result != tt.expected {
				t.Errorf("ExtractVideoID(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestParseYouTubeChannelURL(t *testing.T) {
	tests := []struct {
		input    string
		wantNil  bool
		wantID   string // expected ChannelID (direct extraction)
		wantPath string // expected Path (needs resolution)
	}{
		// /channel/UCxxx → direct extraction
		{"https://www.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		{"https://youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		{"https://m.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		{"www.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},
		// /channel/UCxxx with trailing path
		{"https://www.youtube.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx/videos", false, "UCxxxxxxxxxxxxxxxxxxxxxx", ""},

		// /@Handle → needs resolution
		{"https://www.youtube.com/@SomeHandle", false, "", "/@SomeHandle"},
		{"youtube.com/@SomeHandle", false, "", "/@SomeHandle"},
		{"https://www.youtube.com/@SomeHandle/videos", false, "", "/@SomeHandle"},

		// /c/CustomName → needs resolution
		{"https://www.youtube.com/c/CustomChannel", false, "", "/c/CustomChannel"},
		{"https://www.youtube.com/c/CustomChannel/live", false, "", "/c/CustomChannel"},

		// /user/Username → needs resolution
		{"https://www.youtube.com/user/SomeUser", false, "", "/user/SomeUser"},
		{"https://www.youtube.com/user/SomeUser/videos", false, "", "/user/SomeUser"},

		// Video URLs → nil (not channel URLs)
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", true, "", ""},
		{"https://www.youtube.com/live/dQw4w9WgXcQ", true, "", ""},
		{"https://youtu.be/dQw4w9WgXcQ", true, "", ""},

		// Non-YouTube URLs → nil
		{"https://example.com/channel/UCxxxxxxxxxxxxxxxxxxxxxx", true, "", ""},
		{"https://twitch.tv/streamer", true, "", ""},

		// Bare strings → nil
		{"UCxxxxxxxxxxxxxxxxxxxxxx", true, "", ""},
		{"@SomeHandle", true, "", ""},
		{"", true, "", ""},

		// Invalid channel ID format
		{"https://www.youtube.com/channel/NotAChannelID", true, "", ""},

		// Empty handle
		{"https://www.youtube.com/@", true, "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ParseYouTubeChannelURL(tt.input)
			if tt.wantNil {
				if result != nil {
					t.Errorf("ParseYouTubeChannelURL(%q) = %+v, want nil", tt.input, result)
				}
				return
			}
			if result == nil {
				t.Fatalf("ParseYouTubeChannelURL(%q) = nil, want non-nil", tt.input)
			}
			if result.ChannelID != tt.wantID {
				t.Errorf("ParseYouTubeChannelURL(%q).ChannelID = %q, want %q", tt.input, result.ChannelID, tt.wantID)
			}
			if result.Path != tt.wantPath {
				t.Errorf("ParseYouTubeChannelURL(%q).Path = %q, want %q", tt.input, result.Path, tt.wantPath)
			}
		})
	}
}

func TestIsVideoID(t *testing.T) {
	if !IsVideoID("dQw4w9WgXcQ") {
		t.Error("expected true for valid ID")
	}
	if IsVideoID("short") {
		t.Error("expected false for short string")
	}
	if IsVideoID("has spaces!!") {
		t.Error("expected false for invalid chars")
	}
}

// stubYouTubeBaseURL points youtubeBaseURL at the given test server for
// the duration of t, so ResolveYouTubeChannel hits the local server
// instead of the real YouTube origin.
func stubYouTubeBaseURL(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := youtubeBaseURL
	youtubeBaseURL = srv.URL
	t.Cleanup(func() { youtubeBaseURL = prev })
}

// recordResolveWaits replaces ResolveYouTubeChannel's backoff with one that
// records each delay it was asked for and returns at once.
func recordResolveWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	prev := resolveRetryWait
	resolveRetryWait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	t.Cleanup(func() { resolveRetryWait = prev })
	return &waits
}

// TestResolveYouTubeChannelExtractsCanonical exercises the success path:
// a 200 OK page with a canonical channel link and an og:title meta.
func TestResolveYouTubeChannelExtractsCanonical(t *testing.T) {
	const channelID = "UCabc1234567890_-DEFGHIj"
	page := fmt.Sprintf(`<!doctype html><html><head>
<link rel="canonical" href="https://www.youtube.com/channel/%s">
<meta property="og:title" content="Some Channel">
</head><body></body></html>`, channelID)

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, page)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	info, err := ResolveYouTubeChannel(t.Context(), "/@SomeChannel")
	if err != nil {
		t.Fatalf("ResolveYouTubeChannel: %v", err)
	}
	if info.ChannelID != channelID {
		t.Errorf("ChannelID: want %q, got %q", channelID, info.ChannelID)
	}
	if info.Name != "Some Channel" {
		t.Errorf("Name: want %q, got %q", "Some Channel", info.Name)
	}
}

// TestResolveYouTubeChannelFallsBackToExternalID covers the second
// regex branch: when the canonical link is missing, externalIDRe must
// pick the channel ID from ytInitialData JSON.
func TestResolveYouTubeChannelFallsBackToExternalID(t *testing.T) {
	const channelID = "UCxxxxxxxxxxxxxxxxxxxxxx"
	page := fmt.Sprintf(`<!doctype html><html><head>
<title>Some Channel - YouTube</title>
<script>var ytInitialData = {"externalId": "%s"};</script>
</head><body></body></html>`, channelID)

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, page)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	info, err := ResolveYouTubeChannel(t.Context(), "/@SomeChannel")
	if err != nil {
		t.Fatalf("ResolveYouTubeChannel: %v", err)
	}
	if info.ChannelID != channelID {
		t.Errorf("ChannelID via fallback: want %q, got %q", channelID, info.ChannelID)
	}
	if info.Name != "Some Channel" {
		t.Errorf("Name from <title>: want %q, got %q", "Some Channel", info.Name)
	}
}

// TestResolveYouTubeChannelMissingChannelIDIsError ensures we don't
// silently return an empty ID — operators get a useful error instead.
func TestResolveYouTubeChannelMissingChannelIDIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(rw, `<!doctype html><html><body>nope</body></html>`)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	_, err := ResolveYouTubeChannel(t.Context(), "/@SomeChannel")
	if err == nil {
		t.Fatal("ResolveYouTubeChannel with no channel ID: want error, got nil")
	}
}

// TestResolveYouTubeChannelRetriesOn5xx exercises the retry loop:
// transient 5xx errors should be retried up to twice before surfacing,
// after 1 s and then 2 s. The backoff is recorded rather than slept: the
// test used to sleep both, 3 s a run, to hold the loop to a 2.9 s floor.
//
// Mutants: no retry (one hit, an error); no backoff, or the same delay
// twice (the recorded delays).
func TestResolveYouTubeChannelRetriesOn5xx(t *testing.T) {
	waits := recordResolveWaits(t)
	const channelID = "UCabc1234567890_-DEFGHIj"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			http.Error(rw, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(rw, `<link rel="canonical" href="https://www.youtube.com/channel/%s">`, channelID)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	info, err := ResolveYouTubeChannel(t.Context(), "/@SomeChannel")
	if err != nil {
		t.Fatalf("ResolveYouTubeChannel after retries: %v", err)
	}
	if info.ChannelID != channelID {
		t.Errorf("ChannelID: want %q, got %q", channelID, info.ChannelID)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("server hits: want 3, got %d", got)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !slices.Equal(*waits, want) {
		t.Errorf("backoff before the retries: %v, want %v", *waits, want)
	}
}

// TestResolveRetryWaitSleepsOrEndsWithItsContext covers the real backoff
// the two tests around it record instead of sleeping: it waits out its
// delay, and gives up the moment ctx ends.
//
// Mutants: no sleep (it returns before its delay); ctx ignored (the
// cancelled wait sleeps out its hour, cut at 5 s).
func TestResolveRetryWaitSleepsOrEndsWithItsContext(t *testing.T) {
	const d = 20 * time.Millisecond
	start := time.Now()
	if err := resolveRetryWait(t.Context(), d); err != nil {
		t.Fatalf("resolveRetryWait(%v) = %v, want nil", d, err)
	}
	if took := time.Since(start); took < d {
		t.Errorf("resolveRetryWait(%v) returned after %v", d, took)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("resolveRetryWait panicked: %v", r)
			}
		}()
		done <- resolveRetryWait(ctx, time.Hour)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("resolveRetryWait with an ended context = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolveRetryWait kept waiting after its context ended")
	}
}

// TestResolveYouTubeChannelFastFails4xx covers the deterministic
// fast-fail branch: a 404 should NOT trigger the retry loop — one request,
// and no backoff waited (it used to be timed against 500 ms instead, around
// a real loopback round trip).
//
// Mutant: 4xx retried like 5xx (hits 3, two waits).
func TestResolveYouTubeChannelFastFails4xx(t *testing.T) {
	waits := recordResolveWaits(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(rw, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	_, err := ResolveYouTubeChannel(t.Context(), "/@Missing")
	if err == nil {
		t.Fatal("ResolveYouTubeChannel on 404: want error, got nil")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("4xx should fast-fail without retries: hits=%d, want 1", got)
	}
	if len(*waits) != 0 {
		t.Errorf("4xx fast-fail waited out a backoff: %v (retry loop fired?)", *waits)
	}
}
