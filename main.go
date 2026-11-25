package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/fatih/color"
)

// version is reported by --version.
const version = "1.0.0"

const (
	// defaultMaxFiles caps how many files are scanned so that very large
	// repositories finish quickly. Totals only cover the files that were scanned.
	defaultMaxFiles = 200
	// maxFileBytes skips generated, bundled, or minified files large enough to
	// skew the totals.
	maxFileBytes = 1 << 20
	// binarySniffBytes is how much of a file is checked for NUL bytes.
	binarySniffBytes = 8000
	// maxSkipWarnings limits how many per-file warnings are printed.
	maxSkipWarnings = 10
	// pathColumnWidth is the display width reserved for the file path column.
	pathColumnWidth = 45
	// displayLimit is how many files the table output lists.
	displayLimit = 20
)

var (
	errFileTooLarge   = errors.New("file too large to scan")
	errBinaryFile     = errors.New("binary content")
	errNonRegularFile = errors.New("non-regular file")
)

// commentPair is a start/end delimiter pair used for block comments.
type commentPair struct {
	start     string
	end       string
	lineStart bool // must appear at column 0 and be followed by whitespace or EOL
}

// language describes how comments and string literals are written in one
// file type. It is the single source of truth for "is this a code file" and
// for how to strip comments when counting lines.
type language struct {
	lineComments  []string      // markers that comment out the rest of a line
	blockComments []commentPair // start/end markers for multi-line comments
	quotes        []string      // string delimiters whose contents are not parsed
	multiline     []string      // delimiters that may span lines
	rawQuotes     []string      // delimiters where backslash does not escape
	charLiteral   bool          // single-quoted char literals only (e.g. Rust lifetimes)
}

var (
	// cQuotes covers the languages that use double and single quoted strings.
	cQuotes = []string{`"`, `'`}
	// backtickQuotes adds backtick strings used by Go and JS/TS.
	backtickQuotes = []string{`"`, `'`, "`"}

	// cLike is the "//" + "/* */" syntax shared by most compiled languages.
	cLike = language{
		lineComments:  []string{"//"},
		blockComments: []commentPair{{start: "/*", end: "*/"}},
		quotes:        cQuotes,
	}

	// jsLike covers JavaScript and TypeScript, where backtick template literals
	// span lines and still support backslash escapes.
	jsLike = language{
		lineComments:  []string{"//"},
		blockComments: []commentPair{{start: "/*", end: "*/"}},
		quotes:        backtickQuotes,
		multiline:     []string{"`"},
	}

	// hashLike covers shells, where "#" is the only comment marker.
	hashLike = language{
		lineComments: []string{"#"},
		quotes:       cQuotes,
	}

	// htmlLike covers markup, which only has "<!-- -->" comments. Quotes are
	// omitted so apostrophes and quotation marks in prose do not swallow "<!--".
	htmlLike = language{
		blockComments: []commentPair{{start: "<!--", end: "-->"}},
	}
)

var languages = map[string]language{
	".go": {
		lineComments:  []string{"//"},
		blockComments: []commentPair{{start: "/*", end: "*/"}},
		quotes:        backtickQuotes,
		multiline:     []string{"`"},
		rawQuotes:     []string{"`"},
	},
	".cpp":  cLike,
	".c":    cLike,
	".h":    cLike,
	".java": cLike,
	".rs": {
		lineComments:  []string{"//"},
		blockComments: []commentPair{{start: "/*", end: "*/"}},
		quotes:        []string{`"`},
		charLiteral:   true,
	},
	".swift": cLike,
	".kt":    cLike,
	".scala": cLike,
	".js":    jsLike,
	".ts":    jsLike,
	".tsx":   jsLike,
	".jsx":   jsLike,
	".py": {
		lineComments: []string{"#"},
		// Triple-quoted blocks are treated as comments, matching how most
		// line counters classify docstrings.
		blockComments: []commentPair{{start: `"""`, end: `"""`}, {start: `'''`, end: `'''`}},
		quotes:        cQuotes,
	},
	".rb": {
		lineComments:  []string{"#"},
		blockComments: []commentPair{{start: "=begin", end: "=end", lineStart: true}},
		quotes:        cQuotes,
	},
	".php": {
		lineComments:  []string{"//", "#"},
		blockComments: []commentPair{{start: "/*", end: "*/"}},
		quotes:        cQuotes,
	},
	".lua": {
		lineComments: []string{"--"},
		blockComments: []commentPair{
			{start: "--[[", end: "]]"},
			{start: "--[=[", end: "]=]"},
			{start: "--[==[", end: "]==]"},
			{start: "--[===[", end: "]===]"},
		},
		quotes: cQuotes,
	},
	".sh":   hashLike,
	".bash": hashLike,
	".vue": {
		lineComments:  []string{"//"},
		blockComments: []commentPair{{start: "/*", end: "*/"}, {start: "<!--", end: "-->"}},
		quotes:        backtickQuotes,
		multiline:     []string{"`"},
	},
	".html": htmlLike,
	".htm":  htmlLike,
	".css": {
		blockComments: []commentPair{{start: "/*", end: "*/"}},
		quotes:        cQuotes,
	},
}

