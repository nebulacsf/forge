package tools

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"forge/internal/logx"
	"forge/internal/store"
)

const (
	symbolDefaultResults   = 50
	symbolMaxResults       = 200
	symbolSearchCandidates = 1000
	symbolQueryMaxRunes    = 200
)

var symbolKindGroups = map[string][]string{
	"function": {"function"},
	"method":   {"method"},
	"struct":   {"struct", "interface", "class", "type", "enum"},
	"variable": {"variable", "constant"},
}

// indexLocks serializes index refreshes per project root. Parallel agents can
// share a directory (a BUILD run plus read-only PLAN runs), and two concurrent
// walks would otherwise duplicate the scan and race on the same upserts.
var indexLocks = struct {
	sync.Mutex
	byRoot map[string]*sync.Mutex
}{byRoot: map[string]*sync.Mutex{}}

func lockRoot(root string) func() {
	indexLocks.Lock()
	mu, ok := indexLocks.byRoot[root]
	if !ok {
		mu = &sync.Mutex{}
		indexLocks.byRoot[root] = mu
	}
	indexLocks.Unlock()
	mu.Lock()
	return mu.Unlock
}

// EnsureProjectIndex walks a project once and refreshes both the compact
// repository outline and the queryable symbol index. Files whose mtime and size
// are unchanged are never read or parsed.
func EnsureProjectIndex(ctx context.Context, st *store.Store, projectID int64, root string) (string, error) {
	absRoot, err := outlineRoot(root)
	if err != nil {
		return "", err
	}
	unlock := lockRoot(absRoot)
	defer unlock()
	started := time.Now()

	var cachedOutline []store.RepoOutlineFile
	var cachedFiles map[string]store.ProjectSymbolFile
	if st != nil {
		cachedOutline, err = st.ListRepoOutline(absRoot)
		if err != nil {
			return "", err
		}
		if projectID != 0 {
			cachedFiles, err = st.ListProjectSymbolFiles(projectID)
			if err != nil {
				return "", err
			}
		}
	}
	cachedOutlineByPath := make(map[string]store.RepoOutlineFile, len(cachedOutline))
	for _, f := range cachedOutline {
		cachedOutlineByPath[f.FilePath] = f
	}

	seen := make(map[string]bool, len(cachedOutline))
	drop := make(map[string]bool)
	outlineUpserts := make([]store.RepoOutlineFile, 0)
	symbolFiles := make([]store.ProjectSymbolFile, 0)
	symbols := make([]store.ProjectSymbol, 0)
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

		cachedOutlineRow, hasOutline := cachedOutlineByPath[rel]
		needOutline := !hasOutline ||
			cachedOutlineRow.MTimeNS != info.ModTime().UnixNano() ||
			cachedOutlineRow.SizeBytes != info.Size()

		cachedSymbolFile, hasSymbols := cachedFiles[rel]
		needSymbols := projectID != 0 && (!hasSymbols ||
			cachedSymbolFile.MTimeNS != info.ModTime().UnixNano() ||
			cachedSymbolFile.SizeBytes != info.Size())

		if !needOutline && !needSymbols {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil || isBinary(data) {
			drop[rel] = true
			return nil
		}
		fileSymbols := scanFileSymbols(lang, p, data)

		if needOutline {
			outlineUpserts = append(outlineUpserts, store.RepoOutlineFile{
				ProjectPath: absRoot,
				FilePath:    rel,
				Language:    lang,
				MTimeNS:     info.ModTime().UnixNano(),
				SizeBytes:   info.Size(),
				Symbols:     formatFileOutline(fileSymbols),
				UpdatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
			})
		}
		if projectID != 0 {
			symbolFiles = append(symbolFiles, store.ProjectSymbolFile{
				ProjectID: projectID,
				FilePath:  rel,
				Language:  lang,
				MTimeNS:   info.ModTime().UnixNano(),
				SizeBytes: info.Size(),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			})
			symbols = append(symbols, projectSymbolsFrom(projectID, rel, fileSymbols)...)
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}

	outlineDeleted := make([]string, 0)
	for rel := range cachedOutlineByPath {
		if !seen[rel] || drop[rel] {
			outlineDeleted = append(outlineDeleted, rel)
		}
	}
	sort.Strings(outlineDeleted)

	symbolDeleted := make([]string, 0)
	if projectID != 0 {
		for rel := range cachedFiles {
			if !seen[rel] || drop[rel] {
				symbolDeleted = append(symbolDeleted, rel)
			}
		}
		sort.Strings(symbolDeleted)
	}

	var rows []store.RepoOutlineFile
	if st != nil {
		if err := st.SyncRepoOutline(absRoot, outlineUpserts, outlineDeleted); err != nil {
			return "", err
		}
		if projectID != 0 {
			if err := st.SyncProjectSymbols(projectID, symbolFiles, symbols, symbolDeleted); err != nil {
				return "", err
			}
		}
		rows, err = st.ListRepoOutline(absRoot)
		if err != nil {
			return "", err
		}
	} else {
		rows = outlineUpserts
	}

	outline := RenderRepoOutline(rows)
	logx.Debug("project index ensured", "root", absRoot, "project", projectID,
		"files", len(rows), "outline_parsed", len(outlineUpserts),
		"symbol_files", len(symbolFiles), "symbols", len(symbols),
		"deleted", len(outlineDeleted), "truncated", truncated,
		"chars", utf8.RuneCountInString(outline),
		"duration", time.Since(started).Round(time.Microsecond).String())
	return outline, nil
}

// EnsureProjectSymbols refreshes the cached symbol index without requiring the
// caller to render or consume the repository outline.
func EnsureProjectSymbols(ctx context.Context, st *store.Store, projectID int64, root string) error {
	if st == nil {
		return fmt.Errorf("symbol index is unavailable")
	}
	if projectID == 0 {
		return fmt.Errorf("symbol index is unavailable: missing project id")
	}
	_, err := EnsureProjectIndex(ctx, st, projectID, root)
	return err
}

func projectSymbolsFrom(projectID int64, rel string, symbols []fileSymbol) []store.ProjectSymbol {
	out := make([]store.ProjectSymbol, 0, len(symbols))
	for _, sym := range symbols {
		if sym.Name == "" || sym.StartLine <= 0 {
			continue
		}
		switch sym.Kind {
		case "package", "module", "heading", "id":
			continue
		}
		if sym.OutlineKind == "impl" {
			continue
		}
		end := sym.EndLine
		if end < sym.StartLine {
			end = sym.StartLine
		}
		qualified := sym.QualifiedName
		if qualified == "" {
			qualified = sym.Name
		}
		out = append(out, store.ProjectSymbol{
			ProjectID:     projectID,
			FilePath:      rel,
			Name:          sym.Name,
			QualifiedName: qualified,
			Kind:          sym.Kind,
			Parent:        sym.Parent,
			StartLine:     sym.StartLine,
			EndLine:       end,
		})
	}
	return out
}

// RefreshSymbolFile updates the symbol index for one file after a forged write.
func RefreshSymbolFile(st *store.Store, projectID int64, root, rel string) error {
	if st == nil || projectID == 0 {
		return nil
	}
	absRoot, err := outlineRoot(root)
	if err != nil {
		return err
	}
	rel = cleanProjectRel(rel)
	if rel == "" {
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
		return st.SyncProjectSymbols(projectID, nil, nil, []string{rel})
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() || info.Size() > outlineMaxFileSize {
		return st.SyncProjectSymbols(projectID, nil, nil, []string{rel})
	}
	data, err := os.ReadFile(abs)
	if err != nil || isBinary(data) {
		return st.SyncProjectSymbols(projectID, nil, nil, []string{rel})
	}
	fileSymbols := scanFileSymbols(lang, abs, data)
	return st.SyncProjectSymbols(projectID, []store.ProjectSymbolFile{{
		ProjectID: projectID,
		FilePath:  rel,
		Language:  lang,
		MTimeNS:   info.ModTime().UnixNano(),
		SizeBytes: info.Size(),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}}, projectSymbolsFrom(projectID, rel, fileSymbols), nil)
}

func cleanProjectRel(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	rel = strings.TrimPrefix(rel, "./")
	if rel == "." {
		return ""
	}
	return rel
}

func (m *Manager) symbolLookup(ctx context.Context, query, kind string) Result {
	query = strings.TrimSpace(query)
	if query == "" {
		return fail("symbol_lookup: query is required")
	}
	if utf8.RuneCountInString(query) > symbolQueryMaxRunes {
		query = truncateRunes(query, symbolQueryMaxRunes)
	}
	var kinds []string
	if k := strings.ToLower(strings.TrimSpace(kind)); k != "" {
		group, ok := symbolKindGroups[k]
		if !ok {
			return fail("symbol_lookup: kind must be one of function, struct, method, variable")
		}
		kinds = group
	}
	if m.Store == nil || m.ProjectID == 0 {
		return fail("symbol_lookup: symbol index unavailable for this session")
	}
	if err := EnsureProjectSymbols(ctx, m.Store, m.ProjectID, m.Root); err != nil {
		return fail("symbol_lookup: " + err.Error())
	}
	matches, err := m.Store.SearchProjectSymbols(m.ProjectID, query, kinds, symbolSearchCandidates)
	if err != nil {
		return fail("symbol_lookup: " + err.Error())
	}
	if len(matches) == 0 {
		return Result{Content: fmt.Sprintf("No symbols matching %q in the index. "+
			"The name may be spelled differently or live in free text (string literal, config key, interface field). "+
			"Fall back to grep_search once with a shorter pattern, then continue.", query)}
	}
	rankSymbols(matches, query)
	truncated := false
	if len(matches) > symbolMaxResults {
		matches = matches[:symbolMaxResults]
		truncated = true
	}
	return Result{Content: renderSymbolResults(matches, truncated)}
}

func rankSymbols(symbols []store.ProjectSymbol, query string) {
	q := strings.ToLower(query)
	score := func(sym store.ProjectSymbol) int {
		name := strings.ToLower(sym.Name)
		qualified := strings.ToLower(sym.QualifiedName)
		switch {
		case name == q || qualified == q:
			return 0
		case strings.HasPrefix(name, q) || strings.HasPrefix(qualified, q):
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(symbols, func(i, j int) bool {
		si, sj := score(symbols[i]), score(symbols[j])
		if si != sj {
			return si < sj
		}
		ni, nj := strings.ToLower(symbolDisplayName(symbols[i])), strings.ToLower(symbolDisplayName(symbols[j]))
		if ni != nj {
			return ni < nj
		}
		if symbols[i].FilePath != symbols[j].FilePath {
			return symbols[i].FilePath < symbols[j].FilePath
		}
		return symbols[i].StartLine < symbols[j].StartLine
	})
}

func renderSymbolResults(symbols []store.ProjectSymbol, truncated bool) string {
	var b strings.Builder
	for _, sym := range symbols {
		fmt.Fprintf(&b, "%s [%s] -> %s:%d-%d (read_file path=%q start_line=%d end_line=%d)\n",
			symbolDisplayName(sym), sym.Kind, sym.FilePath, sym.StartLine, sym.EndLine,
			sym.FilePath, sym.StartLine, symbolReadEnd(sym))
	}
	if truncated {
		b.WriteString("[results truncated; narrow the query or add kind]\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// symbolReadEnd caps the suggested read window at maxReadLines so the hint never
// asks for more than read_file can return in one call.
func symbolReadEnd(sym store.ProjectSymbol) int {
	end := sym.EndLine
	if end < sym.StartLine {
		end = sym.StartLine
	}
	if end-sym.StartLine+1 > maxReadLines {
		end = sym.StartLine + maxReadLines - 1
	}
	return end
}

func symbolDisplayName(sym store.ProjectSymbol) string {
	if strings.TrimSpace(sym.QualifiedName) != "" {
		return sym.QualifiedName
	}
	return sym.Name
}
