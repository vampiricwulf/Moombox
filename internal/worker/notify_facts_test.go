package worker

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestNotifyFactsCarriesTheRowStraightThrough is the base case: the mapper is
// the ONE place a database row becomes a JobFacts, so every field a builder
// reads must arrive.
//
// Mutant: dropping ChannelAvatarURL — every job embed loses its author icon
// and nothing else fails.
func TestNotifyFactsCarriesTheRowStraightThrough(t *testing.T) {
	chID := "UC_abc"
	j := &database.Job{
		ID:               "vid1",
		VideoID:          "vid1",
		Platform:         "youtube",
		Title:            "A Title",
		ChannelName:      "A Channel",
		ChannelAvatarURL: "https://yt3.example/a.jpg",
		ChannelID:        &chID,
		URL:              "https://www.youtube.com/watch?v=vid1",
		ThumbnailURL:     "https://i.ytimg.com/vi/vid1/maxresdefault.jpg",
	}
	f := NotifyFacts(j)
	if f.ID != "vid1" || f.VideoID != "vid1" || f.Platform != "youtube" {
		t.Errorf("identity fields = %q/%q/%q", f.ID, f.VideoID, f.Platform)
	}
	if f.Title != "A Title" || f.Channel != "A Channel" {
		t.Errorf("Title/Channel = %q/%q", f.Title, f.Channel)
	}
	if f.ChannelAvatarURL != j.ChannelAvatarURL {
		t.Errorf("ChannelAvatarURL = %q, want %q", f.ChannelAvatarURL, j.ChannelAvatarURL)
	}
	if f.ThumbnailURL != j.ThumbnailURL {
		t.Errorf("ThumbnailURL = %q", f.ThumbnailURL)
	}
	if f.ChannelURL != "https://www.youtube.com/channel/UC_abc" {
		t.Errorf("ChannelURL = %q, want the channel page derived from channel_id", f.ChannelURL)
	}
}

// TestNotifyFactsFallsBackToTheWatchURL folds the identical fallback that the
// cancel route and setJobError each carried separately.
//
// Mutants this kill:
//   - dropping the fallback: a row with no url produces an embed whose title
//     links nowhere.
//   - applying it to Twitch: twitch.tv rows would get a youtube.com link.
func TestNotifyFactsFallsBackToTheWatchURL(t *testing.T) {
	yt := NotifyFacts(&database.Job{ID: "v", VideoID: "v", Platform: "youtube"})
	if yt.URL != "https://www.youtube.com/watch?v=v" {
		t.Errorf("URL = %q, want the watch-URL fallback", yt.URL)
	}
	tw := NotifyFacts(&database.Job{ID: "tw_1", VideoID: "1", Platform: "twitch"})
	if tw.URL != "" {
		t.Errorf("URL = %q for a twitch row with no url, want empty — there is no watch-URL to guess", tw.URL)
	}
}

// TestNotifyFactsDerivesNoChannelURLWithoutAChannelID covers manual and Twitch
// rows, whose channel_id is NULL.
//
// Mutant: dereferencing ChannelID unconditionally — a nil pointer panic on
// every manually added job.
func TestNotifyFactsDerivesNoChannelURLWithoutAChannelID(t *testing.T) {
	empty := ""
	for name, j := range map[string]*database.Job{
		"nil channel id":   {ID: "v", VideoID: "v", Platform: "youtube"},
		"empty channel id": {ID: "v", VideoID: "v", Platform: "youtube", ChannelID: &empty},
		"twitch":           {ID: "tw_1", VideoID: "1", Platform: "twitch"},
	} {
		if got := NotifyFacts(j).ChannelURL; got != "" {
			t.Errorf("%s: ChannelURL = %q, want empty", name, got)
		}
	}
}

// TestNotifyFactsIsNilSafe: the mapper is called from error paths.
func TestNotifyFactsIsNilSafe(t *testing.T) {
	if got := NotifyFacts(nil); got.ID != "" {
		t.Errorf("NotifyFacts(nil) = %+v, want the zero JobFacts", got)
	}
}