// FileInfo holds the effective line count for one source file.
type FileInfo struct {
	path      string
	lineCount int
}

// isCodeFile reports whether ext is a language this tool can count.
func isCodeFile(ext string) bool {
	_, ok := languages[ext]
	return ok
}

// countEffectiveLines returns how many lines of the file contain code, using
// a character-level scan so that comment markers inside string literals,
// trailing block comments, and code after a closing marker are all handled.
//
// Files that are too large or that contain NUL bytes are rejected with
// errFileTooLarge or errBinaryFile so callers can skip them without aborting.
//
// UTF-8, UTF-8 with BOM, UTF-16LE, and UTF-16BE (both with BOM) are decoded.
func countEffectiveLines(filePath string) (int, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%w (%s)", errNonRegularFile, info.Mode().Type())
	}
	if info.Size() > maxFileBytes {
		return 0, fmt.Errorf("%w (%d bytes)", errFileTooLarge, info.Size())
	}

	content, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return 0, err
	}
	if len(content) > maxFileBytes {
		return 0, fmt.Errorf("%w (>%d bytes)", errFileTooLarge, maxFileBytes)
	}

	text, err := decodeContent(content)
	if err != nil {
		return 0, err
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	spec := languages[ext]

	effective := 0
	blockIdx := -1 // index into spec.blockComments while inside a block comment
	quote := ""    // active string delimiter that survived the previous line
	for _, line := range strings.Split(text, "\n") {
		if lineHasCode(line, spec, &blockIdx, &quote) {
			effective++
		}
	}

	return effective, nil
}

// decodeContent converts file bytes to text. Binary-looking content is
// rejected so that generated artifacts are not reported as source.
func decodeContent(content []byte) (string, error) {
	switch {
	case bytes.HasPrefix(content, []byte{0xFF, 0xFE, 0x00, 0x00}):
		return "", errBinaryFile // UTF-32LE or binary
	case bytes.HasPrefix(content, []byte{0xFF, 0xFE}):
		return decodeUTF16(content[2:], true)
	case bytes.HasPrefix(content, []byte{0xFE, 0xFF}):
		return decodeUTF16(content[2:], false)
	}

	head := content
	if len(head) > binarySniffBytes {
		head = head[:binarySniffBytes]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "", errBinaryFile
	}

	return strings.TrimPrefix(string(content), "\ufeff"), nil
}

// decodeUTF16 converts UTF-16 code units to a Go string, rejecting odd-length
// or NUL-containing streams as binary.
func decodeUTF16(content []byte, littleEndian bool) (string, error) {
	if len(content)%2 != 0 {
		return "", errBinaryFile
	}
	sniffUnits := binarySniffBytes / 2
	units := make([]uint16, 0, len(content)/2)
	for i := 0; i < len(content); i += 2 {
		var u uint16
		if littleEndian {
			u = uint16(content[i]) | uint16(content[i+1])<<8
		} else {
			u = uint16(content[i])<<8 | uint16(content[i+1])
		}
		if len(units) < sniffUnits && u == 0 {
			return "", errBinaryFile
		}
		units = append(units, u)
	}
	return string(utf16.Decode(units)), nil
}

