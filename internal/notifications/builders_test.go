package notifications

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ytFacts and twFacts are the two platform shapes every builder is exercised
// against. They carry a markdown-hostile title on purpose: the escape rule is
// the one thing a reader of a Discord channel notices when it is missing.
func ytFacts() JobFacts {
	return JobFacts{
		ID:               "dQw4w9WgXcQ",
		VideoID:          "dQw4w9WgXcQ",
		Platform:         "youtube",
		Title:            "【歌枠】 *live* _test_",
		Channel:          "Some Channel",
		ChannelURL:       "https://www.youtube.com/channel/UC123",
		ChannelAvatarURL: "https://yt3.example/avatar.jpg",
		URL:              "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		ThumbnailURL:     "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg",
	}
}

func twFacts() JobFacts {
	return JobFacts{
		ID:               "tw_12345",
		VideoID:          "12345",
		Platform:         "twitch",
		Title:            "Streamer — playing something",
		Channel:          "Streamer",
		ChannelURL:       "https://twitch.tv/streamer",
		ChannelAvatarURL: "https://static.example/profile.png",
		URL:              "https://twitch.tv/streamer",
		ThumbnailURL:     "https://static.example/preview.jpg",
		Category:         "Just Chatting",
	}
}

// fieldValue returns the value of the named field, and whether it was present.
func fieldValue(fields []Field, name string) (string, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

func mustField(t *testing.T, fields []Field, name string) string {
	t.Helper()
	v, ok := fieldValue(fields, name)
	if !ok {
		var names []string
		for _, f := range fields {
			names = append(names, f.Name)
		}
		t.Fatalf("field %q missing; got %v", name, names)
	}
	return v
}

func mustNotHaveField(t *testing.T, fields []Field, name string) {
	t.Helper()
	if v, ok := fieldValue(fields, name); ok {
		t.Errorf("field %q present with value %q — it must be omitted when unknown", name, v)
	}
}

// TestJobBuildersShareOneOptsShape pins the four rules every job embed obeys,
// for both platforms and all five families at once.
//
// Mutants this kills:
//   - forgetting Platform or JobID on one builder (the footer reads both, and
//     Arc N3 keys edit-in-place on JobID).
//   - building an author line from a blank channel.
//   - putting the platform back in the title (the §0 ruling).
func TestJobBuildersShareOneOptsShape(t *testing.T) {
	for _, f := range []JobFacts{ytFacts(), twFacts()} {
		cases := map[string]func() (string, string, NotificationType, []Field, SendOptions){
			"JobAdded":     func() (string, string, NotificationType, []Field, SendOptions) { return JobAdded(f) },
			"StreamFound":  func() (string, string, NotificationType, []Field, SendOptions) { return StreamFound(f) },
			"JobCancelled": func() (string, string, NotificationType, []Field, SendOptions) { return JobCancelled(f) },
			"DownloadFinished": func() (string, string, NotificationType, []Field, SendOptions) {
				return DownloadFinished(f, []Part{{File: "a.mp4"}})
			},
			"TrimCreated": func() (string, string, NotificationType, []Field, SendOptions) {
				return TrimCreated(f, TrimFacts{TimeRange: "0:00 - 1:00", Duration: time.Minute})
			},
		}
		for name, build := range cases {
			t.Run(f.Platform+"/"+name, func(t *testing.T) {
				title, _, _, _, opts := build()
				if opts.Platform != f.Platform {
					t.Errorf("opts.Platform = %q, want %q", opts.Platform, f.Platform)
				}
				if opts.JobID != f.ID {
					t.Errorf("opts.JobID = %q, want %q", opts.JobID, f.ID)
				}
				if opts.URL != f.URL {
					t.Errorf("opts.URL = %q, want the video's platform page %q", opts.URL, f.URL)
				}
				if opts.Author == nil {
					t.Fatal("opts.Author is nil — every job embed names its channel")
				}
				if opts.Author.Name != f.Channel || opts.Author.IconURL != f.ChannelAvatarURL || opts.Author.URL != f.ChannelURL {
					t.Errorf("opts.Author = %+v, want {%q %q %q}", *opts.Author, f.Channel, f.ChannelAvatarURL, f.ChannelURL)
				}
				lower := strings.ToLower(title)
				if strings.Contains(lower, "twitch") || strings.Contains(lower, "youtube") {
					t.Errorf("title %q names a platform — the platform belongs in the author line", title)
				}
			})
		}
	}
}

// TestBuildersOmitTheAuthorWhenTheChannelIsUnknown covers the CLI add, which
// knows no channel for YouTube.
//
// Mutant: building an Author unconditionally — Discord draws an empty author
// bar with a broken avatar.
func TestBuildersOmitTheAuthorWhenTheChannelIsUnknown(t *testing.T) {
	f := ytFacts()
	f.Channel = ""
	if _, _, _, _, opts := JobAdded(f); opts.Author != nil {
		t.Errorf("opts.Author = %+v with no channel, want nil", *opts.Author)
	}
}

// TestBuildersOmitTheIDFieldWhenTheVideoIDIsUnknown covers the same "omit what
// you do not know" promise JobFacts makes, at the one field that was breaking
// it, for all three builders that emit an id and on both platforms (the LABEL
// differs, so a guard added to only one of them would still ship the other).
//
// Mutant: AddInline instead of AddInlineIf — Field.Value has no omitempty and
// nothing downstream drops an empty one, so Discord 400s on the field and
// discord.go treats a non-429 4xx as permanent: the whole embed is dropped
// after one attempt, not retried.
func TestBuildersOmitTheIDFieldWhenTheVideoIDIsUnknown(t *testing.T) {
	for _, base := range []JobFacts{ytFacts(), twFacts()} {
		f := base
		f.VideoID = ""
		cases := map[string]func() []Field{
			"JobAdded":     func() []Field { _, _, _, fields, _ := JobAdded(f); return fields },
			"StreamFound":  func() []Field { _, _, _, fields, _ := StreamFound(f); return fields },
			"JobCancelled": func() []Field { _, _, _, fields, _ := JobCancelled(f); return fields },
		}
		for name, build := range cases {
			t.Run(f.Platform+"/"+name, func(t *testing.T) {
				fields := build()
				mustNotHaveField(t, fields, IDLabel(f.Platform))
				for _, fl := range fields {
					if fl.Value == "" {
						t.Errorf("field %q has an empty value — Discord answers that with a permanent 400", fl.Name)
					}
				}
			})
		}
	}
}

// TestJobAddedPerPlatformAndEntryPoint is the C3 unification: four call sites,
// one shape.
//
// Mutants this kills:
//   - a hardcoded "Video ID" for Twitch (IDLabel exists for exactly this).
//   - dropping the optional format/range fields the web add supplies.
//   - describing a CLI add with an empty title as "Manually added: ".
func TestJobAddedPerPlatformAndEntryPoint(t *testing.T) {
	t.Run("youtube web add with format and range", func(t *testing.T) {
		f := ytFacts()
		f.VideoFormat = "itag 299"
		f.AudioFormat = "itag 251"
		f.TimeRange = "0:30 - 1:45 (1:15)"
		title, desc, ntype, fields, opts := JobAdded(f)
		if title != "Job Added" {
			t.Errorf("title = %q, want %q", title, "Job Added")
		}
		if ntype != TypeInfo {
			t.Errorf("ntype = %v, want TypeInfo", ntype)
		}
		if want := "Manually added: " + EscapeMarkdown(f.Title); desc != want {
			t.Errorf("desc = %q, want %q", desc, want)
		}
		if got := mustField(t, fields, "Video ID"); got != "dQw4w9WgXcQ" {
			t.Errorf("Video ID = %q", got)
		}
		if got := mustField(t, fields, "Video Format"); got != "itag 299" {
			t.Errorf("Video Format = %q", got)
		}
		mustField(t, fields, "Audio Format")
		mustField(t, fields, "Time Range")
		if opts.Event != "added" {
			t.Errorf("opts.Event = %q, want %q", opts.Event, "added")
		}
		if opts.Thumbnail != f.ThumbnailURL {
			t.Errorf("opts.Thumbnail = %q, want the job thumbnail", opts.Thumbnail)
		}
	})

	t.Run("twitch add uses the Stream ID label", func(t *testing.T) {
		_, _, _, fields, _ := JobAdded(twFacts())
		mustField(t, fields, "Stream ID")
		mustNotHaveField(t, fields, "Video ID")
	})

	t.Run("cli add with no title falls back to the id", func(t *testing.T) {
		f := JobFacts{ID: "abc123", VideoID: "abc123", Platform: "youtube", URL: "https://www.youtube.com/watch?v=abc123"}
		_, desc, _, fields, _ := JobAdded(f)
		if desc != "Manually added: abc123" {
			t.Errorf("desc = %q, want %q", desc, "Manually added: abc123")
		}
		mustNotHaveField(t, fields, "Channel")
		mustNotHaveField(t, fields, "Video Format")
	})
}

// TestStreamFoundDescriptionIsPerPlatform keeps the two discovery shapes the
// audit recorded (rows #26/#27) while collapsing the two titles into one.
//
// Mutants this kills:
//   - one description for both platforms (a Twitch find would read "Found
//     matching stream" for a stream that is live right now).
//   - emitting Category for YouTube, where it is always empty.
func TestStreamFoundDescriptionIsPerPlatform(t *testing.T) {
	t.Run("youtube", func(t *testing.T) {
		f := ytFacts()
		title, desc, ntype, fields, opts := StreamFound(f)
		if title != "Stream Found" {
			t.Errorf("title = %q", title)
		}
		if ntype != TypeInfo {
			t.Errorf("ntype = %v, want TypeInfo", ntype)
		}
		if want := "Found matching stream: " + EscapeMarkdown(f.Title); desc != want {
			t.Errorf("desc = %q, want %q", desc, want)
		}
		mustNotHaveField(t, fields, "Category")
		if opts.Event != "found" {
			t.Errorf("opts.Event = %q", opts.Event)
		}
	})

	t.Run("twitch", func(t *testing.T) {
		f := twFacts()
		_, desc, _, fields, _ := StreamFound(f)
		if want := "Live: " + EscapeMarkdown(f.Title); desc != want {
			t.Errorf("desc = %q, want %q", desc, want)
		}
		if got := mustField(t, fields, "Category"); got != "Just Chatting" {
			t.Errorf("Category = %q", got)
		}
		mustField(t, fields, "Stream ID")
	})
}

// TestJobCancelledIsOneTitle collapses row #18 "Download Cancelled" and row
// #25 "Job Cancelled" (audit C5): which code path noticed the cancel is not
// something an operator can act on.
//
// Mutant: keeping TypeInfo — the embed loses the orange sidebar and reads like
// an ordinary lifecycle note.
func TestJobCancelledIsOneTitle(t *testing.T) {
	f := twFacts()
	title, desc, ntype, fields, opts := JobCancelled(f)
	if title != "Job Cancelled" {
		t.Errorf("title = %q, want %q", title, "Job Cancelled")
	}
	if ntype != TypeCancelled {
		t.Errorf("ntype = %v, want TypeCancelled", ntype)
	}
	if want := "Cancelled: " + EscapeMarkdown(f.Title); desc != want {
		t.Errorf("desc = %q, want %q", desc, want)
	}
	mustField(t, fields, "Channel")
	mustField(t, fields, "Stream ID")
	if opts.Event != "cancelled" {
		t.Errorf("opts.Event = %q, want %q", opts.Event, "cancelled")
	}
}

// TestEscapeReachesEveryJobSuppliedString is the rule a reader notices when it
// is missing. Two values are deliberately raw and documented where they are
// built: the author NAME (Discord renders no markdown in the author bar, so
// escaping would show the backslashes) and the platform ID (addIDField — it is
// the field a reader copies out, and a YouTube id is full of "_" and "-").
//
// Mutant: escaping the whole embed — the generated "Time Range" separator
// would pick up backslashes.
func TestEscapeReachesEveryJobSuppliedString(t *testing.T) {
	f := ytFacts()
	f.Channel = "a_b*c"
	_, _, _, fields, opts := StreamFound(f)
	if got := mustField(t, fields, "Channel"); got != EscapeMarkdown("a_b*c") {
		t.Errorf("Channel = %q, want the escaped form %q", got, EscapeMarkdown("a_b*c"))
	}
	if opts.Author == nil || opts.Author.Name != "a_b*c" {
		t.Error("Author.Name must carry the RAW channel — Discord does not render markdown there")
	}

	// The " -> " between qualities is the BUILDER's own text, not the job's.
	// EscapeMarkdown escapes ">", so escaping the JOINED string puts a
	// backslash in front of every separator.
	_, _, _, ff, _ := DownloadFinished(ytFacts(), []Part{{Quality: "1080p60"}, {Quality: "720p60"}})
	if got := mustField(t, ff, "Qualities"); got != "1080p60 -> 720p60" {
		t.Errorf("Qualities = %q — the generated separator was escaped", got)
	}
}

// TestChatMessagesRendersAtZero is the ruled restore. The old single-part site
// rendered the count whenever the job had one, zero included; the first draft
// of this builder guarded `> 0` and silently turned "the chat capture ran and
// caught nothing" into "no chat capture at all" — two different facts about an
// archive, and only one of them is a reason to go looking.
//
// Mutants this kills:
//   - the `> 0` guard coming back (both shapes assert the zero).
//   - rendering a field for a nil pointer, which really is unknown.
func TestChatMessagesRendersAtZero(t *testing.T) {
	zero, some := 0, 4210
	shapes := map[string][]Part{
		"single-part": {{File: "a.mp4"}},
		"multi-part":  {{Quality: "1080p60"}, {Quality: "720p60"}},
	}
	for shape, parts := range shapes {
		t.Run(shape, func(t *testing.T) {
			f := ytFacts()

			f.ChatMessages = &zero
			_, _, _, fields, _ := DownloadFinished(f, parts)
			if got := mustField(t, fields, "Chat Messages"); got != "0" {
				t.Errorf("Chat Messages = %q, want %q — a capture that caught nothing is a fact, not an unknown", got, "0")
			}

			f.ChatMessages = &some
			_, _, _, fields, _ = DownloadFinished(f, parts)
			if got := mustField(t, fields, "Chat Messages"); got != "4210" {
				t.Errorf("Chat Messages = %q, want %q", got, "4210")
			}

			f.ChatMessages = nil
			_, _, _, fields, _ = DownloadFinished(f, parts)
			mustNotHaveField(t, fields, "Chat Messages")
		})
	}
}

// TestTrimCreatedOmitsAnUnknownSourceVideo is the symmetry addIDField already
// keeps: no builder puts an empty-valued field on the wire, because Discord
// answers one with a permanent 400 and discord.go drops the whole embed.
func TestTrimCreatedOmitsAnUnknownSourceVideo(t *testing.T) {
	_, _, _, fields, _ := TrimCreated(JobFacts{}, TrimFacts{Duration: time.Minute})
	mustNotHaveField(t, fields, "Source Video")
	for _, fl := range fields {
		if fl.Value == "" {
			t.Errorf("field %q has an empty value — Discord answers that with a permanent 400", fl.Name)
		}
	}
}

// TestDescriptionExcerptIsRuneSafeAndBounded pins the COMPOSITION the builder
// uses, at the product budget. ClampRunes' own boundary behaviour is pinned
// exhaustively by N1 in limits_test.go; what this adds is that
// DownloadFinished spends the 300 on the DESCRIPTION and escapes afterwards.
//
// Mutants this kill:
//   - a byte-based cut in place of ClampRunes: 300 bytes of Japanese is 100
//     characters, and the rune count below catches it.
//   - escaping before clamping: the clamp would then spend budget on
//     backslashes and could cut one away from the character it protects.
func TestDescriptionExcerptIsRuneSafeAndBounded(t *testing.T) {
	f := ytFacts()
	f.Description = strings.Repeat("あ", 500) // 3 bytes each
	_, _, _, fields, _ := DownloadFinished(f, []Part{{File: "a.mp4"}})
	got := mustField(t, fields, "Description")
	if !utf8.ValidString(got) {
		t.Error("the excerpt is not valid UTF-8 — the cut landed mid-rune")
	}
	if n := utf8.RuneCountInString(got); n != descriptionExcerptLen {
		t.Errorf("excerpt = %d runes, want %d", n, descriptionExcerptLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("excerpt does not carry the clamp marker: %q", got)
	}
	short := "already short"
	f.Description = short
	_, _, _, fields, _ = DownloadFinished(f, []Part{{File: "a.mp4"}})
	if got := mustField(t, fields, "Description"); got != short {
		t.Errorf("Description = %q — a string already under the budget was altered", got)
	}
}
