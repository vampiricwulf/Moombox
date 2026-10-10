package utils

import (
	"errors"
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

// TestResolveChannelInputBareHandle pins W25-12: a bare @handle — the form
// both channel dialogs advertise — is resolved as the YouTube handle it is,
// fetching the same /@handle page its youtube.com URL would, where it used
// to come back nil and be stored verbatim, a channel ID no monitor could
// poll. A "handle" with whitespace in it is no handle and is never fetched.
//
// Mutants killed: dropping the "@" rewrite in ResolveChannelInput (nil for
// the bare handle); dropping the whitespace check (the stub is fetched for
// "@foo bar" and answers a channel).
func TestResolveChannelInputBareHandle(t *testing.T) {
	const channelID = "UCabc1234567890_-DEFGHIj"
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		paths = append(paths, req.URL.Path)
		fmt.Fprintf(rw, `<link rel="canonical" href="https://www.youtube.com/channel/%s"><meta property="og:title" content="Some Handle">`, channelID)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	got, err := ResolveChannelInput(t.Context(), "  @SomeHandle  ")
	if err != nil {
		t.Fatalf("ResolveChannelInput: %v", err)
	}
	if got == nil {
		t.Fatal("ResolveChannelInput(@SomeHandle) = nil: the bare handle was not resolved")
	}
	if got.ID != channelID || got.Platform != "youtube" || got.Name != "Some Handle" {
		t.Errorf("ResolveChannelInput(@SomeHandle) = %+v, want ID %s, platform youtube, name Some Handle", got, channelID)
	}
	if len(paths) != 1 || paths[0] != "/@SomeHandle" {
		t.Errorf("fetched %q, want the one page /@SomeHandle", paths)
	}

	paths = nil
	got, err = ResolveChannelInput(t.Context(), "@foo bar")
	if err != nil || got != nil {
		t.Errorf("ResolveChannelInput(%q) = %+v, %v; want nil, nil", "@foo bar", got, err)
	}
	if len(paths) != 0 {
		t.Errorf("a handle with a space in it was fetched: %q", paths)
	}
}

// TestNeedsChannelResolve is the rule the writers rate limit or go
// asynchronous on: a channel URL or a bare @handle, nothing else.
//
// Mutant killed: dropping the "@" arm (a bare handle is taken as typed).
func TestNeedsChannelResolve(t *testing.T) {
	for in, want := range map[string]bool{
		"@SomeHandle":                          true,
		"  @SomeHandle":                        true,
		"https://www.youtube.com/@SomeHandle":  true,
		"youtube.com/channel/UCxxxxxxxxxxxxxx": true,
		"twitch.tv/shroud":                     true,
		"UCxxxxxxxxxxxxxxxxxxxxxx":             false,
		"shroud":                               false,
		"":                                     false,
		"foo@bar":                              false,
	} {
		if got := NeedsChannelResolve(in); got != want {
			t.Errorf("NeedsChannelResolve(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestNormalizeChannelID pins the one normaliser behind every channel-ID
// writer (W25-14): a plain ID comes back trimmed and unresolved; a bare
// @handle and a handle URL are resolved; a URL or "handle" that names no
// channel is ErrNotChannelURL, never the input echoed back as an ID; a
// lookup that fails is its own error, not ErrNotChannelURL.
//
// Mutants killed: dropping the TrimSpace (" UC… " kept padded); answering
// a nil resolution with the input instead of ErrNotChannelURL (the watch
// URL comes back as an ID); skipping resolution for a bare handle (it comes
// back verbatim).
func TestNormalizeChannelID(t *testing.T) {
	const channelID = "UCabc1234567890_-DEFGHIj"
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/@Missing" {
			http.NotFound(rw, req)
			return
		}
		fmt.Fprintf(rw, `<link rel="canonical" href="https://www.youtube.com/channel/%s"><meta property="og:title" content="X">`, channelID)
	}))
	t.Cleanup(srv.Close)
	stubYouTubeBaseURL(t, srv)

	got, err := NormalizeChannelID(t.Context(), "  UCxxxxxxxxxxxxxxxxxxxxxx \t")
	if err != nil || got == nil || *got != (ResolvedChannel{ID: "UCxxxxxxxxxxxxxxxxxxxxxx"}) {
		t.Errorf("plain ID: got %+v, %v; want the trimmed ID alone", got, err)
	}
	for _, in := range []string{"@SomeHandle", "https://www.youtube.com/@SomeHandle"} {
		got, err := NormalizeChannelID(t.Context(), in)
		if err != nil || got == nil || got.ID != channelID || got.Platform != "youtube" || got.Name != "X" {
			t.Errorf("NormalizeChannelID(%q) = %+v, %v; want %s on youtube named X", in, got, err, channelID)
		}
	}
	got, err = NormalizeChannelID(t.Context(), "twitch.tv/shroud")
	if err != nil || got == nil || got.ID != "shroud" || got.Platform != "twitch" {
		t.Errorf("Twitch URL: got %+v, %v; want shroud on twitch", got, err)
	}
	for _, in := range []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.twitch.tv/videos/123456",
		"@",
		"@foo bar",
	} {
		got, err := NormalizeChannelID(t.Context(), in)
		if !errors.Is(err, ErrNotChannelURL) {
			t.Errorf("NormalizeChannelID(%q) = %+v, %v; want ErrNotChannelURL", in, got, err)
		}
	}
	got, err = NormalizeChannelID(t.Context(), "@Missing")
	if err == nil || errors.Is(err, ErrNotChannelURL) || got != nil {
		t.Errorf("failed lookup: got %+v, %v; want the lookup's own error", got, err)
	}
}
