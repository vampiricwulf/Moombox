package disk

// Worse picks, of the output directory's reading and the staging directory's,
// the one with the higher usage — the reading the low-disk alerts and the
// backlog admission gate both judge when the two directories are on
// different volumes. staging is nil when there is no second volume to read
// (OtherVolume), and a tie goes to the output directory, so one volume under
// both directories is always named as the output directory.
func Worse(output, staging *DiskSpace) (worse *DiskSpace, isStaging bool) {
	if staging != nil && (output == nil || staging.UsedPct > output.UsedPct) {
		return staging, true
	}
	return output, false
}

// OtherVolume reads the staging directory's volume when it is not the output
// directory's. It returns (nil, nil) when stagingDir is empty or both
// directories are on one volume: there is then nothing to read that the
// output reading does not already say. An error is the staging volume's read
// failing.
//
// same and query are SameVolume and GetDiskSpace when nil; tests stand in
// their own.
func OtherVolume(outputDir, stagingDir string, same func(a, b string) bool, query func(string) (*DiskSpace, error)) (*DiskSpace, error) {
	if stagingDir == "" {
		return nil, nil
	}
	if same == nil {
		same = SameVolume
	}
	if same(outputDir, stagingDir) {
		return nil, nil
	}
	if query == nil {
		query = GetDiskSpace
	}
	return query(stagingDir)
}
