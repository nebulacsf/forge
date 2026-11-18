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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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

// fileSymbol is the rich form used by the queryable symbol index. OutlineKind
// is the coarse label rendered in repo_outline; an empty OutlineKind means the
// declaration is indexed for symbol_lookup but omitted from the prompt map.
type fileSymbol struct {
	Name          string
	QualifiedName string
	OutlineName   string
	Kind          string
	OutlineKind   string
	Parent        string
	StartLine     int
	EndLine       int
}

type symbolContainer struct {
	Name   string
	Start  int
	End    int
	Indent int
}

var (
	jsClassRE  = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	jsFuncRE   = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)
	jsArrowRE  = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`)
	jsMethodRE = regexp.MustCompile(`^\s{2,8}(?:(?:public|private|protected|static|async|readonly|get|set)\s+)*([A-Za-z_$][\w$]*)\s*\([^;]*\)\s*(?::\s*[^{]+)?\{\s*$`)

	pyClassRE = regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`)
	pyDefRE   = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`)

	rsFnRE   = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?fn\s+([A-Za-z_]\w*)`)
	rsModRE  = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?mod\s+([A-Za-z_]\w*)`)
	rsImplRE = regexp.MustCompile(`^\s*impl\s+(?:<[^>]*>\s*)?([A-Za-z_]\w*)`)

	javaMethodRE = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|final|abstract|synchronized|native|default)\s+)+(?:[\w<>\[\],.?]+\s+)+([A-Za-z_$][\w$]*)\s*\([^;]*\)\s*(?:throws[^{]+)?\{`)

	rbClassRE  = regexp.MustCompile(`^\s*class\s+([A-Z]\w*(?:::\w+)*)`)
	rbModuleRE = regexp.MustCompile(`^\s*module\s+([A-Z]\w*(?:::\w+)*)`)
	rbDefRE    = regexp.MustCompile(`^\s*def\s+(?:self\.)?([A-Za-z_]\w*[?!]?)`)

	phpFuncRE = regexp.MustCompile(`^\s*(?:(?:public|protected|private|static|final|abstract)\s+)*function\s+([A-Za-z_]\w*)`)

	htmlHeadingRE = regexp.MustCompile(`(?i)<h[1-3][^>]*>(.*?)</h[1-3]>`)
	htmlIDRE      = regexp.MustCompile(`(?i)<([a-z][\w-]*)[^>]*\bid=["']([^"']+)["']`)
	htmlTagRE     = regexp.MustCompile(`(?i)<[^>]+>`)

	jsTypeDeclRE   = regexp.MustCompile(`^\s*(?:export\s+)?(interface|type|enum)\s+([A-Za-z_$][\w$]*)`)
	jsVarRE        = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)`)
	jsFuncAssignRE = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?function\b`)
	pyAssignRE     = regexp.MustCompile(`^\s*([A-Za-z_]\w*)\s*=(?:[^=]|$)`)
	rsTypeDeclRE   = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(struct|enum|trait|union|type)\s+([A-Za-z_]\w*)`)
	javaTypeDeclRE = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|final|abstract|sealed)\s+)*(class|interface|enum|record)\s+([A-Za-z_$][\w$]*)`)
	phpTypeDeclRE  = regexp.MustCompile(`^\s*(?:abstract\s+|final\s+)?(class|interface|trait|enum)\s+([A-Za-z_]\w*)`)
)

// Control-flow lines such as "if (x) {" look like methods; skip them.
var jsControlKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"function": true, "return": true, "else": true, "do": true, "try": true,
	"finally": true, "with": true, "await": true, "yield": true, "throw": true,
	"new": true, "delete": true, "typeof": true, "void": true, "in": true, "of": true,
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
// It is the project-less compatibility wrapper around EnsureProjectIndex.
func EnsureRepoOutline(ctx context.Context, st *store.Store, root string) (string, error) {
	return EnsureProjectIndex(ctx, st, 0, root)
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
		b.WriteString("[outline truncated; use symbol_lookup for a known declaration name, or list_directory and grep_search for the rest]\n")
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
	return outlineSymbolsFrom(scanFileSymbols(lang, abs, data))
}

