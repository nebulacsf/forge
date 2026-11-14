// This file implements Forge's zero-AI repository map. The scanner uses the Go
// AST for .go files and lightweight line scanners for other languages, then
// persists a compact per-file outline in SQLite so every chat turn can start
// with structural awareness of the project.
package tools

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"forge/internal/logx"
	"forge/internal/store"
)

const (
	outlineMaxRunes     = 2000 // ~450-500 tokens including the section header
	outlineMaxFiles     = 8000
	outlineMaxFileSize  = 512 << 10
	outlineMaxSymbols   = 32
	outlineMaxFileRunes = 180
	outlineMaxLineRunes = 400
	outlineMaxNameRunes = 80
)

type outlineSymbol struct {
	Line int
	Kind string
	Name string
}

type outlinePattern struct {
	re      *regexp.Regexp
	kind    string
	topOnly bool
}

var (
	jsClassRE  = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	jsFuncRE   = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)
	jsArrowRE  = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`)
	jsTypeRE   = regexp.MustCompile(`^\s*(?:export\s+)?(?:interface|type|enum)\s+([A-Za-z_$][\w$]*)`)
	jsMethodRE = regexp.MustCompile(`^\s{2,8}(?:(?:public|private|protected|static|async|readonly|get|set)\s+)*([A-Za-z_$][\w$]*)\s*\([^;]*\)\s*(?::\s*[^{]+)?\{\s*$`)

	pyClassRE = regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`)
	pyDefRE   = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`)

	rsFnRE   = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?fn\s+([A-Za-z_]\w*)`)
	rsTypeRE = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|union|type)\s+([A-Za-z_]\w*)`)
	rsModRE  = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?mod\s+([A-Za-z_]\w*)`)
	rsImplRE = regexp.MustCompile(`^\s*impl\s+(?:<[^>]*>\s*)?([A-Za-z_]\w*)`)

	javaTypeRE   = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|final|abstract|sealed)\s+)*(?:class|interface|enum|record)\s+([A-Za-z_$][\w$]*)`)
	javaMethodRE = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|final|abstract|synchronized|native|default)\s+)+(?:[\w<>\[\],.?]+\s+)+([A-Za-z_$][\w$]*)\s*\([^;]*\)\s*(?:throws[^{]+)?\{`)

	rbClassRE  = regexp.MustCompile(`^\s*class\s+([A-Z]\w*(?:::\w+)*)`)
	rbModuleRE = regexp.MustCompile(`^\s*module\s+([A-Z]\w*(?:::\w+)*)`)
	rbDefRE    = regexp.MustCompile(`^\s*def\s+(?:self\.)?([A-Za-z_]\w*[?!]?)`)

	phpTypeRE = regexp.MustCompile(`^\s*(?:abstract\s+|final\s+)?(?:class|interface|trait|enum)\s+([A-Za-z_]\w*)`)
	phpFuncRE = regexp.MustCompile(`^\s*(?:(?:public|protected|private|static|final|abstract)\s+)*function\s+([A-Za-z_]\w*)`)

	htmlHeadingRE = regexp.MustCompile(`(?i)<h[1-3][^>]*>(.*?)</h[1-3]>`)
	htmlIDRE      = regexp.MustCompile(`(?i)<([a-z][\w-]*)[^>]*\bid=["']([^"']+)["']`)
	htmlTagRE     = regexp.MustCompile(`(?i)<[^>]+>`)
)

var jsOutlinePatterns = []outlinePattern{
	{jsClassRE, "class", true},
	{jsFuncRE, "func", true},
	{jsArrowRE, "func", true},
	{jsTypeRE, "type", true},
	{jsMethodRE, "method", false},
}

// Control-flow lines such as "if (x) {" look like methods; skip them.
var jsControlKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"function": true, "return": true, "else": true, "do": true, "try": true,
	"finally": true, "with": true, "await": true, "yield": true, "throw": true,
	"new": true, "delete": true, "typeof": true, "void": true, "in": true, "of": true,
}

