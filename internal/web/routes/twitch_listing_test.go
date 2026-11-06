package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/web"
)

// fakeTwitchLister is a TwitchMetadataFetcher that also reads listings: it
// records what it was asked and answers from its fields.
type fakeTwitchLister struct {
	fakeTwitchFetcher
	videos     []twitch.TwitchVideo
	err        error
	gotLogin   string
	gotType    string
	gotSort    string
	gotCollect string
	gotOwner   string
}

func (f *fakeTwitchLister) ListChannelVideos(_ context.Context, login, broadcastType, sort string) ([]twitch.TwitchVideo, error) {
	f.gotLogin, f.gotType, f.gotSort = login, broadcastType, sort
	return f.videos, f.err
}

func (f *fakeTwitchLister) CollectionVideos(_ context.Context, id string) (string, []twitch.TwitchVideo, error) {
	f.gotCollect = id
	return "Playthrough Archives", f.videos, f.err
}

func (f *fakeTwitchLister) ChannelCollectionVideos(_ context.Context, login string) ([]twitch.TwitchVideo, error) {
	f.gotOwner = login
	return f.videos, f.err
}

func newListingRouter(t *testing.T, tw TwitchMetadataFetcher, channels []config.ChannelConfig) (chi.Router, *database.Database) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Defaults()
	cfg.Channels = channels
	store := config.NewStore(cfg, filepath.Join(dir, "config.toml"))
	rl := web.NewRateLimiter(1000, time.Minute)
	t.Cleanup(rl.Close)
	r := chi.NewRouter()
	JobRoutes(r, db, store, nil, rl, tw, &fakeYouTubeFetcher{}, notificationtest.New())
	return r, db
}

func postListing(t *testing.T, r chi.Router, url string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"platform": "twitch", "twitchType": TwitchListingType, "url": url})
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestAddTwitchVideosListingQueuesEachVod is owner decision D18: a pasted
// /<ch>/videos?filter=archives&sort=views is read with the filter's type and
// the sort, and each VOD becomes a Queued tw_v row at queue_priority 1 under
// the channel's ID — the archive-slots scheduler's to admit — while a VOD that
// already has a job is skipped.
//
// Mutants: the listing branch removed from the add route (the URL is read as
// a channel and gets a live-wait job, or a 400); the filter's type not passed
// (gotType ""); rows created Upcoming (the status check fails).
func TestAddTwitchVideosListingQueuesEachVod(t *testing.T) {
	tw := &fakeTwitchLister{videos: []twitch.TwitchVideo{
		{ID: "300", Title: "Day three", PublishedAt: "2026-10-03T12:00:00Z", LengthSeconds: 3600},
		{ID: "200", Title: "", PublishedAt: "2026-10-02T12:00:00Z"},
		{ID: "100", Title: "Day one", PublishedAt: "2026-10-01T12:00:00Z"},
	}}
	r, db := newListingRouter(t, tw, []config.ChannelConfig{{ID: "Spamfish", Platform: "twitch", QualityPreference: "720p60"}})
	if _, err := db.AddJob(&database.Job{ID: "tw_v100", VideoID: "tw_v100", Platform: "twitch", Status: database.StatusFinished}); err != nil {
		t.Fatal(err)
	}

	rec := postListing(t, r, "https://www.twitch.tv/spamfish/videos?filter=archives&sort=views")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var res twitchListingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Listed != 3 || res.Added != 2 || res.Skipped != 1 {
		t.Fatalf("response %s, want 3 listed, 2 added, 1 skipped", rec.Body)
	}
	if tw.gotLogin != "spamfish" || tw.gotType != "ARCHIVE" || tw.gotSort != "views" {
		t.Errorf("listed (%q, %q, %q), want the login, ARCHIVE and the sort", tw.gotLogin, tw.gotType, tw.gotSort)
	}
	for id, title := range map[string]string{"tw_v300": "spamfish — Day three", "tw_v200": "spamfish — Untitled Broadcast"} {
		j, err := db.GetJob(id)
		if err != nil || j == nil {
			t.Fatalf("%s not created: %v", id, err)
		}
		if j.Status != database.StatusQueued || j.QueuePriority != 1 || j.ChannelID == nil || *j.ChannelID != "Spamfish" ||
			!j.IsVod || !j.ManuallyAdded || j.Title != title || j.QualityPreference != "720p60" ||
			j.URL != "https://www.twitch.tv/videos/"+strings.TrimPrefix(id, "tw_v") {
			t.Errorf("%s = %+v; want a Queued backlog VOD row of channel Spamfish titled %q", id, j, title)
		}
	}
	if seen, _ := db.HasProcessed("tw_v300"); !seen {
		t.Error("a queued VOD is not in history")
	}
}

