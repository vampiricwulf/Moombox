package notifications

import (
	"fmt"
	"strings"
	"time"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// JobFacts is everything a job-shaped embed can say, in a form this package
// can read without knowing what a database.Job is.
//
// It is a flat carrier on purpose. The alternative — an interface, or an
// import of internal/database — would put the row type inside the package
// internal/tui imports, which is the back door the TUI import fence exists to
// keep shut. Producers fill what they know and leave the rest zero; every
// builder treats a zero field as "unknown" and OMITS its field rather than
// rendering an empty one.
//
// worker.NotifyFacts is the ONE mapper from a database row into this struct. A
// producer that knows more than the mapper assigns to the result; it does not
// write a second mapper.
type JobFacts struct {
	// --- Identity. Every builder reads these. ---

	// ID is the job row's id: the footer's job id and, from Arc N3, the
	// edit-in-place key. VideoID is what the operator sees in the embed — the
	// two differ for Twitch, whose job id carries a prefix.
	ID      string
	VideoID string
	// Platform is "youtube" or "twitch". It drives IDLabel and the author
	// line, never the title (§0 ruling: neutral titles).
	Platform string
	// Title is the stream/video title as the platform supplied it, RAW. Every
	// builder escapes it before it reaches a description or a field.
	Title string
	// Channel, ChannelURL and ChannelAvatarURL become the embed's author line.
	// An empty Channel means no author line at all.
	Channel          string
	ChannelURL       string
	ChannelAvatarURL string
	// URL is the video's page on its platform; ThumbnailURL its preview image.
	URL          string
	ThumbnailURL string

	// --- Discovery and manual add ---

	// Category is the Twitch game category; always empty for YouTube.
	Category string
	// VideoFormat, AudioFormat and TimeRange are the advanced options a web
	// add can carry, pre-rendered by the route that owns their wording.
	VideoFormat string
	AudioFormat string
	TimeRange   string

	// --- Finalize (DownloadFinished only) ---

	// TotalTime is wall clock from download start to finalize.
	TotalTime time.Duration
	// SegmentCounter is the job-level sequence pair, e.g. "V: 1234 A: 1230".
	SegmentCounter string
	// ChatMessages is nil when the job captured no chat at all; a non-nil zero
	// means the capture ran and caught nothing, which is rendered.
	ChatMessages *int
	// FormatSelection and TrimmedRange are pre-rendered by the orchestrator,
	// which owns the itag and timestamp vocabularies.
	FormatSelection string
	TrimmedRange    string
	// Description is the video description, RAW and unbounded; the builder
	// excerpts it.
	Description string

	// --- Outcome truth (DownloadFinished only; the error send builds its own
	// fields at its site, where the staging question is answerable) ---

	// IncompleteTail turns the embed Warning-coloured and adds the Tail field:
	// the archive is knowingly short and Resume appends the rest.
	IncompleteTail bool
	// ChatIncomplete reports chat_status == "incomplete".
	ChatIncomplete bool
	// AsideCount is how many set-aside recordings the job's staging still held
	// at finalize.
	AsideCount int
}

// Part is one muxed output of a job: the whole recording for a job that never
// split, one part for a quality/gap-split job. DownloadFinished SUMS their
// sizes and durations, so a caller must not also pass a job-level total.
type Part struct {
	// File is the output's base name; "" when the caller has no name to give
	// (the multi-part shape names the count, not the files).
	File string
	// Quality is the part's label ("1080p60"); "" when unknown.
	Quality string
	// Size in bytes; 0 means unknown, not empty.
	Size int64
	// Duration; 0 means unknown.
	Duration time.Duration
	// Width/Height/Fps describe the part's video; 0 means unknown. Only the
	// FIRST part's resolution is rendered — a split job's parts differ by
	// definition, and listing every one would bury the useful fields.
	Width, Height, Fps int
}

// TrimFacts is the trim-specific half of a "Trim Created" embed.
type TrimFacts struct {
	// TimeRange is pre-rendered ("1:02:03 - 1:05:00"), so this package needs
	// no timestamp formatter of its own.
	TimeRange string
	Duration  time.Duration
	// Size is nil when the trim file could not be stat'd.
	Size *int64
	// Parts is how many source parts the trim spans; the field is rendered
	// only when it is more than one.
	Parts int
}

// descriptionExcerptLen bounds the Description field at the PRODUCT level: a
// finished embed is a notification, not a copy of the video page.
//
// The cut itself is ClampRunes (limits.go) — the same rune-safe clamp the
// payload safety net uses, which Arc N1 exported for precisely this producer
// excerpt, so the two can never disagree about where a multi-byte character
// ends. Nothing in this file truncates on its own.
const descriptionExcerptLen = 300

// displayName is what an embed calls the job: its title, or its id when no
// title was ever fetched. `moombox add` runs no metadata fetch and writes the
// placeholder "Manual Add" to the row, so the CLI sites pass an EMPTY Title
// and this falls back — "Manually added: Manual Add" is not a sentence.
func displayName(f JobFacts) string {
	if f.Title != "" {
		return f.Title
	}
	if f.VideoID != "" {
		return f.VideoID
	}
	return f.ID
}

// authorFor builds the embed's author line, or nil when the channel is
// unknown. The name is RAW: Discord renders no markdown in the author bar, so
// escaping it would show the backslashes.
func authorFor(f JobFacts) *Author {
	if f.Channel == "" {
		return nil
	}
	return &Author{Name: f.Channel, IconURL: f.ChannelAvatarURL, URL: f.ChannelURL}
}

// jobOpts is the SendOptions every job embed shares. Image (the full-width
// picture) is set only by DownloadFinished, and only for a platform whose
// preview survives the stream.
//
// The builders never read config. Arc N2b's manager rewrites URL and
// Author.URL at SEND time when `network.public_url` is set, which is why the
// values assembled here are always the platform's own pages.
func jobOpts(f JobFacts, event string) SendOptions {
	return SendOptions{
		URL:       f.URL,
		Event:     event,
		Thumbnail: f.ThumbnailURL,
		Author:    authorFor(f),
		Platform:  f.Platform,
		JobID:     f.ID,
	}
}

// addIDField appends the platform-correct id field. Twitch broadcasts carry
// stream ids and everything else video ids; IDLabel is the one place that
// decision lives, and three of the five builders need it.
//
// The id is the one job-supplied string this package does NOT escape, and
// that is deliberate: a YouTube id routinely contains "_" and "-", so
// escaping turns "a_b-c" into "a\_b\-c" in the one field a reader copies out
// of the embed to paste somewhere else. Discord's italic needs a MATCHED pair
// of underscores, so a single id renders verbatim; the cosmetic risk is an
// id that happens to hold two, against a real cost to every id.
//
// The guard is AddInlineIf, not AddInline, and that is not a style choice:
// Field.Value carries no omitempty, clampEmbed never drops an empty value and
// buildPayload copies the slice straight through, so an unknown id would put
// {"name":"Video ID","value":""} on the wire. Discord answers an empty field
// value with a 400, and DiscordWebhook.Send treats a non-429 4xx as permanent
// — the WHOLE embed is dropped after one attempt. N1 guards the same class one
// layer down for the author object; this is JobFacts' "omit what you do not
// know" promise kept at the only field that was breaking it.
func addIDField(b *FieldBuilder, f JobFacts) *FieldBuilder {
	return b.AddInlineIf(f.VideoID != "", IDLabel(f.Platform), f.VideoID)
}

// resolutionLabel renders "1920x1080 @60fps", or "" when nothing was probed.
func resolutionLabel(p Part) string {
	if p.Width <= 0 || p.Height <= 0 {
		return ""
	}
	res := fmt.Sprintf("%dx%d", p.Width, p.Height)
	if p.Fps > 0 {
		res += fmt.Sprintf(" @%dfps", p.Fps)
	}
	return res
}

// JobAdded is the embed for a job created by hand — both web add routes and
// both `moombox add` paths, which previously sent three different titles and
// two different field sets (audit C3).
func JobAdded(f JobFacts) (string, string, NotificationType, []Field, SendOptions) {
	fb := NewFieldBuilder().
		AddInlineIf(f.Channel != "", "Channel", EscapeMarkdown(f.Channel))
	addIDField(fb, f).
		AddInlineIf(f.VideoFormat != "", "Video Format", f.VideoFormat).
		AddInlineIf(f.AudioFormat != "", "Audio Format", f.AudioFormat).
		AddIf(f.TimeRange != "", "Time Range", f.TimeRange)

	return "Job Added",
		"Manually added: " + EscapeMarkdown(displayName(f)),
		TypeInfo,
		fb.Build(),
		jobOpts(f, "added")
}

// StreamFound is the discovery embed for both monitors (audit C4). The TITLE
// is shared; the DESCRIPTION stays per platform because the two moments are
// genuinely different — a YouTube find is usually an upcoming stream, a Twitch
// find is live right now.
func StreamFound(f JobFacts) (string, string, NotificationType, []Field, SendOptions) {
	desc := "Found matching stream: " + EscapeMarkdown(displayName(f))
	if f.Platform == "twitch" {
		desc = "Live: " + EscapeMarkdown(displayName(f))
	}
	fb := NewFieldBuilder().
		AddInlineIf(f.Channel != "", "Channel", EscapeMarkdown(f.Channel))
	addIDField(fb, f).
		AddInlineIf(f.Category != "", "Category", EscapeMarkdown(f.Category))

	return "Stream Found", desc, TypeInfo, fb.Build(), jobOpts(f, "found")
}

// JobCancelled is the one cancel embed. The worker's mid-job cancel and the
// route's never-started cancel described the same event under two titles
// (audit C5); which code path noticed is not something an operator can act on.
func JobCancelled(f JobFacts) (string, string, NotificationType, []Field, SendOptions) {
	fb := NewFieldBuilder().
		AddInlineIf(f.Channel != "", "Channel", EscapeMarkdown(f.Channel))
	addIDField(fb, f)

	return "Job Cancelled",
		"Cancelled: " + EscapeMarkdown(displayName(f)),
		TypeCancelled,
		fb.Build(),
		jobOpts(f, "cancelled")
}

// DownloadFinished is the one finished embed. len(parts) == 1 is the shape a
// job that never split produces (File / File Size / Resolution); more than one
// is the split shape (Parts / Qualities / Total Size). Everything job-level —
// the format selection, the trimmed range, the description excerpt, the chat
// count — is now carried by BOTH: the multi-part builder dropped them for no
// reason anyone recorded (audit C1).
//
// The colour is the OUTCOME, not the code path. A job whose recording is
// knowingly short is Warning, because a green "Successfully archived" over a
// truncated archive says the opposite of the truth (audit A5).
//
// With an EMPTY parts slice no File/Parts/Qualities/size/Resolution field is
// rendered at all — the job-level fields still are. That is the contract, not
// an oversight: a caller that produced a file passes at least one Part.
func DownloadFinished(f JobFacts, parts []Part) (string, string, NotificationType, []Field, SendOptions) {
	var totalSize int64
	var totalDuration time.Duration
	var qualities []string
	for _, p := range parts {
		totalSize += p.Size
		totalDuration += p.Duration
		if p.Quality != "" {
			qualities = append(qualities, p.Quality)
		}
	}

	fb := NewFieldBuilder()
	switch {
	case len(parts) == 1:
		fb.AddIf(parts[0].File != "", "File", EscapeMarkdown(parts[0].File))
	case len(parts) > 1:
		// "Parts", not "Segments": Segments is the job-level video/audio
		// sequence counter below, and the two meant different things under one
		// name before this builder existed.
		fb.AddInline("Parts", fmt.Sprintf("%d", len(parts)))
		// Escape each LABEL and join with the RAW separator. Escaping the
		// joined string escapes the " -> " this builder generated — the
		// EscapeMarkdown set includes ">" — and Discord then renders
		// "1080p60 -\> 720p60".
		escaped := make([]string, 0, len(qualities))
		for _, q := range qualities {
			escaped = append(escaped, EscapeMarkdown(q))
		}
		fb.AddInlineIf(len(escaped) > 0, "Qualities", strings.Join(escaped, " -> "))
	}
	if len(parts) > 0 {
		if res := resolutionLabel(parts[0]); res != "" {
			fb.AddInline("Resolution", res)
		}
	}
	if totalSize > 0 {
		name := "File Size"
		if len(parts) > 1 {
			name = "Total Size"
		}
		fb.AddInline(name, utils.FormatFileSize(totalSize))
	}
	fb.AddInlineIf(totalDuration > 0, "Duration", utils.FormatDurationHuman(totalDuration)).
		AddInlineIf(f.TotalTime > 0, "Total Time", utils.FormatDurationHuman(f.TotalTime)).
		AddInlineIf(f.SegmentCounter != "", "Segments", f.SegmentCounter)
	// Non-nil is the whole test: a capture that ran and caught nothing is a
	// fact about the archive ("Chat Messages: 0"), not an unknown, and the
	// single-part site rendered it that way before this builder existed.
	if f.ChatMessages != nil {
		fb.AddInline("Chat Messages", fmt.Sprintf("%d", *f.ChatMessages))
	}
	fb.AddIf(f.FormatSelection != "", "Format Selection", f.FormatSelection).
		AddInlineIf(f.TrimmedRange != "", "Trimmed Range", f.TrimmedRange)
	if f.Description != "" {
		// CLAMP FIRST, escape second — the same order N1's site used: escaping
		// inserts backslashes, and a cut applied afterwards could slice one
		// away from the character it protects.
		fb.Add("Description", EscapeMarkdown(ClampRunes(f.Description, descriptionExcerptLen)))
	}

	// The truth fields last, so they read as the postscript they are.
	ntype := TypeSuccess
	if f.IncompleteTail {
		ntype = TypeWarning
		fb.Add("Tail", "incomplete — Resume appends the rest")
	}
	fb.AddIf(f.ChatIncomplete, "Chat", "incomplete")
	if f.AsideCount > 0 {
		fb.Add("Set-aside recordings", fmt.Sprintf("%d — Recover to mux them", f.AsideCount))
	}

	opts := jobOpts(f, "finished")
	// The finished embed has always used the FULL-WIDTH image rather than the
	// corner thumbnail, so the shared Thumbnail is cleared here rather than
	// shown twice. For Twitch it is cleared and nothing replaces it: a Twitch
	// preview URL 404s the moment the stream ends, so both pictures are broken
	// by the time anybody reads the embed (§0 ruling — the dead Twitch
	// finished image is dropped; multipart upload of the saved thumbnail file
	// is a later option).
	opts.Thumbnail = ""
	if f.Platform != "twitch" {
		opts.Image = f.ThumbnailURL
	}

	return "Download Finished",
		"Successfully archived: " + EscapeMarkdown(displayName(f)),
		ntype,
		fb.Build(),
		opts
}

// TrimCreated is the one trim embed; the single-file and multi-segment paths
// differed only by a Segments field (audit C2).
func TrimCreated(f JobFacts, t TrimFacts) (string, string, NotificationType, []Field, SendOptions) {
	dur := utils.FormatDurationHuman(t.Duration)
	name := EscapeMarkdown(displayName(f))
	fb := NewFieldBuilder().
		// AddIf, for the same reason addIDField guards its own value: an
		// all-unknown JobFacts would otherwise put an empty-valued field on the
		// wire and Discord answers that with a permanent 400.
		AddIf(name != "", "Source Video", name).
		AddInlineIf(t.TimeRange != "", "Time Range", t.TimeRange).
		AddInline("Duration", dur).
		AddInlineIf(t.Parts > 1, "Segments", fmt.Sprintf("%d segments", t.Parts))
	if t.Size != nil {
		fb.AddInline("File Size", utils.FormatFileSize(*t.Size))
	}

	return "Trim Created",
		fmt.Sprintf("Created %s trim from \"%s\"", dur, name),
		TypeInfo,
		fb.Build(),
		jobOpts(f, "trim_created")
}
