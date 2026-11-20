package main

// folder_permissions.go — the startup notice for a folder an earlier version
// restricted to one account.
//
// Up to v2.9.9 the Windows lockdown of the cookie file's and the config file's
// folder (`icacls <dir> /inheritance:r /grant:r <user>:(OI)(CI)F`) ran on ANY
// folder, and with the default ./cookies.txt that is the install folder: it
// removed SYSTEM and Administrators from the folder and from the output tree,
// the staging tree, the database and the log under it, and logged nothing. The
// lockdown now runs on a dedicated folder only (utils.DirTighteningAllowed);
// this file is for the installs it already ran on. It READS, and says what it
// found with the command that undoes it. It never changes a folder: which
// accounts should reach the operator's recordings is theirs to decide.

import (
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vampiricwulf/Moombox/internal/utils"
)

// uninheritedSharedFolders returns the parent folders of files that BOTH hold
// shared data (holdsSharedData) and no longer inherit permissions from their
// parent (daclProtected), as absolute paths, each once, in the order given.
//
// Both conditions, because each alone is ordinary: a dedicated secrets folder
// is restricted on purpose and is protected by design, and a shared folder
// that still inherits was never touched. An empty file path is skipped (no
// such file is configured), a drive root is skipped (it has no parent), and a
// folder whose ACL cannot be read is not named: the notice states a fact, and
// there is none to state.
func uninheritedSharedFolders(files []string, holdsSharedData func(dir string) bool, daclProtected func(dir string) (bool, error)) []string {
	var out []string
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if file == "" {
			continue
		}
		dir := filepath.Dir(file)
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if filepath.Dir(dir) == dir {
			// A drive root has no parent to inherit from: nothing to report,
			// and no restore command to give.
			continue
		}
		key := dir
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if !holdsSharedData(dir) {
			continue
		}
		if protected, err := daclProtected(dir); err != nil || !protected {
			continue
		}
		out = append(out, dir)
	}
	return out
}

// sharedFoldersWithoutInheritance is uninheritedSharedFolders over the real
// predicates. utils.DACLProtected answers false everywhere but Windows, so
// this names nothing there.
func sharedFoldersWithoutInheritance(files ...string) []string {
	return uninheritedSharedFolders(files, utils.DirHoldsSharedData, utils.DACLProtected)
}

// logUninheritedFolders writes the notice, one Info line per folder.
func logUninheritedFolders(log interface {
	Info(msg string, args ...any)
}, dirs []string) {
	for _, dir := range dirs {
		log.Info("this folder does not inherit permissions from its parent; an earlier Moombox version may have restricted it to one account",
			"dir", dir,
			"restore", `icacls "`+dir+`" /inheritance:e`)
	}
}
