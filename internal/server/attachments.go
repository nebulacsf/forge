package server

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"forge/internal/logx"
	"forge/internal/tools"
)

// Attachments are the files the user pinned with the composer's "@" picker. They
// are inlined into the persisted user message so later turns keep the content
// even after the conversation is pruned or compacted.
const (
	attachmentMaxFiles      = 12
	attachmentMaxFileChars  = 20000
	attachmentMaxTotalChars = 80000
)

const (
	attachmentOpenTag  = "<attachments>"
	attachmentCloseTag = "</attachments>"
)

// buildAttachmentBlock renders the requested project files as a single block to
// append to the user message. Paths that cannot be read are reported inline so
// the agent can react instead of silently losing the context.
func buildAttachmentBlock(root string, paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(paths))
	var body strings.Builder
	total := 0
	used := 0

	for _, raw := range paths {
		path := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if used >= attachmentMaxFiles {
			break
		}
		used++

		remaining := attachmentMaxTotalChars - total
		if remaining <= 0 {
			fmt.Fprintf(&body, "<file path=%q skipped=\"attachment budget reached\" />\n", path)
			continue
		}
		limit := attachmentMaxFileChars
		if limit > remaining {
			limit = remaining
		}
		content, err := tools.ReadAttachment(root, path, limit)
		if err != nil {
			fmt.Fprintf(&body, "<file path=%q error=%q />\n", path, err.Error())
			continue
		}
		total += utf8.RuneCountInString(content)
		fmt.Fprintf(&body, "<file path=%q chars=\"%d\">\n%s\n</file>\n",
			path, utf8.RuneCountInString(content), content)
		logx.Debug("attachment injected", "path", path, "chars", utf8.RuneCountInString(content))
	}
	if used == 0 {
		return ""
	}
	return attachmentOpenTag + "\n" + body.String() + attachmentCloseTag
}

// attachmentPathList renders the pinned paths of an attachment block as a plain
// one-per-line list, so summaries keep the file references without the content.
func attachmentPathList(content string) string {
	start := strings.Index(content, attachmentOpenTag)
	if start < 0 {
		return ""
	}
	end := strings.Index(content[start:], attachmentCloseTag)
	block := content[start:]
	if end >= 0 {
		block = content[start : start+end]
	}
	paths := make([]string, 0, 4)
	for _, chunk := range strings.Split(block, "<file path=") {
		chunk = strings.TrimSpace(chunk)
		if !strings.HasPrefix(chunk, "\"") {
			continue
		}
		rest := chunk[1:]
		quote := strings.Index(rest, "\"")
		if quote <= 0 {
			continue
		}
		paths = append(paths, "[attachment] "+rest[:quote])
	}
	return strings.Join(paths, "\n")
}

// stripAttachmentBlocks removes an attachment block from a persisted user
// message. It mirrors the client-side helper so titles and prompts stay clean.
func stripAttachmentBlocks(content string) string {
	start := strings.Index(content, attachmentOpenTag)
	if start < 0 {
		return strings.TrimSpace(content)
	}
	end := strings.Index(content[start:], attachmentCloseTag)
	if end < 0 {
		return strings.TrimSpace(content[:start])
	}
	head := strings.TrimSpace(content[:start])
	tail := strings.TrimSpace(content[start+end+len(attachmentCloseTag):])
	return strings.TrimSpace(head + "\n" + tail)
}
