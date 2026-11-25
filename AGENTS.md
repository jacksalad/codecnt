# AGENTS.md - CodeNcnt Project Guidelines

This document provides guidelines for agentic coding agents working on the codecnt project.

## Project Overview

Codecnt is a CLI tool written in Go that counts effective lines of code across multiple programming languages. It recursively traverses directories and ignores comments, empty lines, and non-code files.

## Build Commands

```bash
# Build the application
go build -o codecnt.exe main.go

# Build for all platforms
GOOS=windows GOARCH=amd64 go build -o codecnt.exe main.go
GOOS=linux GOARCH=amd64 go build -o codecnt main.go
GOOS=darwin GOARCH=amd64 go build -o codecnt main.go

# Run the application
go run main.go
go run main.go <directory_path>
```

## Linting and Formatting

```bash
# Format code (required before committing)
gofmt -w main.go

# Run go vet
go vet ./...

# Check for unused imports
go run golang.org/x/tools/cmd/goimports@latest -w main.go

# Install and run static analysis
go install honnef.co/go/tools/cmd/staticcheck@latest
staticcheck ./...
```

## Testing

```bash
# Run all tests
go test -v ./...

# Run a single test function
go test -v -run TestFunctionName ./...

# Run tests with coverage
go test -v -cover ./...

# Run tests matching a pattern
go test -v -run "Test.*File" ./...
```

## Code Style Guidelines

### Imports

- Use standard library imports first, then third-party imports
- Group imports: stdlib, third-party, local (if any)
- Use the `github.com/fatih/color` package for colored terminal output (already imported)

```go
import (
    "fmt"
    "os"
    "path/filepath"
    "strings"

    "github.com/fatih/color"
)
```

### Naming Conventions

- **Packages**: lowercase, single word (e.g., `main`)
- **Variables**: camelCase for local variables, PascalCase for exported identifiers
- **Constants**: camelCase for unexported, PascalCase for exported
- **Functions**: camelCase for unexported, PascalCase for exported
- **Structs**: PascalCase for both exported and unexported
- **Files**: lowercase with underscores for multi-word names

### Types

- Use explicit types where beneficial for clarity
- Avoid `var` declarations when type inference is clear
- Use meaningful type names (avoid generic names like `Info` unless specific)

```go
type FileInfo struct {
    path      string
    lineCount int
}
```

### Error Handling

- Return errors from functions; check them in callers
- Use `os.Exit(1)` for fatal errors in `main()`
- Use `color.Red()` for error messages to stderr
- Always handle errors explicitly; do not use `_` for ignored errors

```go
if err != nil {
    color.Red("✗ Error: %v", err)
    os.Exit(1)
}
```

### Formatting

