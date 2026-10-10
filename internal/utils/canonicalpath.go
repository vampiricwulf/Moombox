package utils

import "path/filepath"

// CanonicalPath returns path as an absolute, OS-normalised spelling with its
// symbolic links resolved (filepath.EvalSymlinks), so that two spellings of
// one location compare equal by prefix: a symlink and its target, an 8.3
// short name (RUNNER~1) and its long form, mixed case on a case-insensitive
// filesystem. A path that does not exist yet — a job's not-yet-written
// output, a ghost entry — is spelled from its deepest existing ancestor's
// canonical form plus the remainder, which is what the path guards need: a
// missing file under the output directory must still compare equal to the
// canonical output directory. When no ancestor exists at all the absolute
// spelling is returned.
//
// A Windows junction (a mount point) is NOT resolved. EvalSymlinks has not
// evaluated mount points since Go 1.23 (the winsymlink setting, on by
// default for this module's go version): it returns a junction as it is
// spelled, and refuses a path that continues below one (ENOTDIR — Lstat
// reports the junction as irregular, not as a directory). So a path through
// a junction is spelled through it: the walk up stops at the junction
// itself, which is normalised with everything above it, and the components
// below it are appended as given — their case and 8.3 names are not
// normalised, and a symlink among them is not resolved. A junction and its
// target are therefore two locations to anything that compares these
// spellings, and the path guards, which canonicalise both sides through
// this one function, accept a path only when it reaches the directory the
// way the directory is spelled: through the junction, or through the target.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cur, rest := abs, ""
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return real, nil
			}
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
