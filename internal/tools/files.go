package tools

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// defaultProjectFileLimit caps how many files the UI file picker receives for a
// single project. The walk skips vendor/build directories, so a typical project
// stays far below this.
const defaultProjectFileLimit = 5000

// ProjectFile is one attachable file of a project, as listed by the composer's
// "@" picker.
type ProjectFile struct {
	Path      string `json:"path"`
	Language  string `json:"language,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
}

// ProjectFiles lists the project files that can be attached to a message. It
// walks the project root, skipping hidden entries, symlinks and the usual
// build/vendor directories, and stops after limit entries (0 uses the default).
func ProjectFiles(root string, limit int) ([]ProjectFile, error) {
	absRoot, err := outlineRoot(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", absRoot)
	}
	if limit <= 0 {
		limit = defaultProjectFileLimit
	}

	out := make([]ProjectFile, 0, 256)
	walkErr := filepath.WalkDir(absRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p == absRoot {
				return nil
			}
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		fileInfo, infoErr := d.Info()
		if infoErr != nil || fileInfo.Size() > maxFileSize {
			return nil
		}
		rel, relErr := filepath.Rel(absRoot, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		lang, _ := outlineLanguage(rel)
		out = append(out, ProjectFile{Path: rel, Language: lang, SizeBytes: fileInfo.Size()})
		if len(out) >= limit {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// ReadAttachment loads one project file for inline attachment to a user message.
// The path is resolved inside root (symlinks cannot escape it) and the content is
// capped at maxChars runes. Binary and oversized files are rejected so the
// attachment block never carries garbage.
func ReadAttachment(root, rel string, maxChars int) (string, error) {
	absRoot, err := outlineRoot(root)
	if err != nil {
		return "", err
	}
	m := &Manager{Root: absRoot}
	abs, shown, err := m.resolve(rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", shown, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", shown)
	}
	if info.Size() > maxFileSize {
		return "", fmt.Errorf("%s is too large (%s)", shown, humanSize(info.Size()))
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", shown, err)
	}
	if isBinary(data) {
		return "", fmt.Errorf("%s appears to be binary", shown)
	}
	text := string(data)
	if maxChars > 0 && utf8.RuneCountInString(text) > maxChars {
		text = truncateRunes(text, maxChars) + "\n… [attachment truncated]"
	}
	return text, nil
}
