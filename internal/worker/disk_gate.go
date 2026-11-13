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

// readOutputDisk reads the configured output directory through
// disk.GetDiskSpace, judged by config.DiskConfig.AtCritical, the rule
// routes.ComputeWarnLevel applies, and ClearOfCritical, the rule an open
// disk_critical alert steps down on. It is the output half of the gate's
// reading (readGateDisk), and the whole of what the Twitch gap filler asks:
// a fill writes beside the archive.
func (w *DownloadWorker) readOutputDisk() (diskReading, error) {
	r, _, _, err := w.readOutputVolume()
	return r, err
}

// readOutputVolume is readOutputDisk with the staging directory and the
// thresholds the same config read saw, for readGateDisk.
func (w *DownloadWorker) readOutputVolume() (diskReading, string, config.DiskConfig, error) {
	var dir, staging string
	var thresholds config.DiskConfig
	w.readConfig(func(c *config.MoomboxConfig) {
		dir = c.Paths.OutputDirectory
		staging = c.Paths.EffectiveStagingDir()
		thresholds = c.Disk
	})
	query := w.diskSpace
	if query == nil {
		query = disk.GetDiskSpace
	}
	ds, err := query(dir)
	if err != nil {
		return diskReading{dir: dir}, staging, thresholds, err
	}
	return judgeDisk(dir, ds, thresholds), staging, thresholds, nil
}

// readGateDisk takes the reading the disk alerts judge — the output
// directory's volume or, when the staging directory is on another volume
// and fuller, that one (disk.OtherVolume, disk.Worse) — so backlog admission
// stops on exactly the reading that sends disk_critical and resumes on the
// one that ends it, with no setting of its own. Every download writes its
// segments to staging first: read alone, the output volume let a full
// staging disk admit one backlog VOD after another into downloads that
// could not write. The scheduler calls it once per admission sweep that has
// a backlog to admit, and once per sweep while the gate is closed.
//
// A staging volume that cannot be read is left out, and the output reading
// decides alone, with one exception: when the staging volume's last reading
// was not clear of the threshold (it closed the gate, or sat inside the
// margin), the output reading may still close the gate but does not reopen
// it. A staging drive that was full and then went offline is not known to
// have room, and reopening sent the backlog into a directory that was absent
// or still full. The first staging reading that answers decides again. A job or channel output directory on a third volume is not
// read here, as it is not by the alerts.
//
// Read fresh rather than from the dashboard's last reading: that one is up to
// six minutes old, and in six minutes of admissions a volume that has just
// filled sends a channel's backlog into downloads that cannot write.
func (w *DownloadWorker) readGateDisk() (diskReading, error) {
	out, stagingDir, thresholds, err := w.readOutputVolume()
	if err != nil {
		return out, err
	}
	ds, err := disk.OtherVolume(out.dir, stagingDir, w.sameVolume, w.diskSpace)
	if err != nil {
		if w.stagingNotClear.Load() {
			out.clear = false
		}
		return out, nil
	}
	if ds == nil { // staging is on the output volume
		w.stagingNotClear.Store(false)
		return out, nil
	}
	w.stagingNotClear.Store(!thresholds.ClearOfCritical(ds.UsedPct))
	if _, isStaging := disk.Worse(&disk.DiskSpace{UsedPct: out.usedPct}, ds); isStaging {
		return judgeDisk(stagingDir, ds, thresholds), nil
	}
	return out, nil
}
