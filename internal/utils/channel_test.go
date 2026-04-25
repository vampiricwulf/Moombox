package utils

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLooksLikeURL(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"https://www.youtube.com/watch?v=abc", true},
		{"youtube.com/@SomeChannel", true},
		{"https://youtu.be/abc123", true},
		{"https://twitch.tv/shroud", true},
		{"twitch.tv/shroud", true},
		{"shroud", false},
		{"dQw4w9WgXcQ", false},
		{"", false},
		{"   ", false},
		{"https://example.com/page", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := LooksLikeURL(tt.input)
			if got != tt.want {
				t.Errorf("LooksLikeURL(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestResolveChannelInputTwitchURL covers the no-network Twitch path:
// ExtractTwitchTarget returns immediately for a valid login URL and
// ResolveChannelInput surfaces it as Platform="twitch".
func TestResolveChannelInputTwitchURL(t *testing.T) {
	got, err := ResolveChannelInput(t.Context(), "https://www.twitch.tv/shroud")
	if err != nil {
		t.Fatalf("ResolveChannelInput: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveChannelInput: want resolved, got nil")
	}
	if got.Platform != "twitch" {
		t.Errorf("Platform: want twitch, got %q", got.Platform)
	}
	if got.ID != "shroud" {
		t.Errorf("ID: want shroud, got %q", got.ID)
	}
}

// TestResolveChannelInputDirectChannelID covers the no-network YouTube
// path: a /channel/UCxxx URL needs no resolution.
func TestResolveChannelInputDirectChannelID(t *testing.T) {
	const id = "UCxxxxxxxxxxxxxxxxxxxxxx"
	got, err := ResolveChannelInput(t.Context(), "https://www.youtube.com/channel/"+id)
	if err != nil {
		t.Fatalf("ResolveChannelInput: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveChannelInput: want resolved, got nil")
	}
	if got.Platform != "youtube" {
		t.Errorf("Platform: want youtube, got %q", got.Platform)
	}
	if got.ID != id {
		t.Errorf("ID: want %q, got %q", id, got.ID)
	}
}

// TestResolveChannelInputYouTubeHandle exercises the network-resolved
// path: a /@Handle URL fetches the page and extracts the channel ID.
func TestResolveChannelInputYouTubeHandle(t *testing.T) {
	const channelID = "UCabc1234567890_-DEFGHIj"
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(rw, `<link rel="canonical" href="https://www.youtube.com/channel/%s"><meta property="og:title" content="X">`, channelID)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	got, err := ResolveChannelInput(t.Context(), "https://www.youtube.com/@SomeHandle")
	if err != nil {
		t.Fatalf("ResolveChannelInput: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveChannelInput: want resolved, got nil")
	}
	if got.Platform != "youtube" {
		t.Errorf("Platform: want youtube, got %q", got.Platform)
	}
	if got.ID != channelID {
		t.Errorf("ID: want %q, got %q", channelID, got.ID)
	}
	if got.Name != "X" {
		t.Errorf("Name: want X, got %q", got.Name)
	}
}

// TestResolveChannelInputUnrecognizedReturnsNil covers the catch-all:
// inputs that aren't YouTube or Twitch URLs return (nil, nil) — the
// caller treats this as "not a URL" rather than an error.
func TestResolveChannelInputUnrecognizedReturnsNil(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"https://example.com/page",
		"some random text",
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			got, err := ResolveChannelInput(t.Context(), in)
			if err != nil {
				t.Fatalf("ResolveChannelInput(%q): %v", in, err)
			}
			if got != nil {
				t.Errorf("ResolveChannelInput(%q) = %+v, want nil", in, got)
			}
		})
	}
}
