package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// cleanupScope is an explicit allowlist rooted in one recorded worker worktree.
// It never infers ownership from the absence of a different live worker.
type cleanupScope struct{ root string }

func cleanupProtectedPaths() (string, []string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, err
	}
	home, err = processRoot(home)
	if err != nil || home == "" {
		return "", nil, fmt.Errorf("cannot establish home directory boundary")
	}
	paths := []string{"/System", "/Library", "/usr", "/Applications", filepath.Join(home, "Library")}
	for i, p := range paths {
		paths[i], err = processRoot(p)
		if err != nil {
			return "", nil, err
		}
	}
	return home, paths, nil
}

func newCleanupScope(path string) (cleanupScope, error) {
	root, err := processRoot(path)
	if err != nil {
		return cleanupScope{}, err
	}
	if root == "" {
		return cleanupScope{}, fmt.Errorf("cleanup requires a recorded worktree root")
	}
	home, protected, err := cleanupProtectedPaths()
	if err != nil {
		return cleanupScope{}, err
	}
	// Filesystem container directories are not specific worker scopes.
	for _, container := range []string{"/Users", "/home", "/private", "/private/var", "/var", "/tmp", "/var/tmp", "/Volumes", "/opt", "/mnt", "/media"} {
		canonical, e := processRoot(container)
		if e != nil {
			return cleanupScope{}, e
		}
		if root == canonical {
			return cleanupScope{}, fmt.Errorf("container directory is not a cleanup worktree: %s", root)
		}
	}
	// A parent of home or a protected directory is equally unsafe as a root.
	if insideWorktree(root, home) {
		return cleanupScope{}, fmt.Errorf("home or its parent is not a cleanup worktree: %s", root)
	}
	for _, p := range protected {
		if insideWorktree(p, root) || insideWorktree(root, p) {
			return cleanupScope{}, fmt.Errorf("protected path is not a cleanup worktree: %s", root)
		}
	}
	return cleanupScope{root: root}, nil
}

func (s cleanupScope) contains(p ProcessRecord) bool {
	if !p.UIDKnown || p.UID != os.Geteuid() || p.Started == "" || protectedExecutable(p.Command) {
		return false
	}
	// Resolve again at every use, including directly before TERM and KILL.
	// A replaced root symlink must not expand the previously approved scope.
	current, err := newCleanupScope(s.root)
	if err != nil || current.root != s.root {
		return false
	}
	cwd, err := processRoot(p.Cwd)
	if err != nil || cwd == "" || !insideWorktree(s.root, cwd) {
		return false
	}
	home, protected, err := cleanupProtectedPaths()
	if err != nil || cwd == home {
		return false
	}
	for _, path := range protected {
		if insideWorktree(path, cwd) {
			return false
		}
	}
	return true
}

// A literal existing directory ending with this text is not a deleted cwd.
func processCwd(path string) string {
	const suffix = " (deleted)"
	if len(path) > len(suffix) && path[len(path)-len(suffix):] == suffix {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path[:len(path)-len(suffix)]
		}
	}
	return path
}

// OS application/service processes remain protected even if they inherited a
// worker cwd. Ordinary /usr/bin development tools are scoped by cwd and owner.
func protectedExecutable(command string) bool {
	if !filepath.IsAbs(command) {
		return false
	}
	executable, err := processRoot(command)
	if err != nil {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	for _, base := range []string{"/System", "/Library", "/Applications", filepath.Join(home, "Library"), "/usr/libexec", "/usr/sbin"} {
		root, e := processRoot(base)
		if e != nil || insideWorktree(root, executable) {
			return true
		}
	}
	return false
}
