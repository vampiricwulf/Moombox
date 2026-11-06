package routes

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/utils"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// TwitchListingFetcher reads the Twitch listings the add route queues (owner
// decisions D18 and FE-27). The route type-asserts it off its
// TwitchMetadataFetcher, so a fetcher that does not read listings (a test's)
// answers a listing with 503 rather than a job.
type TwitchListingFetcher interface {
	// ListChannelVideos is a /<ch>/videos listing, newest first:
	// twitch.API.ListChannelVideos with the ?filter= type and the ?sort=.
	ListChannelVideos(ctx context.Context, login, broadcastType, sort string) ([]twitch.TwitchVideo, error)
	// CollectionVideos is a /collections/<id> listing in its own order.
	CollectionVideos(ctx context.Context, collectionID string) (string, []twitch.TwitchVideo, error)
	// ChannelCollectionVideos is a /<ch>/videos?filter=collections listing:
	// the videos of every collection the channel lists
	// (twitch.API.ChannelCollectionVideos).
	ChannelCollectionVideos(ctx context.Context, login string) ([]twitch.TwitchVideo, error)
}

// TwitchListingType is the twitchType an add request names a listing by. The
// URL itself is parsed here, through utils.ExtractTwitchTarget, the one table
// the dashboard, the TUI and the CLI follow.
const TwitchListingType = "listing"

// twitchListingResponse is the add route's answer to a listing: how many VOD
// jobs it queued and how many it skipped because a job already exists.
type twitchListingResponse struct {
	Title   string `json:"title,omitempty"`
	Listed  int    `json:"listed"`
	Added   int    `json:"added"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed,omitempty"`
}

// addTwitchListing answers POST /api/jobs for a Twitch channel video list or
// collection: it reads the listing and queues each VOD as a Queued tw_v job
// (worker.QueueTwitchVideos), which the archive-slots scheduler admits
// archive_slots at a time. 201 with the counts when anything was listed, 404
// for a channel or collection Twitch does not have, 400 for a URL that names
// neither. A channel's collection list (/<ch>/videos?filter=collections,
// yt-dlp's TwitchVideosCollectionsIE) queues the VODs of every collection it
// lists (owner decision FE-27), where it used to be refused.
//
// No "Job Added" notification is sent per VOD: a catalogue paste queues
// hundreds, and each announces itself with "Twitch Download Starting" when the
// scheduler admits it — paced, as a YouTube backlog VOD does
// (announcesJobFound).
func addTwitchListing(rw http.ResponseWriter, req *http.Request, db *database.Database, store *config.Store,
	w *worker.DownloadWorker, fetcher TwitchMetadataFetcher, rawURL, qualityPref, outputDir string) {
	target := utils.ExtractTwitchTarget(rawURL)
	if target == nil || (target.Type != utils.TwitchVideosListing && target.Type != utils.TwitchCollection) {
		if msg := target.Unsupported(); msg != "" {
			jsonError(rw, msg, http.StatusBadRequest)
			return
		}
		jsonError(rw, "not a Twitch video list or collection URL", http.StatusBadRequest)
		return
	}
	filter, sort := utils.TwitchVideosQuery(rawURL)
	lister, ok := fetcher.(TwitchListingFetcher)
	if !ok || fetcher == nil {
		jsonError(rw, "Twitch listings are unavailable", http.StatusServiceUnavailable)
		return
	}

	var (
		videos []twitch.TwitchVideo
		title  string
		err    error
		opts   = worker.TwitchListingOptions{ManuallyAdded: true, QualityPreference: qualityPref, OutputDirectory: outputDir}
	)
	if store != nil {
		store.Read(func(c *config.MoomboxConfig) { opts.Channels = append([]config.ChannelConfig(nil), c.Channels...) })
	}
	switch {
	case target.Type == utils.TwitchCollection:
		title, videos, err = lister.CollectionVideos(req.Context(), target.Value)
	case filter == "collections":
		// The collections' VODs are the channel's own: a node without an
		// owner is paced under it, as a video list's is.
		opts.Login = target.Value
		videos, err = lister.ChannelCollectionVideos(req.Context(), target.Value)
	default:
		opts.Login = target.Value
		videos, err = lister.ListChannelVideos(req.Context(), target.Value, twitch.VideoListBroadcastType(filter), sort)
	}
	switch {
	case errors.Is(err, twitch.ErrChannelNotFound):
		jsonError(rw, "Twitch channel not found", http.StatusNotFound)
		return
	case errors.Is(err, twitch.ErrCollectionNotFound):
		jsonError(rw, "Twitch collection not found", http.StatusNotFound)
		return
	case err != nil:
		jsonError(rw, "failed to read the Twitch listing", http.StatusBadGateway)
		return
	}
	if len(videos) == 0 {
		jsonError(rw, "the Twitch listing has no videos", http.StatusNotFound)
		return
	}
	res := worker.QueueTwitchVideos(db, videos, opts, time.Now())
	if len(res.Added) > 0 && w != nil && w.Scheduler() != nil {
		w.Scheduler().Wake()
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusCreated)
	jsonResponse(rw, twitchListingResponse{
		Title: title, Listed: len(videos), Added: len(res.Added), Skipped: res.Skipped, Failed: res.Failed,
	})
}
