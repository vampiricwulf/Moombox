package routes

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestTwitchAddAcceptsAPastedClip is owner decision D26 follow-up 2's API
// half: a pasted clip always makes a job — sent as a videoId with twitchType
// "clip", or as nothing but its URL — a VOD-style row "tw_clip_<slug>" with
// the clip's metadata, the slug's case kept, and its URL the canonical
// /<login>/clip/<slug> once the broadcaster's login is known. The same clip
// posted again is a 409 (the dedupe by slug), a clip Twitch says does not
// exist a 404 with no row, and a slug outside the URL table's shape a 400.
//
// Mutants: the clip URL branch in the body.URL parse dropped (400 "video ID
// or URL required"); the clip slug held to the login regexp ("-" refused,
// 400); isClip not setting is_vod; the ErrClipUnavailable arm dropped (201).
func TestTwitchAddAcceptsAPastedClip(t *testing.T) {
	f := newJobsFixture(t)
	f.tw.clipMeta = &TwitchJobMetadata{Title: "A day in the life of xQc", ChannelName: "xQc", ChannelLogin: "xqc",
		ThumbnailURL: "https://clips-media-assets2.twitch.tv/p.jpg", StartedAt: "2025-03-25T02:26:55Z", GameCategory: "Just Chatting"}

	const slug = "CulturedAmazingKuduDatSheffy-TiZ_-ixAGYR3y2Uy"
	rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{"url": "https://www.twitch.tv/xqc/clip/" + slug})
	if rec.Code != http.StatusCreated {
		t.Fatalf("pasted clip URL: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	job, err := f.db.GetJob(twitch.ClipJobID(slug))
	if err != nil || job == nil {
		t.Fatalf("no clip row: %v", err)
	}
	if job.Platform != "twitch" || !job.IsVod || job.Title != "xQc — A day in the life of xQc" || job.ChannelName != "xQc" ||
		job.URL != "https://www.twitch.tv/xqc/clip/"+slug || job.StreamStartTime != "2025-03-25T02:26:55Z" ||
		job.TwitchCategory != "Just Chatting" || job.ThumbnailURL != "https://clips-media-assets2.twitch.tv/p.jpg" {
		t.Errorf("clip row = %+v", job)
	}
	if len(f.tw.clipSlugs) != 1 || f.tw.clipSlugs[0] != slug {
		t.Errorf("clip metadata asked for %v, want the slug as pasted", f.tw.clipSlugs)
	}

	rec = doRequest(t, f.router, "POST", "/api/jobs", map[string]any{"platform": "twitch", "videoId": slug, "twitchType": "clip"})
	if rec.Code != http.StatusConflict {
		t.Errorf("the same clip again: want 409, got %d (%s)", rec.Code, rec.Body.String())
	}

	f.tw.clipErr = fmt.Errorf("read: %w", twitch.ErrClipUnavailable)
	rec = doRequest(t, f.router, "POST", "/api/jobs", map[string]any{"platform": "twitch", "videoId": "GoneClipSlug", "twitchType": "clip"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("a clip Twitch has not: want 404, got %d (%s)", rec.Code, rec.Body.String())
	}
	if j, _ := f.db.GetJob(twitch.ClipJobID("GoneClipSlug")); j != nil {
		t.Error("the refused clip has a row")
	}

	rec = doRequest(t, f.router, "POST", "/api/jobs", map[string]any{"platform": "twitch", "videoId": "../x", "twitchType": "clip"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed slug: want 400, got %d", rec.Code)
	}
}