// lineHasCode reports whether a single line contributes code. blockIdx carries
// the open block comment across lines, and quote carries a multiline string
// whose delimiter may span lines.
func lineHasCode(line string, spec language, blockIdx *int, quote *string) bool {
	hasCode := *quote != "" // a continuation of a multiline string is still content

	i := 0
	for i < len(line) {
		rest := line[i:]

		if *blockIdx >= 0 {
			pair := spec.blockComments[*blockIdx]
			if pair.lineStart {
				if i == 0 && matchLineStartMarker(rest, pair.end) {
					*blockIdx = -1
				}
				break
			}
			if strings.HasPrefix(rest, pair.end) {
				i += len(pair.end)
				*blockIdx = -1
				continue
			}
			i++
			continue
		}

		if *quote != "" {
			if rest[0] == '\\' && !isNoEscapeDelimiter(spec, *quote) {
				i++
				if i < len(line) {
					_, size := utf8.DecodeRuneInString(line[i:])
					i += size
				}
				continue
			}
			if strings.HasPrefix(rest, *quote) {
				i += len(*quote)
				*quote = ""
				continue
			}
			i++
			continue
		}

		if idx := matchBlockCommentStart(rest, i == 0, spec); idx >= 0 {
			pair := spec.blockComments[idx]
			*blockIdx = idx
			if pair.lineStart {
				break
			}
			i += len(pair.start)
			continue
		}

		if spec.charLiteral && rest[0] == '\'' {
			if n := matchCharLiteral(rest); n > 0 {
				hasCode = true
				i += n
				continue
			}
		}

		if q := matchQuote(rest, spec); q != "" {
			hasCode = true
			*quote = q
			i += len(q)
			continue
		}

		if matchLineComment(rest, spec) {
			break
		}

		r, size := utf8.DecodeRuneInString(rest)
		if !unicode.IsSpace(r) {
			hasCode = true
		}
		i += size
	}

	// Ordinary strings end with the line; only multiline delimiters may span lines.
	if *quote != "" && !isMultilineDelimiter(spec, *quote) {
		*quote = ""
	}

	return hasCode
}

// isMultilineDelimiter reports whether delim is allowed to span multiple lines.
func isMultilineDelimiter(spec language, delim string) bool {
	for _, m := range spec.multiline {
		if m == delim {
			return true
		}
	}
	return false
}

// isNoEscapeDelimiter reports whether backslash escaping is disabled inside delim.
func isNoEscapeDelimiter(spec language, delim string) bool {
	for _, raw := range spec.rawQuotes {
		if raw == delim {
			return true
		}
	}
	return false
}

// matchLineStartMarker reports whether rest begins with marker followed by
// end-of-line or whitespace (e.g. Ruby's =begin / =end).
func matchLineStartMarker(rest, marker string) bool {
	if !strings.HasPrefix(rest, marker) {
		return false
	}
	if len(rest) == len(marker) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest[len(marker):])
	return unicode.IsSpace(r)
}

// matchCharLiteral returns the byte length of a single-quoted character literal
// at rest[0], or 0 when the quote begins a lifetime such as 'a or 'static.
func matchCharLiteral(rest string) int {
	if len(rest) < 3 || rest[0] != '\'' {
		return 0
	}
	if rest[1] == '\\' {
		if len(rest) < 4 {
			return 0
		}
		if rest[2] == '\'' {
			if rest[3] == '\'' {
				return 4
			}
			return 0
		}
		if rest[2] == ' ' || rest[2] == '\t' || rest[2] == '\r' || rest[2] == '\n' {
			return 0
		}
		limit := len(rest)
		if limit > 14 {
			limit = 14
		}
		for i := 3; i < limit; i++ {
			if rest[i] == '\'' {
				return i + 1
			}
			if rest[i] == ' ' || rest[i] == '\t' || rest[i] == '\r' || rest[i] == '\n' {
				return 0
			}
		}
		return 0
	}
	r, size := utf8.DecodeRuneInString(rest[1:])
	if r == '\'' || 1+size >= len(rest) || rest[1+size] != '\'' {
		return 0
	}
	return 1 + size + 1
}

// matchBlockCommentStart returns the index of the block comment marker that
// starts at the beginning of rest, or -1.
func matchBlockCommentStart(rest string, atLineStart bool, spec language) int {
	for idx, pair := range spec.blockComments {
		if pair.lineStart {
			if atLineStart && matchLineStartMarker(rest, pair.start) {
				return idx
			}
			continue
		}
		if strings.HasPrefix(rest, pair.start) {
			return idx
		}
	}
	return -1
}

// matchQuote returns the string delimiter that starts at the beginning of rest,
// or an empty string. Block comment markers are matched first, so Python's
// triple quotes win over a single quote.
func matchQuote(rest string, spec language) string {
	for _, q := range spec.quotes {
		if strings.HasPrefix(rest, q) {
			return q
		}
	}
	return ""
}

