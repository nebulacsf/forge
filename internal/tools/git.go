package tools

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// gitInfoTimeout bounds the whole probe. The workspace bar asks for this on
// project switches and after every turn, so a hung network filesystem or a
// huge repository must never keep the HTTP request alive.
const gitInfoTimeout = 5 * time.Second

// GitInfo is the git state of a project root as the UI header shows it.
type GitInfo struct {
	// IsRepo is false for a plain directory and for a machine without git, so
	// the caller renders the workspace without a branch chip instead of an
	// error the user cannot act on.
	IsRepo   bool   `json:"is_repo"`
	Branch   string `json:"branch"`
	Detached bool   `json:"detached"`
	Commit   string `json:"commit"`
}

// ReadGitInfo reports the branch (or the short SHA when HEAD is detached) of
// the repository that contains root. It never returns an error: "not a
// repository" is a normal state of a Forge project, not a failure.
func ReadGitInfo(ctx context.Context, root string) GitInfo {
	if strings.TrimSpace(root) == "" {
		return GitInfo{}
	}
	ctx, cancel := context.WithTimeout(ctx, gitInfoTimeout)
	defer cancel()

	// A bare repository, a plain directory and a missing binary all fail here,
	// which is exactly the "no branch to show" case.
	if inside, ok := gitProbe(ctx, root, "rev-parse", "--is-inside-work-tree"); !ok || inside != "true" {
		return GitInfo{}
	}
	info := GitInfo{IsRepo: true}

	// symbolic-ref names the branch even on an unborn HEAD (fresh repo, no
	// commits yet), where rev-parse --abbrev-ref cannot resolve "HEAD".
	if branch, ok := gitProbe(ctx, root, "symbolic-ref", "--short", "HEAD"); ok {
		info.Branch = branch
		if sha, ok := gitProbe(ctx, root, "rev-parse", "--short", "HEAD"); ok {
			info.Commit = sha
		}
		return info
	}

	// Detached HEAD: there is no branch to name, the short SHA is the only
	// label that still tells the user where they are.
	info.Detached = true
	if sha, ok := gitProbe(ctx, root, "rev-parse", "--short", "HEAD"); ok {
		info.Commit = sha
	}
	return info
}

// gitProbe runs one read-only git command inside root and returns its first
// output line. fail means git could not answer (no binary, not a repository,
// timeout), never that the answer was empty.
func gitProbe(ctx context.Context, root string, args ...string) (string, bool) {
	// --no-optional-locks keeps the probe from taking the index lock, so it can
	// never collide with the agent's own git_diff or a bash command.
	full := append([]string{"-C", root, "--no-optional-locks", "-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	configureProcess(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return "", false
	}
	return line, true
}
