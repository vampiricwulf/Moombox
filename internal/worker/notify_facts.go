package worker

import (
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// NotifyFacts turns a job row into the notifications.JobFacts every job embed
// is built from.
//
// It lives in internal/worker rather than in internal/notifications because
// notifications must not import internal/database — internal/tui imports
// notifications, and a row type there would breach the TUI import fence
// through the back door. internal/worker is the package the other two
// producers (internal/web/routes and cmd/moombox) already depend on, so one
// mapper serves all three; a site that knows more than the row assigns to the
// returned struct instead of writing a second mapper.
//
// Two fallbacks are folded in here because both were duplicated at their call
// sites before:
//
//   - The YouTube watch URL, which the cancel route and setJobError each
//     reconstructed when the row's url was empty. It is YouTube-only on
//     purpose: there is no URL to guess for a Twitch row.
//   - The channel page, derived from channel_id. Only the feed and DECAPI
//     creators set channel_id, so manual and Twitch rows get no author link
//     (the author line still carries the name and the avatar).
func NotifyFacts(j *database.Job) notifications.JobFacts {
	if j == nil {
		return notifications.JobFacts{}
	}
	f := notifications.JobFacts{
		ID:               j.ID,
		VideoID:          j.VideoID,
		Platform:         j.Platform,
		Title:            j.Title,
		Channel:          j.ChannelName,
		ChannelAvatarURL: j.ChannelAvatarURL,
		URL:              j.URL,
		ThumbnailURL:     j.ThumbnailURL,
	}
	if f.URL == "" && j.Platform != "twitch" && j.VideoID != "" {
		f.URL = "https://www.youtube.com/watch?v=" + j.VideoID
	}
	if j.Platform != "twitch" && j.ChannelID != nil && *j.ChannelID != "" {
		f.ChannelURL = "https://www.youtube.com/channel/" + *j.ChannelID
	}
	return f
}

// notifyAuthor is this package's copy of the builders' authorFor
// (internal/notifications/builders.go): the embed's author line, or nil when
// the channel is unknown.
//
// A copy because authorFor is unexported and the builders are the only callers
// inside that package — the three job sends this package assembles by hand
// (the "Job Failed" embed, the per-job "Authentication Required", and the
// Twitch chat downgrade) cannot reach it. One copy here rather than three
// inline blocks is what makes "the same row produces the same Author" true by
// construction rather than by three sites agreeing.
//
// The name is RAW for the same reason it is there: Discord renders no markdown
// in the author bar, so escaping would show the backslashes.
func notifyAuthor(f notifications.JobFacts) *notifications.Author {
	if f.Channel == "" {
		return nil
	}
	return &notifications.Author{Name: f.Channel, IconURL: f.ChannelAvatarURL, URL: f.ChannelURL}
}