// matchLineComment reports whether rest begins with a single-line comment marker.
func matchLineComment(rest string, spec language) bool {
	for _, marker := range spec.lineComments {
		if strings.HasPrefix(rest, marker) {
			return true
		}
	}
	return false
}

var ignoredDirs = map[string]bool{
	"node_modules":     true,
	"__pycache__":      true,
	".venv":            true,
	"venv":             true,
	".env":             true,
	"vendor":           true,
	"bower_components": true,
	".git":             true,
	".svn":             true,
	".hg":              true,
	"dist":             true,
	"build":            true,
	".next":            true,
	"out":              true,
	".cache":           true,
	"coverage":         true,
	"bin":              true,
	"obj":              true,
	"target":           true,
	"Pods":             true,
	".idea":            true,
	".vscode":          true,
	".gradle":          true,
	".terraform":       true,
	".pytest_cache":    true,
	".mypy_cache":      true,
	".ruff_cache":      true,
	".tox":             true,
	".pnpm-store":      true,
}

// isIgnoredDir reports whether a directory name is always skipped.
func isIgnoredDir(dirName string) bool {
	return ignoredDirs[dirName]
}

// ignoreRule is one gitignore-style pattern.
type ignoreRule struct {
	pattern  string
	negate   bool
	dirOnly  bool
	anchored bool
	segs     []string
}

// ignoreMatcher applies a list of rules in order; later rules win.
type ignoreMatcher struct {
	rules []ignoreRule
}

// parsePattern builds a rule from a single pattern, without comment or
// negation handling.
func parsePattern(pattern string) (ignoreRule, bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ignoreRule{}, false
	}

	dirOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")

	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "/")
	if pattern == "" {
		return ignoreRule{}, false
	}

	rawSegs := strings.Split(pattern, "/")
	segs := make([]string, 0, len(rawSegs))
	for _, seg := range rawSegs {
		if seg == "" {
			return ignoreRule{}, false
		}
		if _, err := path.Match(seg, ""); err != nil {
			return ignoreRule{}, false
		}
		if seg == "**" && len(segs) > 0 && segs[len(segs)-1] == "**" {
			continue
		}
		segs = append(segs, seg)
	}

	// A trailing "/**" (e.g. "src/gen/**") matches everything inside the
	// directory rather than the directory itself, so negation rules like
	// "!src/gen/keep.go" can still take effect.
	if len(segs) > 1 && segs[len(segs)-1] == "**" {
		segs[len(segs)-1] = "*"
		segs = append(segs, "**")
	}

	return ignoreRule{
		pattern:  pattern,
		dirOnly:  dirOnly,
		anchored: anchored || strings.Contains(pattern, "/"),
		segs:     segs,
	}, true
}

// parseIgnoreLine builds a rule from one .gitignore line.
func parseIgnoreLine(line string) (ignoreRule, bool) {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}

	negate := strings.HasPrefix(line, "!")
	if negate {
		line = line[1:]
	}

	rule, ok := parsePattern(line)
	if !ok {
		return ignoreRule{}, false
	}
	rule.negate = negate
	return rule, true
}

// loadGitignore reads dir/.gitignore, returning nil when the file is absent.
func loadGitignore(dir string) (*ignoreMatcher, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	m := &ignoreMatcher{}
	for _, line := range strings.Split(string(data), "\n") {
		if rule, ok := parseIgnoreLine(line); ok {
			m.rules = append(m.rules, rule)
		}
	}
	return m, nil
}

// newExcludeMatcher builds a matcher from --exclude patterns.
func newExcludeMatcher(patterns []string) (*ignoreMatcher, error) {
	if len(patterns) == 0 {
		return nil, nil
	}

	m := &ignoreMatcher{}
	for _, pattern := range patterns {
		rule, ok := parsePattern(filepath.ToSlash(pattern))
		if !ok {
			return nil, fmt.Errorf("invalid exclude pattern %q", pattern)
		}
		m.rules = append(m.rules, rule)
	}
	return m, nil
}

// ignored reports whether rel is excluded. rel uses forward slashes and is
// relative to the scanned root.
func (m *ignoreMatcher) ignored(rel string, isDir bool) bool {
	if m == nil || rel == "" {
		return false
	}

	ignored := false
	for _, rule := range m.rules {
		if rule.matches(rel, isDir) {
			ignored = !rule.negate
		}
	}
	return ignored
}

