package main

import (
	"github.com/vampiricwulf/Moombox/internal/notifications"
)

// cliAddedFacts describes a job added from the command line.
//
// `moombox add` runs no metadata fetch, so the row it writes carries the
// placeholders "Manual Add" and "Manual"; passing those through would make the
// embed read "Manually added: Manual Add" with a Channel field that names
// nothing. The facts are therefore built from what the COMMAND knows rather
// than from the row: an empty Title (the builder falls back to the id), the
// derived YouTube thumbnail, and — for a Twitch live add, where the target IS
// a channel login — the channel name and its page.
//
// channelLogin is empty for YouTube and for a Twitch VOD add, whose target is
// a video id.
func cliAddedFacts(platform, jobID, jobURL, channelLogin string) notifications.JobFacts {
	f := notifications.JobFacts{
		ID:       jobID,
		VideoID:  jobID,
		Platform: platform,
		URL:      jobURL,
	}
	switch {
	case platform == "twitch":
		if channelLogin != "" {
			f.Channel = channelLogin
			f.ChannelURL = "https://www.twitch.tv/" + channelLogin
		}
	default:
		f.ThumbnailURL = youtubeThumbnailURL(jobID)
	}
	return f
}
