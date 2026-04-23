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