// matches reports whether rel matches this rule.
func (r ignoreRule) matches(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}

	segs := strings.Split(rel, "/")
	if !r.anchored {
		matched, err := path.Match(r.segs[0], segs[len(segs)-1])
		return err == nil && matched
	}
	return matchSegments(r.segs, segs)
}

// matchSegments matches pattern segments against path segments in O(P*S) time,
// where "**" matches zero or more segments and every other segment uses
// path.Match globs.
func matchSegments(pattern, segs []string) bool {
	dp := make([]bool, len(segs)+1)
	dp[0] = true

	for _, p := range pattern {
		if p == "**" {
			for j := 1; j <= len(segs); j++ {
				if dp[j-1] {
					dp[j] = true
				}
			}
			continue
		}
		for j := len(segs); j >= 1; j-- {
			if dp[j-1] {
				matched, err := path.Match(p, segs[j-1])
				dp[j] = err == nil && matched
			} else {
				dp[j] = false
			}
		}
		dp[0] = false
	}

	return dp[len(segs)]
}

// scanConfig carries the filters that apply while walking.
type scanConfig struct {
	maxFiles        int
	noDefaultIgnore bool
	excludes        *ignoreMatcher
	gitignore       *ignoreMatcher
}

// walkCodeFiles collects up to cfg.maxFiles supported source files under dir.
// A maxFiles of 0 means no limit. truncated reports whether additional
// countable files existed past cfg.maxFiles. Unreadable subdirectories are
// recorded in skipped rather than aborting the whole walk.
func walkCodeFiles(dir string, cfg scanConfig) (paths []string, skipped []error, truncated bool, err error) {
	err = filepath.WalkDir(dir, func(full string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if full == dir {
				return walkErr
			}
			skipped = append(skipped, fmt.Errorf("%s: %w", full, walkErr))
			return nil
		}

		rel, relErr := filepath.Rel(dir, full)
		if relErr != nil {
			rel = full
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil // never skip the scan root itself
		}

		if d.IsDir() {
			if !cfg.noDefaultIgnore && isIgnoredDir(d.Name()) {
				return filepath.SkipDir
			}
			if cfg.excludes.ignored(rel, true) {
				return filepath.SkipDir
			}
			if cfg.gitignore.ignored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip symlinks, FIFOs, sockets, and devices so they are neither
		// followed outside the tree nor allowed to block reads.
		if !d.Type().IsRegular() {
			return nil
		}

		if cfg.excludes.ignored(rel, false) || cfg.gitignore.ignored(rel, false) {
			return nil
		}
		if !isCodeFile(strings.ToLower(filepath.Ext(full))) {
			return nil
		}

		if cfg.maxFiles > 0 && len(paths) >= cfg.maxFiles {
			// Stop only once an extra countable file is actually seen, so a
			// directory with exactly maxFiles files is not flagged truncated.
			truncated = true
			return filepath.SkipAll
		}
		paths = append(paths, full)
		return nil
	})

	return paths, skipped, truncated, err
}

// countFiles counts every path concurrently. Unreadable, oversized, or binary
// files are reported as skipped instead of aborting the whole scan.
func countFiles(paths []string) ([]FileInfo, []error) {
	type result struct {
		index int
		info  FileInfo
		err   error
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > len(paths) {
		workers = len(paths)
	}
	if workers < 1 {
		return nil, nil
	}

	jobs := make(chan int)
	results := make(chan result, len(paths))

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				count, err := countEffectiveLines(paths[i])
				results <- result{index: i, info: FileInfo{path: paths[i], lineCount: count}, err: err}
			}
		}()
	}

	go func() {
		for i := range paths {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	counted := make([]result, len(paths))
	ok := make([]bool, len(paths))
	var skipped []error
	for res := range results {
		if res.err != nil {
			skipped = append(skipped, fmt.Errorf("%s: %w", paths[res.index], res.err))
			continue
		}
		counted[res.index] = res
		ok[res.index] = true
	}

	files := make([]FileInfo, 0, len(paths))
	for i, res := range counted {
		if ok[i] {
			files = append(files, res.info)
		}
	}

	// Sort warnings so output is stable regardless of goroutine scheduling.
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Error() < skipped[j].Error() })

	return files, skipped
}