var outlinePatterns = map[string][]outlinePattern{
	"js": jsOutlinePatterns,
	"ts": jsOutlinePatterns,
	"py": {
		{pyClassRE, "class", false},
		{pyDefRE, "func", false},
	},
	"rs": {
		{rsFnRE, "fn", true},
		{rsTypeRE, "type", true},
		{rsModRE, "mod", true},
		{rsImplRE, "impl", true},
	},
	"java": {
		{javaTypeRE, "type", true},
		{javaMethodRE, "method", false},
	},
	"rb": {
		{rbClassRE, "class", true},
		{rbModuleRE, "module", true},
		{rbDefRE, "def", false},
	},
	"php": {
		{phpTypeRE, "type", true},
		{phpFuncRE, "func", false},
	},
}

var outlineLanguages = map[string]string{
	".go":   "go",
	".js":   "js",
	".jsx":  "js",
	".mjs":  "js",
	".cjs":  "js",
	".ts":   "ts",
	".tsx":  "ts",
	".mts":  "ts",
	".cts":  "ts",
	".py":   "py",
	".rs":   "rs",
	".java": "java",
	".rb":   "rb",
	".php":  "php",
	".html": "html",
	".htm":  "html",
}

// EnsureRepoOutline scans a project, refreshes only the files whose mtime or
// size changed, persists the result, and returns the compact rendered outline.
func EnsureRepoOutline(ctx context.Context, st *store.Store, root string) (string, error) {
	absRoot, err := outlineRoot(root)
	if err != nil {
		return "", err
	}
	started := time.Now()

	var cached []store.RepoOutlineFile
	if st != nil {
		cached, err = st.ListRepoOutline(absRoot)
		if err != nil {
			return "", err
		}
	}
	cachedByPath := make(map[string]store.RepoOutlineFile, len(cached))
	for _, f := range cached {
		cachedByPath[f.FilePath] = f
	}

	seen := make(map[string]bool, len(cached))
	upserts := make([]store.RepoOutlineFile, 0)
	truncated := false

	walkErr := filepath.WalkDir(absRoot, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if p == absRoot {
				return nil
			}
			name := d.Name()
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil // never follow symlinked files out of the project
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			return nil
		}
		rel, relErr := filepath.Rel(absRoot, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		lang, ok := outlineLanguage(rel)
		if !ok {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || info.IsDir() || info.Size() > outlineMaxFileSize {
			return nil
		}
		if seen[rel] {
			return nil
		}
		if len(seen) >= outlineMaxFiles {
			truncated = true
			return fs.SkipAll
		}
		seen[rel] = true
		if c, ok := cachedByPath[rel]; ok &&
			c.MTimeNS == info.ModTime().UnixNano() &&
			c.SizeBytes == info.Size() {
			return nil
		}
		if row, ok := outlineRow(absRoot, rel, lang, info); ok {
			upserts = append(upserts, row)
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}

	deleted := make([]string, 0)
	for rel := range cachedByPath {
		if !seen[rel] {
			deleted = append(deleted, rel)
		}
	}
	sort.Strings(deleted)

	var rows []store.RepoOutlineFile
	if st != nil {
		if err := st.SyncRepoOutline(absRoot, upserts, deleted); err != nil {
			return "", err
		}
		rows, err = st.ListRepoOutline(absRoot)
		if err != nil {
			return "", err
		}
	} else {
		rows = upserts
	}

	outline := RenderRepoOutline(rows)
	logx.Debug("repo outline ensured", "root", absRoot, "files", len(rows),
		"parsed", len(upserts), "deleted", len(deleted), "truncated", truncated,
		"chars", utf8.RuneCountInString(outline),
		"duration", time.Since(started).Round(time.Millisecond).String())
	return outline, nil
}

// CurrentRepoOutline renders the cached outline without walking the filesystem.
// It is used between agent rounds so writes are reflected without a rescan.
func CurrentRepoOutline(st *store.Store, root string) (string, error) {
	if st == nil {
		return "", nil
	}
	absRoot, err := outlineRoot(root)
	if err != nil {
		return "", err
	}
	rows, err := st.ListRepoOutline(absRoot)
	if err != nil {
		return "", err
	}
	return RenderRepoOutline(rows), nil
}

// RefreshOutlineFile rescans a single file after Forge writes it.
func RefreshOutlineFile(st *store.Store, root, rel string) error {
	if st == nil {
		return nil
	}
	absRoot, err := outlineRoot(root)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	rel = strings.TrimPrefix(rel, "./")
	if rel == "" || rel == "." {
		return nil
	}
	abs := filepath.Join(absRoot, filepath.FromSlash(rel))
	if err := ensureInside(absRoot, abs); err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		if err := ensureInside(absRoot, real); err != nil {
			return err
		}
	}
	lang, ok := outlineLanguage(rel)
	if !ok {
		return st.SyncRepoOutline(absRoot, nil, []string{rel})
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() || info.Size() > outlineMaxFileSize {
		return st.SyncRepoOutline(absRoot, nil, []string{rel})
	}
	row, ok := outlineRow(absRoot, rel, lang, info)
	if !ok {
		return st.SyncRepoOutline(absRoot, nil, []string{rel})
	}
	return st.SyncRepoOutline(absRoot, []store.RepoOutlineFile{row}, nil)
}

// RenderRepoOutline turns cached rows into a compact, token-bounded outline.
func RenderRepoOutline(files []store.RepoOutlineFile) string {
	if len(files) == 0 {
		return ""
	}
	sort.Slice(files, func(i, j int) bool {
		di, dj := outlineDepth(files[i].FilePath), outlineDepth(files[j].FilePath)
		if di != dj {
			return di < dj
		}
		ti, tj := outlineIsTest(files[i].FilePath), outlineIsTest(files[j].FilePath)
		if ti != tj {
			return !ti // implementation files before tests
		}
		pi, pj := outlinePriority(files[i].FilePath), outlinePriority(files[j].FilePath)
		if pi != pj {
			return pi < pj
		}
		return files[i].FilePath < files[j].FilePath
	})

	var b strings.Builder
	used := 0
	budget := outlineMaxRunes - 100 // reserve room for the truncation notice
	truncated := false
	for _, f := range files {
		line := f.FilePath
		if syms := strings.TrimSpace(f.Symbols); syms != "" {
			if utf8.RuneCountInString(syms) > outlineMaxFileRunes {
				syms = truncateRunes(syms, outlineMaxFileRunes) + "…"
			}
			line += ": " + syms
		}
		if utf8.RuneCountInString(line) > outlineMaxLineRunes {
			line = truncateRunes(line, outlineMaxLineRunes) + "…"
		}
		cost := utf8.RuneCountInString(line) + 1
		if used+cost > budget {
			truncated = true
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		used += cost
	}
	if truncated {
		b.WriteString("[outline truncated; use grep_search or list_directory for the rest]\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func outlineRow(root, rel, lang string, info os.FileInfo) (store.RepoOutlineFile, bool) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	data, err := os.ReadFile(abs)
	if err != nil || isBinary(data) {
		return store.RepoOutlineFile{}, false
	}
	symbols := scanOutlineFile(lang, abs, data)
	return store.RepoOutlineFile{
		ProjectPath: root,
		FilePath:    rel,
		Language:    lang,
		MTimeNS:     info.ModTime().UnixNano(),
		SizeBytes:   info.Size(),
		Symbols:     formatOutlineSymbols(symbols),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}, true
}

func scanOutlineFile(lang, abs string, data []byte) []outlineSymbol {
	switch lang {
	case "go":
		return scanGoOutline(abs, data)
	case "html":
		return scanHTMLOutline(data)
	default:
		return scanRegexOutline(lang, data)
	}
}

func scanGoOutline(abs string, data []byte) []outlineSymbol {
	fset := token.NewFileSet()
	file, _ := parser.ParseFile(fset, abs, data, parser.SkipObjectResolution)
	if file == nil {
		return nil
	}
	lineOf := func(pos token.Pos) int {
		if pos == token.NoPos {
			return 0
		}
		return fset.Position(pos).Line
	}
	out := make([]outlineSymbol, 0, 16)
	if file.Name != nil {
		out = append(out, outlineSymbol{Line: lineOf(file.Package), Kind: "package", Name: file.Name.Name})
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name == nil {
					continue
				}
				kind := "type"
				switch ts.Type.(type) {
				case *ast.StructType:
					kind = "struct"
				case *ast.InterfaceType:
					kind = "interface"
				}
				out = append(out, outlineSymbol{Line: lineOf(ts.Pos()), Kind: kind, Name: ts.Name.Name})
			}
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			kind := "func"
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				if recv := receiverTypeName(d.Recv.List[0].Type); recv != "" {
					name = recv + "." + name
				}
			}
			out = append(out, outlineSymbol{Line: lineOf(d.Pos()), Kind: kind, Name: name})
		}
	}
	return limitAndSortSymbols(out)
}

func receiverTypeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return receiverTypeName(e.X)
	case *ast.ParenExpr:
		return receiverTypeName(e.X)
	case *ast.IndexExpr:
		return receiverTypeName(e.X)
	case *ast.IndexListExpr:
		return receiverTypeName(e.X)
	default:
		return ""
	}
}

