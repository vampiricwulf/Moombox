package monitor

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestTwitch_EndedBroadcastOfALatchedCaptureIsHandedOn is D-T4's monitor half.
// Each channel below carries one job the unconfirmed-end latch left in Error;
// a poll hands OnBroadcastOver exactly the jobs whose broadcast it shows over —
// the channel offline, or live with a DIFFERENT stream — and none whose
// broadcast is still the live one, whose channel's check failed, that the
// latch never marked, or that is no longer in Error.
//
// Mutants: the dispatchEndedBroadcasts call dropped from checkChunk (nothing
// is handed on); the channel's error ignored (the failed check's job is handed
// on); TwitchBroadcastOver replaced by "always over" (the still-live job is
// handed on); TwitchEndUnconfirmedJobs without its status filter (the Finished
// row on the offline "finished" channel is handed on).
func TestTwitch_EndedBroadcastOfALatchedCaptureIsHandedOn(t *testing.T) {
	chans := []config.ChannelConfig{
		{ID: "offline", Name: "offline", Platform: "twitch"},
		{ID: "samestream", Name: "samestream", Platform: "twitch"},
		{ID: "newstream", Name: "newstream", Platform: "twitch"},
		{ID: "failing", Name: "failing", Platform: "twitch"},
		{ID: "unmarked", Name: "unmarked", Platform: "twitch"},
		{ID: "finished", Name: "finished", Platform: "twitch"},
	}
	tm := newTestTwitchMonitor(t, func(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
		infos := make([]*twitch.TwitchStreamInfo, len(logins))
		errs := make([]error, len(logins))
		for i, login := range logins {
			switch login {
			case "samestream":
				infos[i] = &twitch.TwitchStreamInfo{StreamID: "200", ChannelLogin: login, IsLive: true}
			case "newstream":
				infos[i] = &twitch.TwitchStreamInfo{StreamID: "301", ChannelLogin: login, IsLive: true}
			case "failing":
				errs[i] = errors.New("gql slot failed")
			}
		}
		return infos, errs, nil
	}, chans...)

	for _, j := range []struct {
		id, login string
		marked    bool
	}{
		{"tw_100", "offline", true},
		{"tw_200", "samestream", true},
		{"tw_300", "newstream", true},
		{"tw_400", "failing", true},
		{"tw_500", "unmarked", false},
		{"tw_600", "finished", true},
	} {
		park := database.ParkReasonNone
		if j.marked {
			park = database.ParkReasonTwitchEndUnconfirmed
		}
		if _, err := tm.db.AddJob(&database.Job{ID: j.id, VideoID: j.id[3:], URL: "https://twitch.tv/" + j.login,
			Platform: "twitch", Status: database.StatusError}); err != nil {
			t.Fatal(err)
		}
		// Recorded since processed, as the real row is, so the poll's
		// live-channel handling sees a known broadcast and creates nothing.
		tm.db.AddToHistory(j.id)
		tm.db.UpdateJobFields(j.id, map[string]any{"park_reason": park, "error": "HLS playlist fetch failed"})
	}
	// A marked row that is no longer in Error (finished some other way) is
	// nobody's to mux.
	tm.db.UpdateJobFields("tw_600", map[string]any{"status": database.StatusFinished})

	var handed []string
	tm.OnBroadcastOver = func(jobID string) { handed = append(handed, jobID) }
	tm.doCheck(context.Background())

	slices.Sort(handed)
	if !slices.Equal(handed, []string{"tw_100", "tw_300"}) {
		t.Errorf("OnBroadcastOver got %v, want [tw_100 tw_300] — the offline channel's and the one live with another stream", handed)
	}
}

// TestAutoRetryRefusesTheLatchedRow: the automatic mux and the
// auto re-initialisation must never both take a row. A latched row is the
// mux's, so the recovery refuses it even if its error happened to read as the
// offline flap's — re-initialising deletes the staging the mux would archive.
//
// Mutant: the ParkReasonTwitchEndUnconfirmed check dropped from
// jobstate.TwitchAutoRetryPending.
func TestAutoRetryRefusesTheLatchedRow(t *testing.T) {
	job := &database.Job{Status: database.StatusError, Error: "twitch channel is offline",
		ParkReason: database.ParkReasonTwitchEndUnconfirmed}
	if jobstate.TwitchAutoRetryPending(job) {
		t.Error("a row the unconfirmed-end latch marked was offered to the auto re-initialisation")
	}
}

