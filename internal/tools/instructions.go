package tools

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Project instruction artifacts are the conventions a repository ships for
// coding agents (AGENTS.md and its well-known equivalents). Forge captures them
// once per session and injects the snapshot into the stable system prompt, so a
// project that follows the convention is understood without pinning files with
// the "@" picker.
const (
	// instructionMaxAgentChars bounds the agent instruction files together.
	// ~24k runes is roughly 6k tokens, small enough to leave room for history
	// and large enough for a well documented repository.
	instructionMaxAgentChars = 24000
	// instructionMaxReadmeChars bounds the README fallback. A README documents
	// the product rather than prescribing how to work, so it gets a smaller
	// budget and a different label in the prompt.
	instructionMaxReadmeChars = 8000
	// instructionMaxTotalChars is the hard ceiling for everything injected.
	instructionMaxTotalChars = 30000

	instructionKindAgent  = "agent"
	instructionKindReadme = "readme"

	instructionTruncatedMarker = "[attachment truncated]"
)

// instructionCandidates are checked in priority order in the project root.
// The first existing file of each distinct content wins; duplicates are dropped
// because symlinking CLAUDE.md to AGENTS.md is a common convention.
//
// They are stat'ed directly instead of reused through ProjectFiles: that walk
// skips dotfiles by design (build/vendor noise), which would silently break
// .cursorrules and .github/copilot-instructions.md.
var instructionCandidates = []string{
	"AGENTS.md",
	"CLAUDE.md",
	".cursorrules",
	".github/copilot-instructions.md",
}

// instructionReadmeFallback is injected only when no agent file exists.
const instructionReadmeFallback = "README.md"

// InstructionFile is one project artifact captured into the session snapshot.
type InstructionFile struct {
	// Path is the project-relative path, always slash separated.
	Path string `json:"path"`
	// Kind is "agent" for normative instructions and "readme" for background
	// documentation.
	Kind string `json:"kind"`
	// Content is the file text, already bounded by the per-file budget.
	Content string `json:"content"`
	// Chars is the rune count of Content.
	Chars int `json:"chars"`
	// Truncated reports that Content is a prefix of the real file.
	Truncated bool `json:"truncated"`
}

// ReadProjectInstructions discovers and reads the project instruction artifacts
// that Forge injects automatically. It never fails because a file is missing or
// unreadable: those candidates are skipped quietly. The returned slice is empty
// when the project ships no recognized artifact.
func ReadProjectInstructions(root string) ([]InstructionFile, error) {
	absRoot, err := outlineRoot(root)
	if err != nil {
		return nil, err
	}
	out := make([]InstructionFile, 0, len(instructionCandidates)+1)
	seen := make(map[string]bool, len(instructionCandidates)+1)
	total := 0

	agentFiles := readInstructionCandidates(absRoot, instructionCandidates,
		instructionKindAgent, instructionMaxAgentChars, &total, seen)
	out = append(out, agentFiles...)
	if len(agentFiles) > 0 {
		return out, nil
	}

	readme := readInstructionCandidates(absRoot, []string{instructionReadmeFallback},
		instructionKindReadme, instructionMaxReadmeChars, &total, seen)
	return append(out, readme...), nil
}

// readInstructionCandidates reads the first existing, non-duplicate candidate of
// each path within budget.
func readInstructionCandidates(absRoot string, candidates []string, kind string, budget int, total *int, seen map[string]bool) []InstructionFile {
	out := make([]InstructionFile, 0, len(candidates))
	for _, rel := range candidates {
		remaining := budget - *total
		if hard := instructionMaxTotalChars - *total; hard < remaining {
			remaining = hard
		}
		if remaining <= 0 {
			break
		}
		abs := filepath.Join(absRoot, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		content, err := ReadAttachment(absRoot, rel, remaining)
		if err != nil {
			continue
		}
		// ReadAttachment appends its own marker when it clips. Strip it so the
		// snapshot budget measures real content and the block renderer can state
		// the truncation once, in its own wording.
		truncated := false
		if trimmed := strings.TrimRight(content, " \n"); strings.HasSuffix(trimmed, instructionTruncatedMarker) {
			trimmed = strings.TrimSuffix(trimmed, instructionTruncatedMarker)
			trimmed = strings.TrimRight(trimmed, " \n")
			trimmed = strings.TrimSuffix(trimmed, "…")
			content = strings.TrimRight(trimmed, " \n")
			truncated = true
		}
		key := strings.Join(strings.Fields(content), " ")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		file := InstructionFile{
			Path:      rel,
			Kind:      kind,
			Content:   content,
			Chars:     utf8.RuneCountInString(content),
			Truncated: truncated,
		}
		*total += file.Chars
		out = append(out, file)
	}
	return out
}