// traverseDirectory walks dir and returns one FileInfo per counted code file,
// the entries that had to be skipped, and whether maxFiles truncated the scan.
func traverseDirectory(dir string, cfg scanConfig) ([]FileInfo, []error, bool, error) {
	paths, walkSkipped, truncated, err := walkCodeFiles(dir, cfg)
	if err != nil {
		return nil, nil, false, err
	}
	files, countSkipped := countFiles(paths)
	skipped := append(walkSkipped, countSkipped...)
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Error() < skipped[j].Error() })
	return files, skipped, truncated, nil
}

// sortFiles orders files by effective line count, largest first, with the path
// as a stable tie-breaker.
func sortFiles(files []FileInfo) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].lineCount != files[j].lineCount {
			return files[i].lineCount > files[j].lineCount
		}
		return files[i].path < files[j].path
	})
}

// displayWidth returns the number of terminal columns s occupies, counting
// East Asian wide runes as two columns.
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		width += runeWidth(r)
	}
	return width
}

// runeWidth returns the terminal width of a single rune.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Cf, r):
		return 0 // combining marks and format control take no cell
	case isWideRune(r):
		return 2
	}
	return 1
}

// isWideRune reports whether r occupies two terminal columns.
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E,   // CJK radicals and punctuation
		r >= 0x3041 && r <= 0x33FF,   // kana, CJK compatibility
		r >= 0x3400 && r <= 0x4DBF,   // CJK extension A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK unified ideographs
		r >= 0xA000 && r <= 0xA4CF,   // Yi
		r >= 0xA960 && r <= 0xA97F,   // Hangul Jamo extended A
		r >= 0xAC00 && r <= 0xD7A3,   // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF,   // CJK compatibility ideographs
		r >= 0xFE10 && r <= 0xFE19,   // vertical forms
		r >= 0xFE30 && r <= 0xFE6F,   // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60,   // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,   // fullwidth signs
		r >= 0x1F300 && r <= 0x1F64F, // emoji
		r >= 0x1F900 && r <= 0x1F9FF, // supplemental emoji
		r >= 0x20000 && r <= 0x3FFFD: // CJK extensions B and beyond
		return true
	}
	return false
}

// padRight appends spaces until s occupies width terminal columns.
func padRight(s string, width int) string {
	if gap := width - displayWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// iconKey maps an extension to an icon family.
func iconKey(ext string) string {
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "py"
	case ".js", ".ts", ".jsx", ".tsx", ".vue":
		return "js"
	case ".cpp", ".c", ".h":
		return "c"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	default:
		return "other"
	}
}

// nerdIcons and asciiIcons map an icon family to a two-column icon.
var (
	nerdIcons = map[string]string{
		"go":    "\ue724",
		"py":    "\ue235",
		"js":    "\ue781",
		"c":     "\ue61d",
		"html":  "\ue60e",
		"css":   "\ue614",
		"other": "\ue612",
	}

	asciiIcons = map[string]string{
		"go": "go", "py": "py", "js": "js", "c": "c ",
		"html": "ht", "css": "cs", "other": "  ",
	}
)

// fileIcon returns a two-column icon for ext, using Nerd Font glyphs unless
// asciiFallback is set.
func fileIcon(ext string, asciiFallback bool) string {
	key := iconKey(ext)
	if asciiFallback {
		return asciiIcons[key]
	}
	return " " + nerdIcons[key]
}

// options holds parsed command-line settings.
type options struct {
	target          string
	maxFiles        int
	excludes        []string
	json            bool
	ascii           bool
	noColor         bool
	noGitignore     bool
	noDefaultIgnore bool
	showHelp        bool
	showVersion     bool
}

const usageText = `codecnt - count effective lines of code

Usage:
  codecnt [flags] [path]

The path defaults to "." and may be a directory (scanned recursively) or a
single code file. Blank lines and comment-only lines are not counted.

Flags:
  --max-files N       stop after N files (default 200, 0 = no limit)
  --exclude PATTERN   skip paths matching PATTERN (repeatable or comma-separated)
  --json              print a machine-readable report
  --ascii             use ASCII icons instead of Nerd Font glyphs
  --no-color          disable colored output
  --no-gitignore      do not honor .gitignore in the scanned directory
  --no-default-ignore do not skip built-in ignored directories
  -h, --help          show this help
  --version           show the version

Examples:
  codecnt
  codecnt ./src --max-files 500
  codecnt . --exclude "vendor,tmp" --exclude "*.min.js"
  codecnt --json . > stats.json
`

