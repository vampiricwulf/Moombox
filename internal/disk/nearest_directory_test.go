package disk

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestQueryNearestDirectoryAsksTheDeepestExistingOne pins the walk the Windows
// GetDiskSpace now takes. It queried the drive root alone, so a recordings
// disk mounted into a folder of C: reported C:'s space and its low-space alert
// never fired. The deepest directory that exists answers — the mount point
// here — and a missing path still answers for its nearest existing ancestor,
// ending at the root.
//
// Mutant: query only the root (start the walk at the volume root) — the
// mount point's volume is never asked.
func TestQueryNearestDirectoryAsksTheDeepestExistingOne(t *testing.T) {
	root := filepath.FromSlash("/")
	mount := filepath.Join(root, "Recordings")
	volumes := map[string]*DiskSpace{
		root:  {Total: 100, Free: 90},
		mount: {Total: 1000, Free: 5},
	}
	var asked []string
	query := func(dir string) (*DiskSpace, error) {
		asked = append(asked, dir)
		if s, ok := volumes[dir]; ok {
			return s, nil
		}
		return nil, errors.New("the system cannot find the path specified")
	}

	got, err := queryNearestDirectory(filepath.Join(mount, "Channel", "not-yet-created"), query)
	if err != nil {
		t.Fatal(err)
	}
	if got != volumes[mount] {
		t.Errorf("answered %+v, want the mounted volume's %+v (asked %v)", got, volumes[mount], asked)
	}

	asked = nil
	if got, err := queryNearestDirectory(filepath.Join(root, "elsewhere", "deep"), query); err != nil || got != volumes[root] {
		t.Errorf("a path off the mount answered %+v, %v; want the root volume's", got, err)
	}

	failing := func(string) (*DiskSpace, error) { return nil, errors.New("no volume") }
	if _, err := queryNearestDirectory(filepath.Join(root, "a", "b"), failing); err == nil {
		t.Error("every level failing must return an error")
	}
}