func scanRegexOutline(lang string, data []byte) []outlineSymbol {
	patterns := outlinePatterns[lang]
	if len(patterns) == 0 {
		return nil
	}
	out := make([]outlineSymbol, 0, 16)
	for i, raw := range strings.Split(string(data), "\n") {
		if len(out) >= outlineMaxSymbols {
			break
		}
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" || len(line) > 4000 {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		for _, p := range patterns {
			if p.topOnly && indent > 0 {
				continue
			}
			m := p.re.FindStringSubmatch(line)
			if len(m) < 2 || strings.TrimSpace(m[1]) == "" {
				continue
			}
			kind := p.kind
			if lang == "py" && kind == "func" && indent > 0 {
				kind = "method"
			}
			if lang == "rb" && kind == "def" && indent > 0 {
				kind = "method"
			}
			if kind == "method" && jsControlKeywords[m[1]] {
				continue
			}
			out = append(out, outlineSymbol{Line: i + 1, Kind: kind, Name: clipOutlineName(m[1])})
			break
		}
	}
	return out
}

func scanHTMLOutline(data []byte) []outlineSymbol {
	out := make([]outlineSymbol, 0, 8)
	for i, raw := range strings.Split(string(data), "\n") {
		if len(out) >= outlineMaxSymbols {
			break
		}
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := htmlHeadingRE.FindStringSubmatch(line); len(m) >= 2 {
			name := strings.Join(strings.Fields(htmlTagRE.ReplaceAllString(m[1], " ")), " ")
			if name != "" {
				out = append(out, outlineSymbol{Line: i + 1, Kind: "heading", Name: clipOutlineName(name)})
				continue
			}
		}
		if m := htmlIDRE.FindStringSubmatch(line); len(m) >= 3 {
			out = append(out, outlineSymbol{Line: i + 1, Kind: "id",
				Name: clipOutlineName(strings.ToLower(m[1]) + "#" + m[2])})
		}
	}
	return out
}