func outlineSymbolsFrom(symbols []fileSymbol) []outlineSymbol {
	out := make([]outlineSymbol, 0, len(symbols))
	for _, s := range symbols {
		if s.OutlineKind == "" || s.StartLine <= 0 {
			continue
		}
		name := s.Name
		if s.OutlineName != "" {
			name = s.OutlineName
		}
		out = append(out, outlineSymbol{Line: s.StartLine, Kind: s.OutlineKind, Name: name})
	}
	return limitAndSortSymbols(out)
}

func formatFileOutline(symbols []fileSymbol) string {
	return formatOutlineSymbols(outlineSymbolsFrom(symbols))
}

// scanFileSymbols is the single parsing entry point used by both the compact
// repository outline and the queryable symbol index.
func scanFileSymbols(lang, abs string, data []byte) []fileSymbol {
	switch lang {
	case "go":
		return scanGoSymbols(abs, data)
	case "html":
		return scanHTMLSymbols(data)
	default:
		return scanRegexSymbols(lang, data)
	}
}

func scanGoSymbols(abs string, data []byte) []fileSymbol {
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
	out := make([]fileSymbol, 0, 16)
	if file.Name != nil {
		out = append(out, fileSymbol{
			Name: file.Name.Name, QualifiedName: file.Name.Name,
			Kind: "package", OutlineKind: "package",
			StartLine: lineOf(file.Package), EndLine: lineOf(file.Package),
		})
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name == nil {
						continue
					}
					kind, outlineKind := "type", "type"
					switch ts.Type.(type) {
					case *ast.StructType:
						kind, outlineKind = "struct", "struct"
					case *ast.InterfaceType:
						kind, outlineKind = "interface", "interface"
					}
					out = append(out, fileSymbol{
						Name: ts.Name.Name, QualifiedName: ts.Name.Name,
						Kind: kind, OutlineKind: outlineKind,
						StartLine: lineOf(ts.Pos()), EndLine: lineOf(ts.End()),
					})
				}
			case token.VAR, token.CONST:
				kind := "variable"
				if d.Tok == token.CONST {
					kind = "constant"
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					start, end := lineOf(vs.Pos()), lineOf(vs.End())
					for _, n := range vs.Names {
						if n == nil {
							continue
						}
						out = append(out, fileSymbol{
							Name: n.Name, QualifiedName: n.Name, Kind: kind,
							StartLine: start, EndLine: end,
						})
					}
				}
			}
		case *ast.FuncDecl:
			if d.Name == nil {
				continue
			}
			kind, outlineKind := "function", "func"
			name, qualified, parent := d.Name.Name, d.Name.Name, ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind, outlineKind = "method", "method"
				if recv := receiverTypeName(d.Recv.List[0].Type); recv != "" {
					parent = recv
					qualified = recv + "." + name
				}
			}
			out = append(out, fileSymbol{
				Name: name, QualifiedName: qualified, OutlineName: qualified,
				Kind: kind, OutlineKind: outlineKind,
				Parent: parent, StartLine: lineOf(d.Pos()), EndLine: lineOf(d.End()),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartLine < out[j].StartLine })
	return out
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

func scanRegexSymbols(lang string, data []byte) []fileSymbol {
	lines := sourceLines(data)
	switch lang {
	case "js", "ts":
		return scanJSSymbols(lines)
	case "py":
		return scanPySymbols(lines)
	case "rs":
		return scanRustSymbols(lines)
	case "java":
		return scanJavaSymbols(lines)
	case "rb":
		return scanRubySymbols(lines)
	case "php":
		return scanPHPSymbols(lines)
	default:
		return nil
	}
}

