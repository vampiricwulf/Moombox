package worker

import (
	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/disk"
)

// diskReading is one free-space reading of the volume the jobs write to,
// judged against the disk_critical threshold — what the backlog scheduler's
// admission gate decides on (Scheduler.diskGateClosed). critical closes the
// gate and clear reopens it; a reading that is neither — below the threshold
// but inside its recovery margin — leaves the gate as it was.
type diskReading struct {
	dir      string
	free     uint64
	usedPct  float64
	critical bool // at or past the threshold (config.DiskConfig.AtCritical)
	clear    bool // DiskRecoveryMargin below it (config.DiskConfig.ClearOfCritical)
	// resumeAtPct is the usage clear begins at, for the gate's log line.
	resumeAtPct float64
}

// judgeDisk is a reading of ds against thresholds.
func judgeDisk(dir string, ds *disk.DiskSpace, thresholds config.DiskConfig) diskReading {
	return diskReading{
		dir:         dir,
		free:        ds.Free,
		usedPct:     ds.UsedPct,
		critical:    thresholds.AtCritical(ds.UsedPct),
		clear:       thresholds.ClearOfCritical(ds.UsedPct),
		resumeAtPct: float64(thresholds.CriticalPercent) - config.DiskRecoveryMargin,
	}
}

// readOutputDisk takes the reading the disk alerts take — the configured
// output directory through disk.GetDiskSpace, judged by
// config.DiskConfig.AtCritical, the rule routes.ComputeWarnLevel applies, and
// ClearOfCritical, the rule an open disk_critical alert steps down on — so
// backlog admission stops on exactly the reading that sends disk_critical and
// resumes on the one that ends it, with no setting of its own. The scheduler
// calls it once per admission sweep that has a backlog to admit.
//
// Read fresh rather than from the dashboard's last reading: that one is up to
// six minutes old, and in six minutes of admissions a volume that has just
// filled sends a channel's backlog into downloads that cannot write.
func (w *DownloadWorker) readOutputDisk() (diskReading, error) {
	var dir string
	var thresholds config.DiskConfig
	w.readConfig(func(c *config.MoomboxConfig) {
		dir = c.Paths.OutputDirectory
		thresholds = c.Disk
	})
	query := w.diskSpace
	if query == nil {
		query = disk.GetDiskSpace
	}
	ds, err := query(dir)
	if err != nil {
		return diskReading{dir: dir}, err
	}
	return judgeDisk(dir, ds, thresholds), nil
}