// TestTwitch_OfflineFlapIsOfferedRecoveryByItsParkReason is the offline
// flap's monitor half (owner ruling R-I1): a poll that finds the same
// broadcast live offers OnStreamRecover the Error row its park reason marks —
// under a reworded error, the text routing nothing — and offers neither the
// row whose budget is spent, the row that downloaded a segment, the
// ended-offline row (the operator's Mux), nor a row with no reason under
// that same reworded text. A row from before the reason existed, the frozen
// text alone, is still offered for one release.
//
// Mutants: TwitchAutoRetryPending keyed on the error text (the reworded
// twitch_offline row is not offered); the reason read without the budget or
// the segment term; the legacy fallback dropped (the legacy row is not
// offered) or widened to any text (the unmarked row is).
func TestTwitch_OfflineFlapIsOfferedRecoveryByItsParkReason(t *testing.T) {
	rows := []struct {
		login, stream string
		errText       string
		reason        database.ParkReason
		retries       int
		seq           bool
	}{
		{"flap", "100", "Twitch says the channel is not live", database.ParkReasonTwitchOffline, 1, false},
		{"spent", "200", "Twitch says the channel is not live", database.ParkReasonTwitchOffline, jobstate.MaxTwitchAutoRetries, false},
		{"started", "300", "Twitch says the channel is not live", database.ParkReasonTwitchOffline, 0, true},
		{"ended", "400", "Twitch says the channel is not live", database.ParkReasonTwitchEndedOffline, 0, false},
		{"unmarked", "500", "Twitch says the channel is not live", database.ParkReasonNone, 0, false},
		{"legacy", "600", database.LegacyTwitchOfflineError, database.ParkReasonNone, 0, false},
	}
	var chans []config.ChannelConfig
	streams := map[string]string{}
	for _, r := range rows {
		chans = append(chans, config.ChannelConfig{ID: r.login, Name: r.login, Platform: "twitch"})
		streams[r.login] = r.stream
	}
	tm := newTestTwitchMonitor(t, func(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
		infos := make([]*twitch.TwitchStreamInfo, len(logins))
		for i, login := range logins {
			infos[i] = &twitch.TwitchStreamInfo{StreamID: streams[login], ChannelLogin: login, IsLive: true}
		}
		return infos, make([]error, len(logins)), nil
	}, chans...)
	for _, r := range rows {
		id := "tw_" + r.stream
		if _, err := tm.db.AddJob(&database.Job{ID: id, VideoID: r.stream, URL: "https://twitch.tv/" + r.login,
			Platform: "twitch", Status: database.StatusError}); err != nil {
			t.Fatal(err)
		}
		tm.db.AddToHistory(id)
		fields := map[string]any{"park_reason": r.reason, "error": r.errText, "auto_retry_count": r.retries}
		if r.seq {
			fields["last_video_seq"] = 3
		}
		if tm.db.UpdateJobFields(id, fields) == nil {
			t.Fatalf("%s: row not written", id)
		}
	}

	var offered []string
	tm.OnStreamRecover = func(_ *twitch.TwitchStreamInfo, _ *config.ChannelConfig, jobID string) {
		offered = append(offered, jobID)
	}
	tm.OnStreamFound = func(info *twitch.TwitchStreamInfo, _ *config.ChannelConfig) {
		t.Errorf("a known broadcast (%s) was announced as new", info.StreamID)
	}
	tm.doCheck(context.Background())

	slices.Sort(offered)
	if !slices.Equal(offered, []string{"tw_100", "tw_600"}) {
		t.Errorf("OnStreamRecover got %v, want [tw_100 tw_600] — the marked flap and the legacy row", offered)
	}
}