// parseArgs parses flags in any position, so "codecnt . --json" works.
func parseArgs(args []string) (options, error) {
	opt := options{target: ".", maxFiles: defaultMaxFiles}
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case arg == "-h" || arg == "--help":
			opt.showHelp = true
		case arg == "--version":
			opt.showVersion = true
		case arg == "--json":
			opt.json = true
		case arg == "--ascii":
			opt.ascii = true
		case arg == "--no-color":
			opt.noColor = true
		case arg == "--no-gitignore":
			opt.noGitignore = true
		case arg == "--no-default-ignore":
			opt.noDefaultIgnore = true
		case arg == "--max-files" || strings.HasPrefix(arg, "--max-files="):
			value, next, err := flagValue(arg, args, i)
			if err != nil {
				return opt, err
			}
			n, convErr := strconv.Atoi(value)
			if convErr != nil || n < 0 {
				return opt, fmt.Errorf("--max-files needs a non-negative number, got %q", value)
			}
			opt.maxFiles = n
			i = next
		case arg == "--exclude" || strings.HasPrefix(arg, "--exclude="):
			value, next, err := flagValue(arg, args, i)
			if err != nil {
				return opt, err
			}
			for _, pattern := range strings.Split(value, ",") {
				if pattern = strings.TrimSpace(pattern); pattern != "" {
					opt.excludes = append(opt.excludes, pattern)
				}
			}
			i = next
		case strings.HasPrefix(arg, "-") && arg != "-":
			return opt, fmt.Errorf("unknown flag %q", arg)
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) > 1 {
		return opt, fmt.Errorf("expected at most one path, got %d", len(positional))
	}
	if len(positional) == 1 {
		opt.target = positional[0]
	}
	return opt, nil
}

// flagValue returns the value of a flag written as "--flag=value" or
// "--flag value", plus the index of the last consumed argument.
func flagValue(arg string, args []string, i int) (string, int, error) {
	if eq := strings.IndexByte(arg, '='); eq >= 0 {
		return arg[eq+1:], i, nil
	}
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("flag %s needs a value", arg)
	}
	return args[i+1], i + 1, nil
}

// jsonFile is one entry in a JSON report.
type jsonFile struct {
	Path  string `json:"path"`
	Lines int    `json:"lines"`
}

// jsonReport is the machine-readable output of --json.
type jsonReport struct {
	Version    string     `json:"version"`
	Root       string     `json:"root"`
	TotalFiles int        `json:"totalFiles"`
	TotalLines int        `json:"totalLines"`
	Truncated  bool       `json:"truncated"`
	Skipped    []string   `json:"skipped"`
	Files      []jsonFile `json:"files"`
}

// buildReport assembles the JSON report from scanned files.
func buildReport(baseDir string, files []FileInfo, skipped []error, truncated bool) jsonReport {
	report := jsonReport{
		Version:   version,
		Root:      baseDir,
		Truncated: truncated,
		Skipped:   make([]string, 0, len(skipped)),
		Files:     make([]jsonFile, 0, len(files)),
	}

	for _, err := range skipped {
		report.Skipped = append(report.Skipped, err.Error())
	}

	for _, f := range files {
		rel, err := filepath.Rel(baseDir, f.path)
		if err != nil {
			rel = f.path
		}
		report.Files = append(report.Files, jsonFile{Path: filepath.ToSlash(rel), Lines: f.lineCount})
		report.TotalFiles++
		report.TotalLines += f.lineCount
	}

	return report
}