func formatOutlineSymbols(symbols []outlineSymbol) string {
	if len(symbols) == 0 {
		return ""
	}
	parts := make([]string, 0, len(symbols))
	for _, s := range symbols {
		parts = append(parts, fmt.Sprintf("L%d %s %s", s.Line, s.Kind, s.Name))
	}
	return strings.Join(parts, "; ")
}

func limitAndSortSymbols(symbols []outlineSymbol) []outlineSymbol {
	sort.SliceStable(symbols, func(i, j int) bool {
		return symbols[i].Line < symbols[j].Line
	})
	if len(symbols) > outlineMaxSymbols {
		symbols = symbols[:outlineMaxSymbols]
	}
	return symbols
}

func clipOutlineName(name string) string {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) <= outlineMaxNameRunes {
		return name
	}
	return truncateRunes(name, outlineMaxNameRunes) + "…"
}

func outlineLanguage(rel string) (string, bool) {
	lang, ok := outlineLanguages[strings.ToLower(filepath.Ext(rel))]
	return lang, ok
}

func outlineDepth(rel string) int {
	return strings.Count(filepath.ToSlash(rel), "/")
}

func outlineIsTest(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	return strings.HasSuffix(base, "_test.go") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") ||
		strings.HasSuffix(base, "_test.py") ||
		strings.HasPrefix(base, "test_")
}

// outlinePriority keeps executable source ahead of markup when the budget is tight.
func outlinePriority(rel string) int {
	if lang, ok := outlineLanguage(rel); ok && lang == "html" {
		return 1
	}
	return 0
}

func outlineRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}
