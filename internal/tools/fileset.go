package tools

import (
	"context"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"forge/internal/logx"
)

const (
	// fileSetTimeout bounds the whole listing. Like ReadGitInfo it must never
	// keep a tool call or an HTTP request alive on a hung credential helper or a
	// network filesystem.
	fileSetTimeout = 20 * time.Second
	// fileSetMaxEntries caps memory on pathological repositories. Beyond it the
	// lexically first entries win, so the truncation is deterministic.
	fileSetMaxEntries = 200000
)

// FileSet is the authoritative list of files that belong to a project: every
// path git tracks plus every untracked path that no ignore rule covers. It
// replaces the per-walk "starts with a dot" heuristic that used to hide
// .github/, .cursorrules and every other dotfile, and it makes the four
// discovery surfaces (the @ picker, the repository outline, grep_search and
// list_directory) agree on a single definition of "part of the project".
//
// A zero FileSet means git could not answer (no binary, not a repository,
// timeout). Callers must then fall back to walking the filesystem, which shows
// dotfiles too and only drops the well-known noise directories in skipDirs.
type FileSet struct {
	root   string
	files  map[string]struct{}
	dirs   map[string]struct{}
	order  []string
	loaded bool
}

// Root is the absolute project root the set was loaded for.
func (s FileSet) Root() string { return s.root }

// Loaded reports whether git answered. When false the caller must walk the
// filesystem instead of iterating the set.
func (s FileSet) Loaded() bool { return s.loaded }

// Len is the number of project files in the set.
func (s FileSet) Len() int { return len(s.order) }

// Files returns the project-relative slash paths in lexical order. The slice is
// owned by the set; callers must not modify it.
func (s FileSet) Files() []string { return s.order }

// Has reports whether rel is a project file. Symlinks and files that vanished
// from disk are still in the set; the caller stats before using the path.
func (s FileSet) Has(rel string) bool {
	if !s.loaded {
		return false
	}
	_, ok := s.files[cleanProjectRel(rel)]
	return ok
}

// HasDir reports whether rel is a directory that holds project files. The root
// itself always counts so a listing of "." is never filtered away.
func (s FileSet) HasDir(rel string) bool {
	if !s.loaded {
		return false
	}
	clean := cleanProjectRel(rel)
	if clean == "" {
		return true
	}
	if _, ok := s.dirs[clean]; ok {
		return true
	}
	// A gitlink (submodule) is recorded as a file entry but is a directory on
	// disk; treating it as a directory keeps it visible in list_directory.
	_, ok := s.files[clean]
	return ok
}

// LoadFileSet asks git which files belong to the project. It never returns an
// error: "not a repository" and "no git installed" are normal states of a Forge
// project, not failures, and they are reported as a non-loaded set.
func LoadFileSet(ctx context.Context, root string) FileSet {
	absRoot, err := outlineRoot(root)
	if err != nil {
		return FileSet{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, fileSetTimeout)
	defer cancel()

	// A bare repository, a plain directory and a missing binary all fail here.
	if inside, ok := gitProbe(ctx, absRoot, "rev-parse", "--is-inside-work-tree"); !ok || inside != "true" {
		return FileSet{}
	}

	// --cached --others --exclude-standard is the exact definition the user
	// asked for: tracked files plus untracked files that no ignore rule covers,
	// honoring nested .gitignore files, negations, .git/info/exclude and
	// core.excludesFile. A tracked file stays listed even when an ignore rule
	// matches it, which is what git itself does.
	cmd := exec.CommandContext(ctx, "git", "-C", absRoot, "--no-optional-locks",
		"-c", "core.quotepath=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	configureProcess(cmd)
	out, err := cmd.Output()
	if err != nil {
		logx.Debug("file set unavailable", "root", absRoot, "error", err)
		return FileSet{}
	}

	seen := make(map[string]struct{}, 256)
	order := make([]string, 0, 256)
	for _, raw := range strings.Split(string(out), "\x00") {
		rel := cleanProjectRel(raw)
		if rel == "" || strings.HasPrefix(rel, "../") || skipDirsPath(rel) {
			continue
		}
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		order = append(order, rel)
	}
	sort.Strings(order)

	truncated := false
	if len(order) > fileSetMaxEntries {
		order = order[:fileSetMaxEntries]
		truncated = true
	}

	set := FileSet{
		root:   absRoot,
		files:  make(map[string]struct{}, len(order)),
		dirs:   make(map[string]struct{}, len(order)/2),
		order:  order,
		loaded: true,
	}
	for _, rel := range order {
		set.files[rel] = struct{}{}
		for dir := path.Dir(rel); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
			if _, ok := set.dirs[dir]; ok {
				break // its ancestors were added together with it
			}
			set.dirs[dir] = struct{}{}
		}
	}
	if truncated {
		logx.Debug("file set truncated", "root", absRoot, "entries", len(order), "limit", fileSetMaxEntries)
	}
	return set
}

// projectFileSet is LoadFileSet for callers that have no request context.
func projectFileSet(root string) FileSet {
	return LoadFileSet(context.Background(), root)
}

// skipDirsPath reports whether any path component is a known noise directory.
// A committed vendor/ or node_modules/ tree is part of the repository but would
// drown every surface, so the long-standing skipDirs list still applies on top
// of git's answer.
func skipDirsPath(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if skipDirs[part] {
			return true
		}
	}
	return false
}

// fileSetPath joins a project-relative slash path to an absolute root.
func fileSetPath(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}