// printJSON writes the report to stdout.
func printJSON(report jsonReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// errPrintf writes a red error message to stderr.
func errPrintf(format string, args ...any) {
	color.New(color.FgRed, color.Bold).Fprintf(os.Stderr, format, args...)
}

// warnPrintf writes a yellow warning to stderr.
func warnPrintf(format string, args ...any) {
	color.New(color.FgYellow).Fprintf(os.Stderr, format, args...)
}

// reportSkipped prints per-file skip warnings, capped to avoid flooding.
func reportSkipped(skipped []error) {
	for i, err := range skipped {
		if i == maxSkipWarnings {
			warnPrintf("  ⚠ %d more files skipped\n", len(skipped)-maxSkipWarnings)
			break
		}
		warnPrintf("  ⚠ skipped %v\n", err)
	}
}

func main() {
	opt, err := parseArgs(os.Args[1:])
	if err != nil {
		errPrintf("✗ %v\n\n", err)
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}

	if opt.showHelp {
		fmt.Print(usageText)
		return
	}
	if opt.showVersion {
		fmt.Printf("codecnt %s\n", version)
		return
	}
	if opt.noColor {
		color.NoColor = true
	}

	info, err := os.Stat(opt.target)
	if err != nil {
		errPrintf("✗ Error: cannot access '%s': %v\n", opt.target, err)
		os.Exit(1)
	}

	excludes, err := newExcludeMatcher(opt.excludes)
	if err != nil {
		errPrintf("✗ Error: %v\n", err)
		os.Exit(2)
	}

	cfg := scanConfig{
		maxFiles:        opt.maxFiles,
		noDefaultIgnore: opt.noDefaultIgnore,
		excludes:        excludes,
	}
	if info.IsDir() && !opt.noGitignore {
		gitignore, giErr := loadGitignore(opt.target)
		if giErr != nil {
			warnPrintf("  ⚠ ignoring .gitignore: %v\n", giErr)
		}
		cfg.gitignore = gitignore
	}

	baseDir := opt.target
	var files []FileInfo
	var skipped []error
	var truncated bool

	if info.IsDir() {
		files, skipped, truncated, err = traverseDirectory(opt.target, cfg)
		if err != nil {
			errPrintf("✗ Error traversing directory: %v\n", err)
			os.Exit(1)
		}
	} else {
		// A single file is accepted explicitly, so passing a file path
		// works directly without walking a parent directory.
		if !isCodeFile(strings.ToLower(filepath.Ext(opt.target))) {
			errPrintf("✗ Error: '%s' is not a supported code file\n", opt.target)
			os.Exit(1)
		}
		count, countErr := countEffectiveLines(opt.target)
		if countErr != nil {
			errPrintf("✗ Error: %s: %v\n", opt.target, countErr)
			os.Exit(1)
		}
		files = []FileInfo{{path: opt.target, lineCount: count}}
		baseDir = filepath.Dir(opt.target)
	}

	reportSkipped(skipped)

	sortFiles(files)

	if opt.json {
		if err := printJSON(buildReport(baseDir, files, skipped, truncated)); err != nil {
			errPrintf("✗ Error writing JSON: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if len(files) == 0 {
		if len(skipped) > 0 {
			color.Yellow("⚠ No countable code files found (%d skipped).", len(skipped))
			return
		}
		color.Yellow("⚠ No code files found.")
		return
	}

	totalLines := 0
	for _, f := range files {
		totalLines += f.lineCount
	}

	header := color.New(color.Bold, color.BgHiBlue, color.FgWhite).SprintFunc()
	title := color.New(color.Bold, color.FgCyan).SprintFunc()
	highlight := color.New(color.Bold, color.FgGreen).SprintFunc()
	fileInfo := color.New(color.FgHiWhite).SprintFunc()
	border := color.New(color.FgBlack).SprintFunc()
	dim := color.New(color.FgBlack).SprintFunc()
	normal := color.New(color.FgWhite).SprintFunc()

	fmt.Println()
	fmt.Printf("  %s Code Statistics\n", header("  CODE"))
	fmt.Printf("  %s\n", border(strings.Repeat("─", 50)))

	for i, f := range files {
		if i >= displayLimit {
			break
		}
		rel, err := filepath.Rel(baseDir, f.path)
		if err != nil {
			rel = f.path
		}
		icon := fileIcon(strings.ToLower(filepath.Ext(f.path)), opt.ascii)
		// Pad before coloring: ANSI escapes would otherwise count as width.
		fmt.Printf(" %s %s %s\n", icon, fileInfo(padRight(rel, pathColumnWidth)), highlight(fmt.Sprintf("%4d", f.lineCount)))
	}

	if len(files) > displayLimit {
		fmt.Printf("  %s %d more files\n", dim("⋯"), len(files)-displayLimit)
	}

	if truncated {
		color.Yellow("\n  ⚠ Stopped at the %d-file limit; totals cover only the first %d files scanned.", opt.maxFiles, opt.maxFiles)
	}

	fmt.Printf("  %s\n", border(strings.Repeat("─", 50)))
	fmt.Printf("  %s %s  %s %s %s\n", normal("Total"), title(fmt.Sprintf("%d", len(files))), "files,",
		highlight(fmt.Sprintf("%d", totalLines)), "effective lines")
	fmt.Println()
}
