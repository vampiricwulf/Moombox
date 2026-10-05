package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// printUsage is flag.Usage: the daemon's flags and the add subcommand.
func printUsage() {
	out := flag.CommandLine.Output()
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  moombox [flags]                                run the archiver")
	fmt.Fprintln(out, "  moombox [-config path] add <video_id_or_url>   queue a video and exit")
	fmt.Fprintln(out, "\nFlags:")
	flag.PrintDefaults()
}

// runAddCommand parses `add`'s own arguments (a -config may follow the
// subcommand as well as precede it) and runs addVideo.
func runAddCommand(configPath string, args []string) {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	fs.Usage = printUsage
	cfgPath := fs.String("config", configPath, "Path to config file")
	_ = fs.Parse(args) // ExitOnError: a bad flag exits here
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: moombox [-config path] add <video_id_or_url>")
		os.Exit(1)
	}
	addVideo(fs.Arg(0), *cfgPath)
}

// addVideo adds a video/stream to the queue from the command line.
// Mirrors TypeScript's addVideo() from index.ts, including notification dispatch.
// configPath is the -config flag: empty runs config.Load's search, the same
// one the daemon runs.
func addVideo(input, configPath string) {
	target := utils.ExtractMediaID(input)
	if target == nil {
		fmt.Fprintf(os.Stderr, "Invalid video ID or URL: %s\n", input)
		os.Exit(1)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}
	if !cfg.ConfigLoaded {
		// config.Load falls back to Defaults() without error when no config
		// file exists — proceeding would silently create a fresh
		// ./moombox.db in the CURRENT directory and report success while
		// the daemon's real database never sees the job.
		if configPath != "" {
			fmt.Fprintf(os.Stderr, "No config file at %s — pass the -config the daemon runs with so the job lands in the daemon's database.\n", configPath)
		} else {
			fmt.Fprintln(os.Stderr, "No config.toml found — run `moombox add` from the Moombox daemon's directory, or pass its -config, so the job lands in the daemon's database.")
		}
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
	// `added` is a lifecycle event, and this side process writes it against
	// the same database the daemon reads — without the store an edit-mode
	// target would open a message here that the daemon could never edit.
	notifyMgr.SetMessageStore(db)

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
			kind := "channel"
			if tw.Type == utils.TwitchVOD {
				kind = "VOD"
			}
			fmt.Printf("Added Twitch %s %s to queue.\n", kind, jobID)
			login := ""
			if tw.Type != utils.TwitchVOD {
				login = tw.Value
			}
			notifyMgr.Send(notifications.JobAdded(cliAddedFacts("twitch", jobID, jobURL, login)))
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
			notifyMgr.Send(notifications.JobAdded(cliAddedFacts("youtube", videoID, videoURL, "")))
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