func scanJSSymbols(lines []string) []fileSymbol {
	out := make([]fileSymbol, 0, 16)
	containers := make([]symbolContainer, 0, 4)
	for i, line := range lines {
		if isCommentLine(line) {
			continue
		}
		indent := lineIndent(line)
		if m := jsClassRE.FindStringSubmatch(line); len(m) >= 2 && strings.TrimSpace(m[1]) != "" {
			end := blockEndLine(lines, i)
			containers = append(containers, symbolContainer{m[1], i + 1, end, indent})
			out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "class",
				OutlineKind: "class", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := jsFuncRE.FindStringSubmatch(line); len(m) >= 2 && strings.TrimSpace(m[1]) != "" {
			end := blockEndLine(lines, i)
			out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "function",
				OutlineKind: "func", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := jsArrowRE.FindStringSubmatch(line); len(m) >= 2 && strings.TrimSpace(m[1]) != "" {
			end := blockEndLine(lines, i)
			out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "function",
				OutlineKind: "func", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := jsTypeDeclRE.FindStringSubmatch(line); len(m) >= 3 {
			kind := "type"
			if m[1] == "interface" {
				kind = "interface"
			}
			end := blockEndLine(lines, i)
			out = append(out, fileSymbol{Name: m[2], QualifiedName: m[2], Kind: kind,
				OutlineKind: "type", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := jsMethodRE.FindStringSubmatch(line); len(m) >= 2 && strings.TrimSpace(m[1]) != "" {
			name := m[1]
			if jsControlKeywords[name] {
				continue
			}
			if parent := enclosingContainer(containers, i+1, indent); parent != "" {
				end := blockEndLine(lines, i)
				out = append(out, fileSymbol{Name: name, QualifiedName: parent + "." + name,
					Kind: "method", OutlineKind: "method", Parent: parent,
					StartLine: i + 1, EndLine: end})
				continue
			}
		}
		if indent == 0 {
			if m := jsFuncAssignRE.FindStringSubmatch(line); len(m) >= 2 {
				end := blockEndLine(lines, i)
				out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "function",
					OutlineKind: "func", StartLine: i + 1, EndLine: end})
				continue
			}
			if m := jsVarRE.FindStringSubmatch(line); len(m) >= 2 {
				end := blockEndLine(lines, i)
				out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "variable",
					StartLine: i + 1, EndLine: end})
			}
		}
	}
	return out
}

func scanPySymbols(lines []string) []fileSymbol {
	out := make([]fileSymbol, 0, 16)
	containers := make([]symbolContainer, 0, 4)
	for i, line := range lines {
		if isCommentLine(line) {
			continue
		}
		indent := lineIndent(line)
		if m := pyClassRE.FindStringSubmatch(line); len(m) >= 2 {
			end := indentEndLine(lines, i)
			containers = append(containers, symbolContainer{m[1], i + 1, end, indent})
			out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "class",
				OutlineKind: "class", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := pyDefRE.FindStringSubmatch(line); len(m) >= 2 {
			parent := enclosingContainer(containers, i+1, indent)
			kind, outlineKind := "function", "func"
			qualified := m[1]
			if parent != "" {
				kind, outlineKind = "method", "method"
				qualified = parent + "." + m[1]
			}
			end := indentEndLine(lines, i)
			out = append(out, fileSymbol{Name: m[1], QualifiedName: qualified, Kind: kind,
				OutlineKind: outlineKind, Parent: parent, StartLine: i + 1, EndLine: end})
			continue
		}
		if indent == 0 {
			if m := pyAssignRE.FindStringSubmatch(line); len(m) >= 2 {
				out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "variable",
					StartLine: i + 1, EndLine: i + 1})
			}
		}
	}
	return out
}

func scanRustSymbols(lines []string) []fileSymbol {
	out := make([]fileSymbol, 0, 16)
	containers := make([]symbolContainer, 0, 4)
	for i, line := range lines {
		if isCommentLine(line) {
			continue
		}
		indent := lineIndent(line)
		if m := rsImplRE.FindStringSubmatch(line); len(m) >= 2 {
			end := blockEndLine(lines, i)
			containers = append(containers, symbolContainer{m[1], i + 1, end, indent})
			out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "type",
				OutlineKind: "impl", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := rsFnRE.FindStringSubmatch(line); len(m) >= 2 {
			parent := enclosingContainer(containers, i+1, indent)
			kind, outlineKind := "function", "fn"
			qualified := m[1]
			if parent != "" {
				kind, outlineKind = "method", "method"
				qualified = parent + "." + m[1]
			}
			end := blockEndLine(lines, i)
			out = append(out, fileSymbol{Name: m[1], QualifiedName: qualified, Kind: kind,
				OutlineKind: outlineKind, Parent: parent, StartLine: i + 1, EndLine: end})
			continue
		}
		if m := rsTypeDeclRE.FindStringSubmatch(line); len(m) >= 3 {
			kind := "type"
			switch m[1] {
			case "struct", "union":
				kind = "struct"
			case "trait":
				kind = "interface"
			}
			end := blockEndLine(lines, i)
			out = append(out, fileSymbol{Name: m[2], QualifiedName: m[2], Kind: kind,
				OutlineKind: "type", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := rsModRE.FindStringSubmatch(line); len(m) >= 2 {
			end := blockEndLine(lines, i)
			out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "module",
				OutlineKind: "mod", StartLine: i + 1, EndLine: end})
		}
	}
	return out
}

func scanJavaSymbols(lines []string) []fileSymbol {
	out := make([]fileSymbol, 0, 16)
	containers := make([]symbolContainer, 0, 4)
	for i, line := range lines {
		if isCommentLine(line) {
			continue
		}
		indent := lineIndent(line)
		if m := javaTypeDeclRE.FindStringSubmatch(line); len(m) >= 3 {
			kind := "class"
			switch m[1] {
			case "interface":
				kind = "interface"
			case "enum", "record":
				kind = "type"
			}
			end := blockEndLine(lines, i)
			containers = append(containers, symbolContainer{m[2], i + 1, end, indent})
			out = append(out, fileSymbol{Name: m[2], QualifiedName: m[2], Kind: kind,
				OutlineKind: "type", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := javaMethodRE.FindStringSubmatch(line); len(m) >= 2 {
			if parent := enclosingContainer(containers, i+1, indent); parent != "" {
				end := blockEndLine(lines, i)
				out = append(out, fileSymbol{Name: m[1], QualifiedName: parent + "." + m[1],
					Kind: "method", OutlineKind: "method", Parent: parent,
					StartLine: i + 1, EndLine: end})
			}
		}
	}
	return out
}

func scanRubySymbols(lines []string) []fileSymbol {
	out := make([]fileSymbol, 0, 16)
	containers := make([]symbolContainer, 0, 4)
	for i, line := range lines {
		if isCommentLine(line) {
			continue
		}
		indent := lineIndent(line)
		if m := rbClassRE.FindStringSubmatch(line); len(m) >= 2 {
			end := indentEndLine(lines, i)
			containers = append(containers, symbolContainer{m[1], i + 1, end, indent})
			out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "class",
				OutlineKind: "class", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := rbModuleRE.FindStringSubmatch(line); len(m) >= 2 {
			end := indentEndLine(lines, i)
			containers = append(containers, symbolContainer{m[1], i + 1, end, indent})
			out = append(out, fileSymbol{Name: m[1], QualifiedName: m[1], Kind: "module",
				OutlineKind: "module", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := rbDefRE.FindStringSubmatch(line); len(m) >= 2 {
			parent := enclosingContainer(containers, i+1, indent)
			kind, outlineKind := "function", "def"
			qualified := m[1]
			if parent != "" {
				kind, outlineKind = "method", "method"
				qualified = parent + "." + m[1]
			}
			end := indentEndLine(lines, i)
			out = append(out, fileSymbol{Name: m[1], QualifiedName: qualified, Kind: kind,
				OutlineKind: outlineKind, Parent: parent, StartLine: i + 1, EndLine: end})
		}
	}
	return out
}

func scanPHPSymbols(lines []string) []fileSymbol {
	out := make([]fileSymbol, 0, 16)
	containers := make([]symbolContainer, 0, 4)
	for i, line := range lines {
		if isCommentLine(line) {
			continue
		}
		indent := lineIndent(line)
		if m := phpTypeDeclRE.FindStringSubmatch(line); len(m) >= 3 {
			kind := "class"
			switch m[1] {
			case "interface":
				kind = "interface"
			case "trait", "enum":
				kind = "type"
			}
			end := blockEndLine(lines, i)
			containers = append(containers, symbolContainer{m[2], i + 1, end, indent})
			out = append(out, fileSymbol{Name: m[2], QualifiedName: m[2], Kind: kind,
				OutlineKind: "type", StartLine: i + 1, EndLine: end})
			continue
		}
		if m := phpFuncRE.FindStringSubmatch(line); len(m) >= 2 {
			parent := enclosingContainer(containers, i+1, indent)
			kind, outlineKind := "function", "func"
			qualified := m[1]
			if parent != "" {
				kind, outlineKind = "method", "method"
				qualified = parent + "." + m[1]
			}
			end := blockEndLine(lines, i)
			out = append(out, fileSymbol{Name: m[1], QualifiedName: qualified, Kind: kind,
				OutlineKind: outlineKind, Parent: parent, StartLine: i + 1, EndLine: end})
		}
	}
	return out
}

func scanHTMLSymbols(data []byte) []fileSymbol {
	out := make([]fileSymbol, 0, 8)
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if m := htmlHeadingRE.FindStringSubmatch(line); len(m) >= 2 {
			name := strings.Join(strings.Fields(htmlTagRE.ReplaceAllString(m[1], " ")), " ")
			if name != "" {
				out = append(out, fileSymbol{Name: clipOutlineName(name), QualifiedName: clipOutlineName(name),
					Kind: "heading", OutlineKind: "heading", StartLine: i + 1, EndLine: i + 1})
				continue
			}
		}
		if m := htmlIDRE.FindStringSubmatch(line); len(m) >= 3 {
			name := clipOutlineName(strings.ToLower(m[1]) + "#" + m[2])
			out = append(out, fileSymbol{Name: name, QualifiedName: name,
				Kind: "id", OutlineKind: "id", StartLine: i + 1, EndLine: i + 1})
		}
	}
	return out
}

func sourceLines(data []byte) []string {
	lines := strings.Split(string(data), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func isCommentLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed == "" ||
		strings.HasPrefix(trimmed, "//") ||
		strings.HasPrefix(trimmed, "/*") ||
		strings.HasPrefix(trimmed, "*") ||
		strings.HasPrefix(trimmed, "#")
}

func lineIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

func enclosingContainer(containers []symbolContainer, line, indent int) string {
	for i := len(containers) - 1; i >= 0; i-- {
		c := containers[i]
		if c.Start < line && line <= c.End && c.Indent < indent {
			return c.Name
		}
	}
	return ""
}

// blockEndLine finds the closing line of a brace-delimited declaration. It
// tolerates multi-line signatures and ignores braces inside comments/strings.
func blockEndLine(lines []string, start int) int {
	if start < 0 || start >= len(lines) {
		return start + 1
	}
	var counter braceCounter
	depth := 0
	opened := false
	for i := start; i < len(lines); i++ {
		delta, sawOpen := counter.count(lines[i])
		depth += delta
		opened = opened || sawOpen
		if opened && depth <= 0 {
			return i + 1
		}
		if !opened && strings.Contains(lines[i], ";") {
			return i + 1
		}
	}
	if opened {
		return len(lines)
	}
	return start + 1
}

// indentEndLine finds the last line of an indentation-delimited block.
func indentEndLine(lines []string, start int) int {
	if start < 0 || start >= len(lines) {
		return start + 1
	}
	indent := lineIndent(lines[start])
	last := start
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if lineIndent(line) <= indent {
			break
		}
		last = i
	}
	return last + 1
}

type braceCounter struct {
	blockComment bool
	quote        byte
	escaped      bool
}

func (c *braceCounter) count(line string) (delta int, opened bool) {
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if c.blockComment {
			if ch == '*' && i+1 < len(line) && line[i+1] == '/' {
				c.blockComment = false
				i++
			}
			continue
		}
		if c.quote != 0 {
			if c.escaped {
				c.escaped = false
				continue
			}
			if ch == '\\' {
				c.escaped = true
				continue
			}
			if ch == c.quote {
				c.quote = 0
			}
			continue
		}
		switch ch {
		case '/':
			if i+1 < len(line) && line[i+1] == '/' {
				return delta, opened
			}
			if i+1 < len(line) && line[i+1] == '*' {
				c.blockComment = true
				i++
			}
		case '\'', '"', '`':
			c.quote = ch
		case '{':
			delta++
			opened = true
		case '}':
			delta--
		}
	}
	return delta, opened
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