// TestAddTwitchCollectionQueuesEachVod: /collections/<id> is read as the
// collection, and each VOD is queued under its owner's login.
//
// Mutant: the collection branch reading it as a channel video list (gotCollect
// empty).
func TestAddTwitchCollectionQueuesEachVod(t *testing.T) {
	tw := &fakeTwitchLister{videos: []twitch.TwitchVideo{{ID: "7", Title: "Part 1"}}}
	tw.videos[0].Owner = &struct {
		Login       string `json:"login"`
		DisplayName string `json:"displayName"`
	}{Login: "Monstercat", DisplayName: "Monstercat"}
	r, db := newListingRouter(t, tw, nil)
	rec := postListing(t, r, "https://www.twitch.tv/collections/o9zZer3IQBhTJw")
	if rec.Code != http.StatusCreated || tw.gotCollect != "o9zZer3IQBhTJw" {
		t.Fatalf("status %d, collection %q: %s", rec.Code, tw.gotCollect, rec.Body)
	}
	j, _ := db.GetJob("tw_v7")
	if j == nil || j.Status != database.StatusQueued || j.ChannelID == nil || *j.ChannelID != "monstercat" || j.Title != "Monstercat — Part 1" {
		t.Errorf("tw_v7 = %+v, want a Queued row of monstercat", j)
	}
}

// TestAddTwitchCollectionListQueuesEveryCollectionsVods is owner decision
// FE-27: /<ch>/videos?filter=collections — yt-dlp's TwitchVideosCollectionsIE,
// which TwitchCollectionsListingRefusal used to turn away — is read as the
// channel's collections, never as its video list, and each VOD is queued as a
// Queued backlog row of the channel, a node without an owner taking the
// channel's login.
//
// Mutants: the collections case removed (read as the video list: gotOwner
// "", gotLogin "spamfish"); opts.Login not set (the owner-less VOD names no
// channel and is not queued).
func TestAddTwitchCollectionListQueuesEveryCollectionsVods(t *testing.T) {
	tw := &fakeTwitchLister{videos: []twitch.TwitchVideo{{ID: "41", Title: "Act 1"}, {ID: "42", Title: "Act 2"}}}
	r, db := newListingRouter(t, tw, []config.ChannelConfig{{ID: "Spamfish", Platform: "twitch"}})
	rec := postListing(t, r, "https://www.twitch.tv/spamfish/videos?filter=collections")
	if rec.Code != http.StatusCreated || tw.gotOwner != "spamfish" || tw.gotLogin != "" {
		t.Fatalf("status %d, collections of %q, video list of %q: %s", rec.Code, tw.gotOwner, tw.gotLogin, rec.Body)
	}
	var res twitchListingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Listed != 2 || res.Added != 2 {
		t.Fatalf("response %s, want 2 listed and added", rec.Body)
	}
	for _, id := range []string{"tw_v41", "tw_v42"} {
		j, _ := db.GetJob(id)
		if j == nil || j.Status != database.StatusQueued || j.ChannelID == nil || *j.ChannelID != "Spamfish" {
			t.Errorf("%s = %+v, want a Queued row of channel Spamfish", id, j)
		}
	}
}

