package utils

import "path/filepath"

// CanonicalPath returns path as an absolute, symlink-free, OS-normalised
// spelling, so that two spellings of one location compare equal by prefix:
// a junction or symlink and its target, an 8.3 short name (RUNNER~1) and its
// long form, mixed case on a case-insensitive filesystem. A path that does
// not exist yet — a job's not-yet-written output, a ghost entry — is spelled
// from its deepest existing ancestor's canonical form plus the remainder,
// which is what the path guards need: a missing file under the output
// directory must still compare equal to the canonical output directory.
// When no ancestor exists at all the absolute spelling is returned.
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
