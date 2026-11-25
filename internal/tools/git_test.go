package tools

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gitRun executes git in dir with a fixed identity so the test never depends on
// the developer's global configuration.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Forge Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Forge Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"NO_COLOR=1", "TERM=dumb", "GIT_PAGER=cat",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo creates a repository with one commit. It skips the test when git is
// unavailable or too old for "init -b".
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-q", "-b", "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git init -b unsupported: %v\n%s", err, out)
	}
	file := dir + "/README.md"
	if err := os.WriteFile(file, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "README.md")
	gitRun(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func TestReadGitInfoBranch(t *testing.T) {
	dir := initRepo(t)
	info := ReadGitInfo(context.Background(), dir)
	if !info.IsRepo {
		t.Fatal("is_repo = false for a repository")
	}
	if info.Branch != "main" || info.Detached {
		t.Fatalf("info = %+v, want branch main", info)
	}
	if len(info.Commit) < 7 {
		t.Fatalf("commit = %q, want a short sha", info.Commit)
	}
}

func TestReadGitInfoDetachedHead(t *testing.T) {
	dir := initRepo(t)
	gitRun(t, dir, "checkout", "-q", "--detach")
	info := ReadGitInfo(context.Background(), dir)
	if !info.IsRepo || !info.Detached {
		t.Fatalf("info = %+v, want a detached repository", info)
	}
	if info.Branch != "" || info.Commit == "" {
		t.Fatalf("info = %+v, want an empty branch and a commit", info)
	}
}

func TestReadGitInfoUnbornBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Skipf("git init -b unsupported: %v\n%s", err, out)
	}
	// No commit yet: rev-parse --abbrev-ref HEAD fails here, the bar still has
	// to name the branch the user will commit to.
	info := ReadGitInfo(context.Background(), dir)
	if !info.IsRepo || info.Branch != "main" || info.Detached {
		t.Fatalf("info = %+v, want branch main on an unborn HEAD", info)
	}
}

func TestReadGitInfoNonRepo(t *testing.T) {
	info := ReadGitInfo(context.Background(), t.TempDir())
	if info.IsRepo || info.Branch != "" || info.Detached {
		t.Fatalf("info = %+v, want the zero value", info)
	}
}

func TestReadGitInfoEmptyRoot(t *testing.T) {
	if info := ReadGitInfo(context.Background(), "  "); info != (GitInfo{}) {
		t.Fatalf("info = %+v, want the zero value", info)
	}
}