- Use tabs for indentation (Go's default)
- Keep lines under 120 characters when possible
- Use consistent spacing around operators
- Group related functionality with blank lines
- Put closing braces on new lines for control structures

### Commenting

- Comment exported functions, types, and constants
- Use `//` for single-line comments, `/* */` for multi-line
- Comments should explain "why", not "what"
- Use godoc-style comments for exported APIs

### File Organization

- Main entry point in `main.go`
- Keep related functions grouped together
- Put helper functions near their usage
- Variable declarations at the top of files (Go style)

### Control Structures

- Prefer early returns over nested conditionals
- Use `switch` with explicit cases over long if-else chains
- Keep loops simple; extract complex loop bodies into functions

### String Handling

- Use `strings` package for string operations
- Avoid string concatenation in loops (use `strings.Builder`)
- Handle path separators with `filepath` package for cross-platform compatibility

### Terminal Output

- Use `github.com/fatih/color` for colored output
- Use emoji prefixes for status indicators (✓, ✗, ⚠, etc.)
- Use consistent formatting for tabular output
- Print a blank line before and after major output sections

## Adding New Language Support

All language knowledge lives in the `languages` map in `main.go`, keyed by file
extension. It is the single source of truth: an extension not in the map is not
counted, and every entry defines how comments and strings are parsed.

To add support for a new language:

1. Add an entry to the `languages` map:
   - `lineComments`: markers that comment out the rest of a line (`//`, `#`, `--`)
   - `blockComments`: start/end delimiter pairs for multi-line comments
   - `quotes`: string delimiters whose contents must not be parsed as code
   - `multiline`: delimiters that may span lines (Go backticks, JS templates)
2. Reuse an existing spec when the syntax matches (`cLike`, `hashLike`, `htmlLike`)
3. Add an icon mapping in `iconKey` (and both icon maps if a new family is needed)
4. Add table-driven cases to `TestCountEffectiveLines` in `main_test.go`

Do not add extension lists elsewhere; `isCodeFile` derives from `languages`.

## Architecture Notes

- **parseArgs()**: Position-agnostic flag parser (`codecnt . --json` works);
  returns an `options` struct. Bad input exits with status 2.
- **walkCodeFiles()**: Walks the tree, applies `ignoredDirs`, `--exclude` rules
  and root `.gitignore`, and stops at the configured cap via `filepath.SkipAll`
- **countFiles()**: Counts paths concurrently with a bounded worker pool
  (`runtime.GOMAXPROCS`) and preserves a deterministic result order
- **traverseDirectory()**: Glues walking and counting together and returns
  counted files plus the files that had to be skipped
- **countEffectiveLines()**: Reads and decodes the file (UTF-8/UTF-16), then
  asks `lineHasCode` per line
- **lineHasCode()**: Character-level scanner tracking block-comment and string
  state, so markers inside strings, trailing block comments, code after a
  closing marker, and multi-line raw strings are handled correctly
- **ignoreMatcher / parseIgnoreLine()**: Gitignore-style rule list with
  negation, anchoring, directory-only rules, and `**`; also drives `--exclude`
- **sortFiles()**: Orders by effective line count, largest first, path ascending
  on ties; the top 20 are displayed
- **displayWidth() / padRight()**: Width-aware alignment so CJK paths and
  colored output still line up (padding happens before colorizing)
- **fileIcon()**: Nerd Font glyphs with an ASCII fallback (`--ascii`)
- **buildReport() / printJSON()**: Machine-readable output for `--json`
- **main()**: Entry point. Accepts a directory (recursive) or a single code file,
  writes diagnostics to stderr, results to stdout

### Failure policy

Per-file problems never abort a scan. Files larger than `maxFileBytes`, binary
files (NUL bytes), and unreadable files are collected as skip warnings and
printed to stderr (first `maxSkipWarnings`, then a count). They also appear in
the JSON `skipped` array. Only path-level failures (missing path, walk error)
and bad flags exit non-zero.

## Current Test Coverage

`main_test.go` covers the counter, traversal filters, CLI parsing, and output:

- Comment detection for Go, Python, CSS, HTML, Vue, shell, and Lua
- Markers inside string literals, escaped quotes, CRLF input
- Multi-line Go raw strings and JS template literals
- UTF-16LE/BE with BOM; UTF-16 without BOM rejected as binary
- Exact file-cap enforcement, unlimited (`0`) scans, deterministic sorting
- Ignored directories, non-code extensions, binary/oversized skips
- `.gitignore` semantics (negation, anchoring, dir-only, `**`) and `--exclude`
- Flag parsing in any position, unknown flags, bad values, `--`
- Width-aware padding for CJK and box-drawing characters
- JSON report shape and totals

Known limitations:

- Only the first `--max-files` files scanned are counted; totals are not
  repository-wide and the subset depends on traversal order
- Nested `.gitignore` files are not read, only the one at the scanned root
- Escaped `.gitignore` characters (`\#`) and trailing-space escapes are ignored
- Ordinary string literals are assumed to end at the line break

## Dependencies

- `github.com/fatih/color v1.18.0` - Colored terminal output
- Go 1.25.4 or higher required

## Common Tasks

**Building a release:**
```bash
go build -ldflags="-s -w" -o codecnt.exe main.go
```

**Running on a specific directory:**
```bash
go run main.go /path/to/project
codecnt.exe C:\projects\myapp --max-files 500 --exclude "vendor,tmp"
codecnt.exe --json . > stats.json
```

**Adding a new file extension:**
Add an entry to the `languages` map in `main.go`; see "Adding New Language
Support" above. Do not reintroduce per-extension lists.
