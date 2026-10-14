package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each fixture is created entirely below the test's temporary directory; proving
// disposal never creates, modifies or checks out anything in the running workspace.
func linkedFixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	main := filepath.Join(base, "main")
	linked := filepath.Join(base, "linked")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = disposableGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %v: %s", err, out)
		}
	}
	run("init", "-q", main)
	run("-C", main, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "fixture")
	run("-C", main, "worktree", "add", "-q", "--detach", linked, "HEAD")
	return main, linked
}

func TestLinkedWorktreeRequiresActualExactLinkedRoot(t *testing.T) {
	main, linked := linkedFixture(t)
	sub := filepath.Join(linked, "nested")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	nonGit := t.TempDir()
	for _, tc := range []struct {
		name, root string
		want       bool
	}{
		{"linked", linked, true}, {"main", main, false}, {"non git", nonGit, false}, {"subdirectory", sub, false}, {"missing", filepath.Join(nonGit, "missing"), false}, {"relative", ".", false}, {"empty", "", false}, {"filesystem root", string(filepath.Separator), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkedWorktree(tc.root); got != tc.want {
				t.Fatalf("linkedWorktree(%q)=%v want %v", tc.root, got, tc.want)
			}
		})
	}
	alias := filepath.Join(filepath.Dir(linked), "alias")
	if err := os.Symlink(linked, alias); err != nil {
		t.Fatal(err)
	}
	if !linkedWorktree(alias) {
		t.Fatal("canonical alias of exact linked root refused")
	}
}

func TestLinkedWorktreeRejectsGitEnvironmentRedirection(t *testing.T) {
	main, linked := linkedFixture(t)
	nonGit := t.TempDir()
	gitFile, err := os.ReadFile(filepath.Join(linked, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitDir := string(gitFile[len("gitdir: "):])
	gitDir = strings.TrimSpace(gitDir)
	for key, value := range map[string]string{
		"GIT_DIR": gitDir, "GIT_WORK_TREE": nonGit, "GIT_COMMON_DIR": filepath.Join(main, ".git"),
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.worktree", "GIT_CONFIG_VALUE_0": nonGit,
		"GIT_CONFIG_PARAMETERS": "'core.worktree'='" + nonGit + "'", "GIT_CEILING_DIRECTORIES": filepath.Dir(linked),
	} {
		t.Setenv(key, value)
	}
	if linkedWorktree(nonGit) {
		t.Fatal("environment fabricated a disposable non-Git directory")
	}
	if linkedWorktree(main) {
		t.Fatal("environment fabricated a disposable main checkout")
	}
	if !linkedWorktree(linked) {
		t.Fatal("inherited Git redirection changed real linked-root proof")
	}
}

func TestLinkedWorktreeRejectsCopiedGitFileAndSeparateGitDir(t *testing.T) {
	_, linked := linkedFixture(t)
	spoof := t.TempDir()
	gitFile, err := os.ReadFile(filepath.Join(linked, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spoof, ".git"), gitFile, 0600); err != nil {
		t.Fatal(err)
	}
	if linkedWorktree(spoof) {
		t.Fatal("copied .git file impersonated registered linked root")
	}
	symlinkRoot := t.TempDir()
	if err := os.Symlink(filepath.Join(linked, ".git"), filepath.Join(symlinkRoot, ".git")); err != nil {
		t.Fatal(err)
	}
	if linkedWorktree(symlinkRoot) {
		t.Fatal("symlinked .git file impersonated registered linked root")
	}
	root := t.TempDir()
	gitDir := filepath.Join(t.TempDir(), "separate.git")
	cmd := exec.Command("git", "init", "-q", "--separate-git-dir", gitDir, root)
	cmd.Env = disposableGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("separate git fixture: %v %s", err, out)
	}
	if linkedWorktree(root) {
		t.Fatal("main checkout with separate git-dir accepted")
	}
}

func TestLinkedWorktreeIgnoresGlobalCoreWorktreeConfiguration(t *testing.T) {
	main, linked := linkedFixture(t)
	nonGit := t.TempDir()
	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config, []byte("[core]\n\tworktree = "+nonGit+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	if linkedWorktree(main) || linkedWorktree(nonGit) {
		t.Fatal("global configuration fabricated a disposable root")
	}
	if !linkedWorktree(linked) {
		t.Fatal("global configuration overrode actual linked root")
	}
}
