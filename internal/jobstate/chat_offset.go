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
// Typical, not measured per recording. The 2026-11-27 research put a typical
// viewer's delay behind ingest at the head lag it measured (ingest to
// downloadable: Normal about 6-11 s, Low 1-3 s, Ultra low 2-4 s), plus up to
// one segment (the lag is measured to a segment's start), plus the player's
// readahead YouTube sends for each mode (that the readahead adds to the
// delay is an inference the run did not show): about 15-25 s for Normal,
// 5-9 s for Low and 3-6 s for Ultra low. The ingest figures, 20, 7 and 4,
// are chosen inside those ranges. They agree with YouTube's help, which says
// most viewers see under 10 seconds of delay on Low and under 5 on Ultra low
// (it gives no figure for Normal). The start figures are those less the 5 s
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

// HasOperatorChatOffset reports whether the operator set job's chat offset:
// ChatOffsetSet, or a non-zero ChatOffset saved before the flag existed (a
// row from before v30 holds the flag at 0). An operator's offset, a 0
// included, always wins over SuggestedChatOffset (owner ruling RV-19).
// Derived, never stored. utils.js hasOperatorChatOffset is the dashboard's
// copy (the job-state parity table's operatorChatOffset section).
func HasOperatorChatOffset(job *database.Job) bool {
	return job != nil && (job.ChatOffsetSet || job.ChatOffset != 0)
}

// SuggestedChatOffset is the chat offset, in whole seconds, the dashboard
// player applies to a recording whose operator has set none
// (HasOperatorChatOffset), owner ruling RV-19. known is false
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
// (Twitch, never asked), and for a VOD download stored as unstated: the
// backfill stores that marker on an ordinary upload (never a stream, owner
// ruling RV-19: it has no latency mode) and on a private or deleted video,
// only so it is not asked again, and a VOD that is unstated carries no
// suggestion either way. utils.js latencyText is the dashboard's copy (same
// parity table).
func LatencyLine(job *database.Job) string {
	if job == nil || job.Platform == "twitch" {
		return ""
	}
	if job.LatencyMode == database.LatencyModeUnstated {
		if job.IsVod {
			return ""
		}
		return "Not stated by YouTube"
	}
	seconds, known, _ := SuggestedChatOffset(job)
	if !known {
		return ""
	}
	return fmt.Sprintf("%s, suggested chat offset %d s", job.LatencyMode.Label(), seconds)
}
