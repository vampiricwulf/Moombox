package main

import (
	"fmt"
	"os"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// addVideo adds a video/stream to the queue from the command line.
// Mirrors TypeScript's addVideo() from index.ts, including notification dispatch.
func addVideo(input string) {
	target := utils.ExtractMediaID(input)
	if target == nil {
		fmt.Fprintf(os.Stderr, "Invalid video ID or URL: %s\n", input)
		os.Exit(1)
	}

	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	if !cfg.ConfigLoaded {
		// config.Load falls back to Defaults() without error when no config
		// file exists — proceeding would silently create a fresh
		// ./moombox.db in the CURRENT directory and report success while
		// the daemon's real database never sees the job.
		fmt.Fprintln(os.Stderr, "No config.toml found — run `moombox add` from the Moombox daemon's directory so the job lands in the daemon's database.")
		os.Exit(1)
	}

	// Refuse a schema-version mismatch instead of migrating: database.Open
	// migrates unconditionally, and doing that to the daemon's live DB from
	// this side process (e.g. when the on-disk binary is newer than the
	// running daemon during an update window) would leave the daemon's old
	// code writing against a new schema. Also catches a missing DB file —
	// the daemon, not `add`, should create it.
	if v, verr := database.FileSchemaVersion(cfg.Paths.DatabasePath); verr != nil {
		fmt.Fprintf(os.Stderr, "Failed to inspect database %s: %v\nStart the Moombox daemon first.\n", cfg.Paths.DatabasePath, verr)
		os.Exit(1)
	} else if v != database.CurrentSchemaVersion() {
		fmt.Fprintf(os.Stderr, "Database schema v%d does not match this binary (v%d) — start the Moombox daemon to migrate, then retry.\n", v, database.CurrentSchemaVersion())
		os.Exit(1)
	}

	db, err := database.Open(cfg.Paths.DatabasePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Init notification manager for "Video Added" dispatch (matches TS addVideo)
	notifyMgr := notifications.NewManager(cfg, &nopLogger{})

	now := time.Now().UTC().Format(time.RFC3339)

	if target.Platform == "twitch" {
		tw := target.Twitch
		if tw.Type == utils.TwitchClip {
			fmt.Fprintln(os.Stderr, "Twitch clips are not supported.")
			os.Exit(1)
		}

		var jobID, jobURL, channelName string
		if tw.Type == utils.TwitchVOD {
			jobID = "tw_v" + tw.Value
			jobURL = "https://www.twitch.tv/videos/" + tw.Value
			channelName = "Manual"
		} else {
			jobID = tw.Value // Will be resolved by the worker
			jobURL = "https://www.twitch.tv/" + tw.Value
			channelName = tw.Value
		}

		if db.JobExists(jobID) {
			fmt.Printf("Job already exists: %s\n", jobID)
			return
		}

		job := &database.Job{
			ID:            jobID,
			VideoID:       jobID,
			URL:           jobURL,
			Title:         "Manual Add",
			ChannelName:   channelName,
			Platform:      "twitch",
			Status:        database.StatusUpcoming,
			ManuallyAdded: true,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		added, err := db.AddJob(job)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to add job: %v\n", err)
			os.Exit(1)
		}
		if added {
			fmt.Printf("Added Twitch %s %s to queue.\n", tw.Type, jobID)
			notifyMgr.Send("Video Added",
				fmt.Sprintf("Manually added Twitch %s: %s", tw.Type, jobID),
				notifications.TypeInfo,
				[]notifications.Field{{Name: "ID", Value: jobID, Inline: true}},
				notifications.SendOptions{URL: jobURL, Event: "added"})
		} else {
			fmt.Printf("Failed to add %s (may already exist).\n", jobID)
		}
	} else {
		// YouTube
		videoID := target.VideoID
		if db.JobExists(videoID) {
			fmt.Printf("Job already exists for video: %s\n", videoID)
			return
		}

		videoURL := "https://www.youtube.com/watch?v=" + videoID
		job := &database.Job{
			ID:            videoID,
			VideoID:       videoID,
			URL:           videoURL,
			Title:         "Manual Add",
			ChannelName:   "Manual",
			Platform:      "youtube",
			Status:        database.StatusUpcoming,
			ManuallyAdded: true,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		added, err := db.AddJob(job)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to add job: %v\n", err)
			os.Exit(1)
		}
		if added {
			fmt.Printf("Added %s to queue.\n", videoID)
			notifyMgr.Send("Video Added",
				fmt.Sprintf("Manually added: %s", videoID),
				notifications.TypeInfo,
				[]notifications.Field{{Name: "Video ID", Value: videoID, Inline: true}},
				notifications.SendOptions{URL: videoURL, Event: "added"})
		} else {
			fmt.Printf("Failed to add %s (may already exist).\n", videoID)
		}
	}

	// Wait for in-flight notification dispatches to finish. Manager.Wait has
	// its own 30s timeout; calling it here replaces the previous unbounded
	// 500ms sleep with a deterministic flush so we do not lose notifications
	// when a webhook is slow and do not linger when they are fast.
	notifyMgr.Wait()
}
