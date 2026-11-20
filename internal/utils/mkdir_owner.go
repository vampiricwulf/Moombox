package utils

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// MkdirOwnerOnly makes sure dir exists, for a folder that will hold a secret
// Moombox writes (the cookie file, config.toml). A folder that is already
// there is left exactly as it is. A missing one is created for its owner only
// (mode 0700 on POSIX) — the folder itself and nothing else: a missing parent
// is created with the ordinary 0755, because the parent is not the secret's
// folder and may come to hold the output tree. On Windows the modes mean
// nothing and no ACL call is made; the new folders inherit their parent's
// permissions.
//
// Owner ruling, 2026-11-19: Moombox sets permissions on the files it writes
// and on a folder at the moment Moombox itself creates it, and never changes
// the permissions of a folder that already existed.
func MkdirOwnerOnly(dir string) error {
	// Looked at first rather than inferred from Mkdir's error: os.Mkdir on a
	// drive root or "." does not answer ErrExist on every platform.
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a folder", dir)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	// ErrExist here is another writer creating it between the Stat and now.
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}
