package jobstate

import (
	"fmt"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// ChatOffsetBasis names the clock a recording's chat sits on in the
// dashboard player, which decides how far the suggested chat offset reaches
// (owner ruling RV-19).
type ChatOffsetBasis string

const (
	// ChatOffsetBasisIngest is a recording with its file-zero wall clock
	// (Job.MediaStartTime): chat is placed on the ingest clock exactly, so a
	// message shows at the moment its sender's video was ingested, and the
	// suggestion is the whole delay a viewer watched behind ingest.
	ChatOffsetBasisIngest ChatOffsetBasis = "ingest"
	// ChatOffsetBasisStart is a recording placed by the broadcast's start
	// time (no MediaStartTime: from before v30, or its first segment carried
	// no wall clock). That baseline already sits about 5 s after ingest, so
	// the suggestion is the viewer delay less those 5 s.
	ChatOffsetBasisStart ChatOffsetBasis = "start"
)

// The suggested chat offsets, in seconds: how far behind YouTube's ingest a
// typical viewer of a broadcast in each latency mode watched, so how much
// sooner the player shows chat to match what the chatters were reacting to.
// Typical, not measured per recording. The ingest figures are the segment
// length plus the player's readahead YouTube sends for each mode
// (2026-11-27 research: Normal 5 s segments and 10-12 s readahead, Low 2 s
// and 4-4.8 s, Ultra low 1 s and 1-1.6 s), rounded up, and agree with
// YouTube's help, which promises Low "less than 10 seconds" and Ultra low
// "less than 5 seconds" of delay. The start figures are those less the 5 s
// the start-time baseline already sits behind ingest, never below 0. Built
// in, one per mode, not configurable (owner ruling RV-19 c).
const (
	SuggestedChatOffsetIngestNormal   = 20
	SuggestedChatOffsetIngestLow      = 7
	SuggestedChatOffsetIngestUltraLow = 4

	SuggestedChatOffsetStartNormal   = 15
	SuggestedChatOffsetStartLow      = 2
	SuggestedChatOffsetStartUltraLow = 0
)

// SuggestedChatOffset is the chat offset, in whole seconds, the dashboard
// player applies to a recording whose operator has set none
// (database.Job.HasOperatorChatOffset), owner ruling RV-19. known is false
// for a Twitch job (Twitch has no latency mode, and its chat is already on
// the media clock), and for a YouTube job whose latency mode is not known
// (never asked, or YouTube would not say). basis says which table the
// figure came from. A positive offset shows chat sooner. Derived, never
// stored.
//
// utils.js suggestedChatOffset is the dashboard's copy; cmd/moombox's
// job-state parity test runs both over web/tests/fixtures/job-state-parity.json.
func SuggestedChatOffset(job *database.Job) (seconds int, known bool, basis ChatOffsetBasis) {
	if job == nil || job.Platform == "twitch" || !job.LatencyMode.Known() {
		return 0, false, ""
	}
	if job.MediaStartTime != "" {
		switch job.LatencyMode {
		case database.LatencyModeNormal:
			return SuggestedChatOffsetIngestNormal, true, ChatOffsetBasisIngest
		case database.LatencyModeLow:
			return SuggestedChatOffsetIngestLow, true, ChatOffsetBasisIngest
		default:
			return SuggestedChatOffsetIngestUltraLow, true, ChatOffsetBasisIngest
		}
	}
	switch job.LatencyMode {
	case database.LatencyModeNormal:
		return SuggestedChatOffsetStartNormal, true, ChatOffsetBasisStart
	case database.LatencyModeLow:
		return SuggestedChatOffsetStartLow, true, ChatOffsetBasisStart
	default:
		return SuggestedChatOffsetStartUltraLow, true, ChatOffsetBasisStart
	}
}

// LatencyLine is the value of the "Latency" row both UIs' job details show
// for a YouTube job's latency mode and the chat offset it suggests: "Normal
// latency, suggested chat offset 15 s", or "Not stated by YouTube" for a
// broadcast YouTube would not name a mode for. "" for a job with none
// (Twitch, an upload, never asked). utils.js latencyText is the dashboard's
// copy (same parity table).
func LatencyLine(job *database.Job) string {
	if job == nil || job.Platform == "twitch" {
		return ""
	}
	if job.LatencyMode == database.LatencyModeUnstated {
		return "Not stated by YouTube"
	}
	seconds, known, _ := SuggestedChatOffset(job)
	if !known {
		return ""
	}
	return fmt.Sprintf("%s, suggested chat offset %d s", job.LatencyMode.Label(), seconds)
}
