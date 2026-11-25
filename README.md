# codecnt

Count effective lines of code across a repository. Blank lines and
comment-only lines are ignored, and comments are stripped with a real
character-level scanner rather than naive line prefix checks.

```
  CODE Code Statistics
  ──────────────────────────────────────────────────
  main.go                                          499
  main_test.go                                     330
  ──────────────────────────────────────────────────
  Total 2  files, 829 effective lines
```

## Install

Requires Go 1.25.4 or newer.

```bash
go install github.com/jacksalad/codecnt@latest
```

Or build from source:

```bash
go build -ldflags="-s -w" -o codecnt.exe main.go
```

## Usage

```
codecnt [flags] [path]
```

`path` defaults to `.` and may be a directory (scanned recursively) or a single
code file.

| Flag | Description |
| --- | --- |
| `--max-files N` | Stop after N files (default `200`, `0` = no limit) |
| `--exclude PATTERN` | Skip paths matching PATTERN (repeatable or comma-separated) |
| `--json` | Print a machine-readable report |
| `--ascii` | Use ASCII icons instead of Nerd Font glyphs |
| `--no-color` | Disable colored output |
| `--no-gitignore` | Do not honor `.gitignore` in the scanned directory |
| `--no-default-ignore` | Do not skip built-in ignored directories |
| `-h`, `--help` | Show help |
| `--version` | Show the version |

Flags are accepted before or after the path.

### Examples

```bash
# Count the current directory
codecnt

# Count a subdirectory, allowing more files
codecnt ./src --max-files 500

# Skip directories and generated bundles
codecnt . --exclude "vendor,tmp" --exclude "*.min.js"

# Machine-readable output
codecnt --json . > stats.json
```

### Exclude patterns

`--exclude` accepts gitignore-style patterns:

- `vendor` — any file or directory named `vendor` at any depth
- `build/` — directories named `build` only
- `/dist` — only the top-level `dist`
- `*.min.js` — any minified bundle
- `src/gen/**` — everything under `src/gen`

Patterns with a `/` are anchored to the scanned root, mirroring `.gitignore`
semantics. Later patterns win, and `.gitignore` rules also apply.

## JSON output

```json
{
  "version": "1.0.0",
  "root": "/repo",
  "totalFiles": 2,
  "totalLines": 829,
  "truncated": false,
  "skipped": [],
  "files": [
    { "path": "main.go", "lines": 499 },
    { "path": "main_test.go", "lines": 330 }
  ]
}
```

`truncated` is `true` when the `--max-files` cap stopped the scan. `skipped`
lists files excluded from the totals with the reason.

## How lines are counted

Each file is scanned character by character while tracking three states:

1. **Normal code** — any non-space character outside a comment marks the line
   as effective.
2. **Block comment** — `/* ... */`, `<!-- ... -->`, or Python triple quotes.
   Opened and closed on the same line, or spanning lines, both work.
3. **String literal** — comment markers inside strings are not treated as
   comments. Go backticks and JavaScript template literals may span lines.

This handles the cases that naive counters get wrong:

| Input | Effective |
| --- | --- |
| `x := 1 /* start` (comment continues) | 1, not 0 |
| `/* doc */ x := 1` | 1, not 0 |
| `a := "/*"; b := "*/"` | 1, not 0 |
| `*/ x := 1` | 1, not 0 |
| Python `"""docstring"""` | 0 (treated as a comment) |
| CSS `// not valid css` | 1 (CSS has no line comments) |

UTF-8 (with or without BOM), UTF-16LE, and UTF-16BE (with BOM) are supported.

## Supported languages

Go, C, C++, headers, Java, Rust, Swift, Kotlin, Scala, Python, Ruby, PHP, Lua,
JavaScript, TypeScript, JSX/TSX, Vue, HTML, CSS, and shell scripts.

Adding a language means adding one entry to the `languages` map in `main.go`;
see [`AGENTS.md`](AGENTS.md).

## Skipped files

Files are skipped (with a warning on stderr) when they are:

- larger than 1 MiB, which usually means generated or bundled output
- binary, detected by NUL bytes in the first 8 KiB
- unreadable (permissions, locks)

Directories such as `node_modules`, `vendor`, `dist`, `build`, `target`,
`obj`, `bin`, and `Pods` are skipped by default (disable with `--no-default-ignore`).

## Development

```bash
gofmt -l .        # must print nothing
go vet ./...
go test ./...
go test -race ./...
```

## Limitations

- Only the first `--max-files` files are scanned; totals are not
  repository-wide. `--json` reports this as `truncated`.
- Nested `.gitignore` files are not read; only the one at the scanned root.
- Escaped characters in `.gitignore` (for example `\#`) and trailing-space
  escapes are not interpreted.
- Ordinary strings are assumed to end at the line break; only raw delimiters
  span lines.
- Nerd Font icons require a patched font. Use `--ascii` otherwise.

## License

MIT. See [`LICENSE`](LICENSE).
