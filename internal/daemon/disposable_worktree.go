package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// disposableGitEnv prevents inherited repository/configuration overrides from
// making an unrelated project appear to be a disposable linked worktree.
func disposableGitEnv() []string {
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
}

// linkedWorktree proves the exact supplied directory is a registered linked Git
// worktree. Main checkouts, nested directories and uncertain/missing evidence fail
// closed. A copied .git file is insufficient: Git's administration backlink must
// identify this worktree too. This helper never creates or changes Git state.
func linkedWorktree(root string) bool {
	if !filepath.IsAbs(root) || strings.ContainsAny(root, "\r\n") {
		return false
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	root = filepath.Clean(root)
	if root == string(filepath.Separator) {
		return false
	}
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return false
	}
	dotGit := filepath.Join(root, ".git")
	st, err = os.Lstat(dotGit)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir", "--is-inside-work-tree")
	cmd.Env = disposableGitEnv()
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if len(lines) != 4 || lines[3] != "true" {
		return false
	}
	canonical := func(path string) (string, bool) {
		if !filepath.IsAbs(path) {
			return "", false
		}
		resolved, err := filepath.EvalSymlinks(path)
		return filepath.Clean(resolved), err == nil
	}
	top, ok := canonical(lines[0])
	if !ok || top != root {
		return false
	}
	gitDir, ok := canonical(lines[1])
	if !ok {
		return false
	}
	commonDir, ok := canonical(lines[2])
	if !ok || gitDir == commonDir {
		return false
	}
	worktrees, ok := canonical(filepath.Join(commonDir, "worktrees"))
	if !ok || filepath.Dir(gitDir) != worktrees {
		return false
	}
	backlink, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return false
	}
	gitFile := strings.TrimSuffix(string(backlink), "\n")
	if gitFile == "" || strings.ContainsAny(gitFile, "\r\n") {
		return false
	}
	if !filepath.IsAbs(gitFile) {
		gitFile = filepath.Join(gitDir, gitFile)
	}
	linkedGit, ok := canonical(gitFile)
	if !ok {
		return false
	}
	actualGit, ok := canonical(dotGit)
	return ok && linkedGit == actualGit
}