// TestAddTwitchListingRefusals: a clip list, a channel Twitch does not have
// and a fetcher that cannot read listings each queue nothing.
//
// Mutant: the not-found mapping removed (502).
func TestAddTwitchListingRefusals(t *testing.T) {
	cases := []struct {
		name string
		url  string
		tw   TwitchMetadataFetcher
		want int
	}{
		{"unknown channel's collections", "https://www.twitch.tv/nobody/videos?filter=collections", &fakeTwitchLister{err: twitch.ErrChannelNotFound}, http.StatusNotFound},
		{"clip list", "https://www.twitch.tv/spamfish/clips", &fakeTwitchLister{videos: []twitch.TwitchVideo{{ID: "1"}}}, http.StatusBadRequest},
		{"unknown channel", "https://www.twitch.tv/nobody/videos", &fakeTwitchLister{err: twitch.ErrChannelNotFound}, http.StatusNotFound},
		{"no lister", "https://www.twitch.tv/spamfish/videos", &fakeTwitchFetcher{}, http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, db := newListingRouter(t, c.tw, nil)
			if rec := postListing(t, r, c.url); rec.Code != c.want {
				t.Errorf("status %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
			if jobs, _ := db.GetAllJobs(); len(jobs) != 0 {
				t.Errorf("%d jobs created, want none", len(jobs))
			}
		})
	}
}

// TestJobCreateFromTwitchURLRoutesEveryKind is owner decision FE-5: a POST
// /api/jobs carrying only a Twitch `url` routes it by the shared URL table
// (utils.ExtractTwitchTarget, G35), as the dashboard routes the same paste —
// a VOD URL (/<ch>/v/<id>, schedule?vodID=) is its tw_v job, a video list,
// a collection list and a collection queue their VODs (addTwitchListing),
// a clip list gets the shared refusal, and a channel URL on the m. or go.
// host still makes the channel's job. (A clip URL makes the clip's own job,
// owner decision D26: TestTwitchAddAcceptsAPastedClip.) Before FE-5 the VOD
// and listing URLs were a 400, and before G35 a channel's live-wait job.
//
// Mutants: the URL-only routing removed (the VOD and listing rows are 400);
// its VOD arm dropped (the VOD rows are 400); its listing arm dropped (the
// listing rows are 400); its clip-list arm dropped (the clip list is "video
// ID or URL required", not the shared refusal); the platform guard dropped
// (the youtube-platform row becomes a Twitch job).
func TestJobCreateFromTwitchURLRoutesEveryKind(t *testing.T) {
	vods := map[string]string{
		"https://www.twitch.tv/riotgames/v/6528877":                    "tw_v6528877",
		"https://www.twitch.tv/northernlion/schedule?vodID=1234567890": "tw_v1234567890",
	}
	for u, id := range vods {
		r, db := newListingRouter(t, &fakeTwitchLister{}, nil)
		rec := doRequest(t, r, "POST", "/api/jobs", map[string]any{"url": u})
		if rec.Code != http.StatusCreated {
			t.Errorf("%s: want 201, got %d (body: %s)", u, rec.Code, rec.Body.String())
			continue
		}
		if j, _ := db.GetJob(id); j == nil || !j.IsVod || j.Platform != "twitch" || j.URL != "https://www.twitch.tv/videos/"+strings.TrimPrefix(id, "tw_v") {
			t.Errorf("%s: %s = %+v, want the VOD's job", u, id, j)
		}
	}

	listings := []struct {
		url                   string
		login, collect, owner string
	}{
		{"https://www.twitch.tv/spamfish/videos", "spamfish", "", ""},
		{"https://www.twitch.tv/collections/o9zZer3IQBhTJw", "", "o9zZer3IQBhTJw", ""},
		{"https://www.twitch.tv/spamfish/videos?filter=collections", "", "", "spamfish"},
	}
	for _, l := range listings {
		tw := &fakeTwitchLister{videos: []twitch.TwitchVideo{{ID: "77", Title: "x"}}}
		tw.videos[0].Owner = &struct {
			Login       string `json:"login"`
			DisplayName string `json:"displayName"`
		}{Login: "spamfish"}
		r, db := newListingRouter(t, tw, nil)
		rec := doRequest(t, r, "POST", "/api/jobs", map[string]any{"url": l.url})
		if rec.Code != http.StatusCreated || tw.gotLogin != l.login || tw.gotCollect != l.collect || tw.gotOwner != l.owner {
			t.Errorf("%s: %d, read (%q, %q, %q), want 201 reading (%q, %q, %q): %s", l.url, rec.Code,
				tw.gotLogin, tw.gotCollect, tw.gotOwner, l.login, l.collect, l.owner, rec.Body.String())
			continue
		}
		if j, _ := db.GetJob("tw_v77"); j == nil || j.Status != database.StatusQueued {
			t.Errorf("%s: tw_v77 = %+v, want a Queued VOD", l.url, j)
		}
	}

	r, db := newListingRouter(t, &fakeTwitchLister{videos: []twitch.TwitchVideo{{ID: "1"}}}, nil)
	rec := doRequest(t, r, "POST", "/api/jobs", map[string]any{"url": "https://www.twitch.tv/spamfish/clips"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Twitch clip lists are not supported yet.") {
		t.Errorf("clip list: %d %s, want 400 with the shared refusal", rec.Code, rec.Body.String())
	}
	// A caller that says the platform is YouTube is not second-guessed.
	rec = doRequest(t, r, "POST", "/api/jobs", map[string]any{"url": "https://www.twitch.tv/videos/6528877", "platform": "youtube"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("youtube platform with a Twitch VOD URL: %d %s, want 400", rec.Code, rec.Body.String())
	}
	if jobs, _ := db.GetAllJobs(); len(jobs) != 0 {
		t.Errorf("%d jobs created by refused posts, want none", len(jobs))
	}

	for _, u := range []string{"https://go.twitch.tv/Shroud", "https://m.twitch.tv/shroud"} {
		f := newJobsFixture(t)
		rec := doRequest(t, f.router, "POST", "/api/jobs", map[string]any{"url": u})
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Errorf("%s: want the channel's job, got %d (body: %s)", u, rec.Code, rec.Body.String())
			continue
		}
		var body struct {
			ChannelName string `json:"channelName"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.ChannelName != "" && body.ChannelName != "shroud" {
			t.Errorf("%s: channel = %q, want shroud", u, body.ChannelName)
		}
	}
}
