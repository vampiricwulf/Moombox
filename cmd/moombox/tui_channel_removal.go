package main

import (
	"log/slog"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/tui"
	"github.com/vampiricwulf/Moombox/internal/worker"
)

// wireTUIChannelRemoval gives the TUI's Settings → Channels removal prompt
// (W25-09) the two calls the dashboard makes through
// GET /api/config/channels/{id}/removal and DELETE ?jobs=delete: the same
// worker functions, adapted to the TUI's types.
func (s *runState) wireTUIChannelRemoval(app *tui.App) {
	stagingDir := func() string {
		var dir string
		s.configStore.Read(func(c *config.MoomboxConfig) { dir = c.Paths.EffectiveStagingDir() })
		return dir
	}
	app.OnChannelRemovalSummary = func(channelID, platform string) (tui.ChannelRemovalInfo, error) {
		sum, err := worker.SummarizeChannelRemoval(s.db, stagingDir(), channelID, platform)
		if err != nil {
			return tui.ChannelRemovalInfo{}, err
		}
		info := tui.ChannelRemovalInfo{Total: sum.Total, Pending: sum.Pending, Active: sum.Active}
		for _, f := range sum.Footage {
			info.Footage = append(info.Footage, f.Title)
		}
		return info, nil
	}
	app.OnDeletePendingChannelJobs = func(channelID string) (int, int, error) {
		n, kept, err := worker.DeletePendingChannelJobs(s.db, stagingDir(), channelID)
		if err != nil {
			s.log.Error("channel removed, but deleting its pending jobs failed; they are kept",
				slog.String("channel", channelID), slog.String("error", err.Error()))
			return 0, 0, err
		}
		s.log.Info("channel removed with its pending jobs",
			slog.String("channel", channelID), slog.Int("jobsDeleted", n), slog.Int("footageKept", len(kept)))
		return n, len(kept), nil
	}
}
