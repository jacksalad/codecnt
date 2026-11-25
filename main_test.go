package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

// writeSample creates a temp file with the given extension and content.
func writeSample(t *testing.T, ext, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample"+ext)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	return path
}

// writeFile creates a file with parents under dir.
func writeFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return path
}

func TestCountEffectiveLines(t *testing.T) {
	cases := []struct {
		name string
		ext  string
		src  string
		want int
	}{
		{
			name: "plain code",
			ext:  ".go",
			src:  "package main\n\nfunc main() {}\n",
			want: 2,
		},
		{
			name: "line comment only",
			ext:  ".go",
			src:  "// a\n// b\n",
			want: 0,
		},
		{
			name: "trailing line comment still counts as code",
			ext:  ".go",
			src:  "x := 1 // trailing\n",
			want: 1,
		},
		{
			name: "block comment opened at end of a code line",
			ext:  ".go",
			src:  "x := 1 /* start\ninside\ninside\n*/\ny := 2\n",
			want: 2,
		},
		{
			name: "single line block comment before code",
			ext:  ".go",
			src:  "/* doc */ x := 1\n",
			want: 1,
		},
		{
			name: "code after closing marker on its own line",
			ext:  ".go",
			src:  "/*\ncomment\n*/ x := 1\n",
			want: 1,
		},
		{
			name: "comment markers inside a string literal",
			ext:  ".go",
			src:  "a := \"/*\"; b := \"*/\"\n",
			want: 1,
		},
		{
			name: "escaped quote keeps the string open",
			ext:  ".go",
			src:  "s := \"a\\\"b\" // c\n",
			want: 1,
		},
		{
			name: "crlf line endings",
			ext:  ".go",
			src:  "package main\r\n\r\n// c\r\nfunc f() {}\r\n",
			want: 2,
		},
		{
			name: "python docstring is a comment",
			ext:  ".py",
			src:  "def f():\n    \"\"\"doc\"\"\"\n    return 1\n",
			want: 2,
		},
		{
			name: "python multi line docstring",
			ext:  ".py",
			src:  "def f():\n    \"\"\"line1\n    line2\n    \"\"\"\n    return 1\n",
			want: 2,
		},
		{
			name: "python hash comment",
			ext:  ".py",
			src:  "# c\nx = 1  # trailing\n",
			want: 1,
		},
		{
			name: "css has no line comments",
			ext:  ".css",
			src:  "// not valid css\na{color:red}\n",
			want: 2,
		},
		{
			name: "css block comment",
			ext:  ".css",
			src:  "/* c */\na{color:red}\n",
			want: 1,
		},
		{
			name: "html comment only",
			ext:  ".html",
			src:  "<!-- c -->\n",
			want: 0,
		},
		{
			name: "html keeps inline content around a comment",
			ext:  ".html",
			src:  "<div><!-- c --></div>\n",
			want: 1,
		},
		{
			name: "html multiline comment",
			ext:  ".html",
			src:  "<!--\na\nb\n-->\n<p>hi</p>\n",
			want: 1,
		},
		{
			name: "vue strips both comment styles",
			ext:  ".vue",
			src:  "<!-- c -->\n<script>\n// c\nconst a = 1\n</script>\n",
			want: 3,
		},
		{
			name: "shell shebang is a comment",
			ext:  ".sh",
			src:  "#!/bin/sh\necho hi\n",
			want: 1,
		},
		{
			name: "lua block comment",
			ext:  ".lua",
			src:  "--[[\nc\n]]\nprint(1)\n",
			want: 1,
		},
		{
			name: "lua line comment",
			ext:  ".lua",
			src:  "-- c\nprint(1)\n",
			want: 1,
		},
		{
			name: "unterminated block comment swallows rest",
			ext:  ".go",
			src:  "x := 1\n/*\ny := 2\n",
			want: 1,
		},
		{
			name: "go raw string spans lines",
			ext:  ".go",
			src:  "var s = `\n// not a comment\npackage fake\n`\n",
			want: 4,
		},
		{
			name: "go raw string trailing backslash does not escape backtick",
			ext:  ".go",
			src:  "var s = `a\\` // trailing\n// real comment\n",
			want: 1,
		},
		{
			name: "js template literal spans lines",
			ext:  ".js",
			src:  "const s = `\n// not a comment\n`;\n",
			want: 3,
		},
		{
			name: "js template literal escaped backtick does not close string",
			ext:  ".js",
			src:  "const s = `hello \\` world`; // trailing\n// real comment\n\nconst x = 1;\n",
			want: 2,
		},
		{
			name: "ts template literal escaped backtick",
			ext:  ".ts",
			src:  "const s: string = `a \\` b`;\n// comment\n",
			want: 1,
		},
		{
			name: "vue template literal spans lines",
			ext:  ".vue",
			src:  "<script>\nconst s = `\n// inside template\n`;\n// real comment\n</script>\n",
			want: 5,
		},
		{
			name: "rust lifetimes do not swallow block comments",
			ext:  ".rs",
			src:  "fn foo<'a>(x: &'a str) { /*\ncomment 1\ncomment 2\n*/\n}\n",
			want: 2,
		},
		{
			name: "rust char literal containing double quote",
			ext:  ".rs",
			src:  "let q = '\"'; /*\ncomment\n*/\nlet e = '\\''; // trailing\n",
			want: 2,
		},
		{
			name: "html apostrophe in prose does not swallow comment",
			ext:  ".html",
			src:  "<p>Don't forget <!--\ncomment 1\ncomment 2\n--></p>\n",
			want: 2,
		},
		{
			name: "ruby block comment at column zero",
			ext:  ".rb",
			src:  "=begin doc\nline1\n  =end not closing\n=end\nputs 1\n",
			want: 1,
		},
		{
			name: "ruby block comment with crlf",
			ext:  ".rb",
			src:  "=begin\r\nignored\r\n=end\r\nputs 1\r\n",
			want: 1,
		},
		{
			name: "ruby inline equals begin is not a block comment",
			ext:  ".rb",
			src:  "x =begin_time\nputs x\n",
			want: 2,
		},
		{
			name: "ruby indented equals begin is not a block comment",
			ext:  ".rb",
			src:  "  =begin\n  puts 1\n",
			want: 2,
		},
		{
			name: "lua level 1 long block comment",
			ext:  ".lua",
			src:  "--[=[\nc\n]=]\nprint(1)\n",
			want: 1,
		},
		{
			name: "ordinary string does not span lines",
			ext:  ".go",
			src:  "x := \"abc\n// real comment\n",
			want: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := countEffectiveLines(writeSample(t, c.ext, c.src))
			if err != nil {
				t.Fatalf("countEffectiveLines: %v", err)
			}
			if got != c.want {
				t.Fatalf("effective lines = %d, want %d", got, c.want)
			}
		})
	}
}

func TestCountEffectiveLinesMissingFile(t *testing.T) {
	if _, err := countEffectiveLines(filepath.Join(t.TempDir(), "missing.go")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestCountEffectiveLinesRejectsBinary(t *testing.T) {
	path := writeSample(t, ".go", "package x\n\x00\x01\x02\n")
	_, err := countEffectiveLines(path)
	if !errors.Is(err, errBinaryFile) {
		t.Fatalf("err = %v, want errBinaryFile", err)
	}
}

func TestCountEffectiveLinesRejectsTooLarge(t *testing.T) {
	body := strings.Repeat("x := 1\n", maxFileBytes/7+1)
	if len(body) <= maxFileBytes {
		t.Fatalf("test body is only %d bytes", len(body))
	}
	path := writeSample(t, ".go", body)
	_, err := countEffectiveLines(path)
	if !errors.Is(err, errFileTooLarge) {
		t.Fatalf("err = %v, want errFileTooLarge", err)
	}
}

func TestCountEffectiveLinesUTF16(t *testing.T) {
	src := "package x\n\n// c\nvar a = 1\n"
	units := utf16.Encode([]rune(src))

	le := []byte{0xFF, 0xFE}
	be := []byte{0xFE, 0xFF}
	for _, u := range units {
		le = append(le, byte(u), byte(u>>8))
		be = append(be, byte(u>>8), byte(u))
	}

	dir := t.TempDir()
	lePath := filepath.Join(dir, "le.go")
	bePath := filepath.Join(dir, "be.go")
	if err := os.WriteFile(lePath, le, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bePath, be, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{lePath, bePath} {
		got, err := countEffectiveLines(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if got != 2 {
			t.Fatalf("%s: effective lines = %d, want 2", path, got)
		}
	}
}

func TestCountEffectiveLinesUTF16WithoutBOMIsBinary(t *testing.T) {
	units := utf16.Encode([]rune("package x\n"))
	raw := make([]byte, 0, len(units)*2)
	for _, u := range units {
		raw = append(raw, byte(u), byte(u>>8))
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nobom.go")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := countEffectiveLines(path); !errors.Is(err, errBinaryFile) {
		t.Fatalf("err = %v, want errBinaryFile", err)
	}
}

func TestCountEffectiveLinesRejectsFakeUTF16AndUTF32(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{
			name: "utf32le bom",
			data: []byte{0xFF, 0xFE, 0x00, 0x00, 'a', 0x00, 0x00, 0x00},
		},
		{
			name: "odd length after utf16le bom",
			data: []byte{0xFF, 0xFE, 'a'},
		},
		{
			name: "utf16le with embedded nul code unit",
			data: []byte{0xFF, 0xFE, 'a', 0x00, 0x00, 0x00, 'b', 0x00},
		},
		{
			name: "utf16be with embedded nul code unit",
			data: []byte{0xFE, 0xFF, 0x00, 'a', 0x00, 0x00, 0x00, 'b'},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.go")
			if err := os.WriteFile(path, c.data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := countEffectiveLines(path); !errors.Is(err, errBinaryFile) {
				t.Fatalf("err = %v, want errBinaryFile", err)
			}
		})
	}
}

func TestCountEffectiveLinesRejectsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fake.go")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := countEffectiveLines(dir); !errors.Is(err, errNonRegularFile) {
		t.Fatalf("err = %v, want errNonRegularFile", err)
	}
}

func TestIsCodeFile(t *testing.T) {
	cases := map[string]bool{
		".go": true, ".py": true, ".html": true, ".css": true,
		".md": false, ".txt": false, ".exe": false, "": false,
	}
	for ext, want := range cases {
		if got := isCodeFile(ext); got != want {
			t.Errorf("isCodeFile(%q) = %v, want %v", ext, got, want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    options
		wantErr bool
	}{
		{
			name: "defaults",
			args: nil,
			want: options{target: ".", maxFiles: defaultMaxFiles},
		},
		{
			name: "positional path",
			args: []string{"src"},
			want: options{target: "src", maxFiles: defaultMaxFiles},
		},
		{
			name: "flag after positional",
			args: []string{"src", "--json"},
			want: options{target: "src", maxFiles: defaultMaxFiles, json: true},
		},
		{
			name: "max files with space",
			args: []string{"--max-files", "50", "src"},
			want: options{target: "src", maxFiles: 50},
		},
		{
			name: "max files with equals and zero",
			args: []string{"--max-files=0"},
			want: options{target: ".", maxFiles: 0},
		},
		{
			name: "comma separated excludes",
			args: []string{"--exclude", "vendor,tmp", "--exclude=*.min.js"},
			want: options{target: ".", maxFiles: defaultMaxFiles, excludes: []string{"vendor", "tmp", "*.min.js"}},
		},
		{
			name: "toggles",
			args: []string{"--ascii", "--no-color", "--no-gitignore", "--no-default-ignore", "--json"},
			want: options{target: ".", maxFiles: defaultMaxFiles, json: true, ascii: true, noColor: true, noGitignore: true, noDefaultIgnore: true},
		},
		{
			name: "double dash ends flags",
			args: []string{"--", "--json"},
			want: options{target: "--json", maxFiles: defaultMaxFiles},
		},
		{
			name:    "unknown flag",
			args:    []string{"--nope"},
			wantErr: true,
		},
		{
			name:    "missing value",
			args:    []string{"--max-files"},
			wantErr: true,
		},
		{
			name:    "negative max files",
			args:    []string{"--max-files", "-1"},
			wantErr: true,
		},
		{
			name:    "non numeric max files",
			args:    []string{"--max-files", "many"},
			wantErr: true,
		},
		{
			name:    "two paths",
			args:    []string{"a", "b"},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseArgs(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got.excludes) != len(c.want.excludes) {
				t.Fatalf("excludes = %v, want %v", got.excludes, c.want.excludes)
			}
			for i := range got.excludes {
				if got.excludes[i] != c.want.excludes[i] {
					t.Fatalf("excludes = %v, want %v", got.excludes, c.want.excludes)
				}
			}
			got.excludes, c.want.excludes = nil, nil
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("options = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestGitignoreMatcher(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".gitignore", strings.Join([]string{
		"# comment",
		"",
		"*.log",
		"!keep.log",
		"build/",
		"/root.txt",
		"docs/**/tmp",
		"coverage/",
	}, "\n"))

	m, err := loadGitignore(dir)
	if err != nil {
		t.Fatalf("loadGitignore: %v", err)
	}
	if m == nil {
		t.Fatal("matcher is nil")
	}

	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"a.log", false, true},
		{"sub/deep/a.log", false, true},
		{"keep.log", false, false},
		{"sub/keep.log", false, false},
		{"build", true, true},
		{"build", false, false},
		{"sub/build", true, true},
		{"root.txt", false, true},
		{"sub/root.txt", false, false},
		{"docs/tmp", true, true},
		{"docs/x/y/tmp", true, true},
		{"docs/x/y/other", true, false},
		{"coverage", true, true},
		{"main.go", false, false},
	}

	for _, c := range cases {
		if got := m.ignored(c.rel, c.isDir); got != c.want {
			t.Errorf("ignored(%q, dir=%v) = %v, want %v", c.rel, c.isDir, got, c.want)
		}
	}
}

func TestLoadGitignoreMissing(t *testing.T) {
	m, err := loadGitignore(t.TempDir())
	if err != nil {
		t.Fatalf("loadGitignore: %v", err)
	}
	if m != nil {
		t.Fatal("expected nil matcher for a missing .gitignore")
	}
	if m.ignored("a.go", false) {
		t.Fatal("nil matcher must ignore nothing")
	}
}

func TestWalkCodeFilesHonorsGitignoreAndExcludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "generated/\n*.min.js\nsrc/gen/**\n!src/gen/keep.go\n")
	writeFile(t, root, "src/app.go", "package x\n")
	writeFile(t, root, "src/gen/drop.go", "package x\n")
	writeFile(t, root, "src/gen/keep.go", "package x\n")
	writeFile(t, root, "keep/lib.js", "var a = 1;\n")
	writeFile(t, root, "app.min.js", "var a=1;\n")
	writeFile(t, root, "generated/out.go", "package x\n")
	writeFile(t, root, "tmp/scratch.go", "package x\n")

	cfg := scanConfig{maxFiles: defaultMaxFiles}
	gi, err := loadGitignore(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.gitignore = gi

	paths, skipped, truncated, err := walkCodeFiles(root, cfg)
	if err != nil {
		t.Fatalf("walkCodeFiles: %v", err)
	}
	if len(skipped) != 0 || truncated {
		t.Fatalf("skipped=%v truncated=%v, want empty/false", skipped, truncated)
	}
	got := relPaths(t, root, paths)
	assertStrings(t, got, []string{"keep/lib.js", "src/app.go", "src/gen/keep.go", "tmp/scratch.go"})

	// --exclude should prune the tmp directory and normalize OS separators.
	excludes, err := newExcludeMatcher([]string{"tmp", filepath.FromSlash("src/gen")})
	if err != nil {
		t.Fatal(err)
	}
	cfg.excludes = excludes
	paths, _, _, err = walkCodeFiles(root, cfg)
	if err != nil {
		t.Fatalf("walkCodeFiles: %v", err)
	}
	got = relPaths(t, root, paths)
	assertStrings(t, got, []string{"keep/lib.js", "src/app.go"})
}

func TestMatchSegmentsNoExponentialBacktracking(t *testing.T) {
	rule, ok := parsePattern("a/**/**/**/**/**/**/b/**/c/**/d/**/z")
	if !ok {
		t.Fatal("parsePattern failed")
	}
	// Deep non-matching path that would stall naive recursive "**" matching.
	deep := "a/" + strings.Repeat("x/", 40) + "y"
	if rule.matches(deep, false) {
		t.Fatal("expected deep non-matching path to return false")
	}
	match := "a/" + strings.Repeat("x/", 20) + "b/m/c/n/d/o/z"
	if !rule.matches(match, false) {
		t.Fatal("expected deep matching path to return true")
	}
}

func TestWalkCodeFilesNoLimits(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeFile(t, root, "f"+string(rune('a'+i))+".go", "package x\n")
	}

	paths, _, truncated, err := walkCodeFiles(root, scanConfig{maxFiles: 0})
	if err != nil {
		t.Fatalf("walkCodeFiles: %v", err)
	}
	if truncated {
		t.Fatal("expected truncated=false when maxFiles=0")
	}
	if len(paths) != 5 {
		t.Fatalf("collected %d files, want 5", len(paths))
	}
}

func TestWalkCodeFilesExactMaxFilesNotTruncated(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeFile(t, root, "f"+string(rune('a'+i))+".go", "package x\n")
	}

	paths, _, truncated, err := walkCodeFiles(root, scanConfig{maxFiles: 5})
	if err != nil {
		t.Fatalf("walkCodeFiles: %v", err)
	}
	if len(paths) != 5 {
		t.Fatalf("collected %d files, want 5", len(paths))
	}
	if truncated {
		t.Fatal("expected truncated=false when file count equals maxFiles")
	}

	writeFile(t, root, "fz.go", "package x\n")
	paths, _, truncated, err = walkCodeFiles(root, scanConfig{maxFiles: 5})
	if err != nil {
		t.Fatalf("walkCodeFiles: %v", err)
	}
	if len(paths) != 5 {
		t.Fatalf("collected %d files, want 5", len(paths))
	}
	if !truncated {
		t.Fatal("expected truncated=true when file count exceeds maxFiles")
	}
}

func TestWalkCodeFilesNoDefaultIgnore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "vendor/dep.go", "package dep\n")
	writeFile(t, root, "app.go", "package x\n")

	paths, _, _, err := walkCodeFiles(root, scanConfig{maxFiles: defaultMaxFiles})
	if err != nil {
		t.Fatalf("walkCodeFiles: %v", err)
	}
	assertStrings(t, relPaths(t, root, paths), []string{"app.go"})

	paths, _, _, err = walkCodeFiles(root, scanConfig{maxFiles: defaultMaxFiles, noDefaultIgnore: true})
	if err != nil {
		t.Fatalf("walkCodeFiles: %v", err)
	}
	assertStrings(t, relPaths(t, root, paths), []string{"app.go", "vendor/dep.go"})
}

func TestWalkCodeFilesSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := writeFile(t, t.TempDir(), "ext.go", "package ext\n")
	writeFile(t, root, "app.go", "package x\n")

	link := filepath.Join(root, "link.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	paths, _, _, err := walkCodeFiles(root, scanConfig{maxFiles: defaultMaxFiles})
	if err != nil {
		t.Fatalf("walkCodeFiles: %v", err)
	}
	assertStrings(t, relPaths(t, root, paths), []string{"app.go"})
}

func TestWalkCodeFilesRootNamedLikeIgnoredDir(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "build")
	writeFile(t, root, "app.go", "package x\n")

	paths, _, _, err := walkCodeFiles(root, scanConfig{maxFiles: defaultMaxFiles})
	if err != nil {
		t.Fatalf("walkCodeFiles: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("collected %d files, want 1 (the root must not be skipped)", len(paths))
	}
}

func TestNewExcludeMatcherRejectsBadPattern(t *testing.T) {
	if _, err := newExcludeMatcher([]string{"["}); err == nil {
		t.Fatal("expected an error for an invalid glob")
	}
}

func TestTraverseDirectoryHonorsMaxFiles(t *testing.T) {
	root := t.TempDir()
	for d := 0; d < 3; d++ {
		for i := 0; i < 150; i++ {
			name := "d" + string(rune('a'+d)) + "/f" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".go"
			writeFile(t, root, name, "package x\n")
		}
	}

	files, skipped, truncated, err := traverseDirectory(root, scanConfig{maxFiles: defaultMaxFiles})
	if err != nil {
		t.Fatalf("traverseDirectory: %v", err)
	}
	if !truncated {
		t.Fatal("expected truncated=true when directory exceeds defaultMaxFiles")
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped %d files, want 0", len(skipped))
	}
	if len(files) != defaultMaxFiles {
		t.Fatalf("collected %d files, want exactly %d", len(files), defaultMaxFiles)
	}
}

func TestTraverseDirectorySkipsIgnoredDirsAndOtherFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "node_modules/dep.js", "var a = 1;\n")
	writeFile(t, root, "readme.md", "# hi\n")
	writeFile(t, root, "app.go", "package x\n")

	files, skipped, truncated, err := traverseDirectory(root, scanConfig{maxFiles: defaultMaxFiles})
	if err != nil {
		t.Fatalf("traverseDirectory: %v", err)
	}
	if truncated {
		t.Fatal("expected truncated=false")
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped %d files, want 0", len(skipped))
	}
	if len(files) != 1 {
		t.Fatalf("collected %d files, want 1 (only app.go)", len(files))
	}
	if filepath.Base(files[0].path) != "app.go" {
		t.Fatalf("collected %q, want app.go", files[0].path)
	}
}

func TestTraverseDirectorySkipsBadFilesInsteadOfFailing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "good.go", "package x\n\nvar a = 1\n")
	writeFile(t, root, "binary.go", "package x\n\x00\n")

	big := strings.Repeat("x := 1\n", maxFileBytes/7+1)
	writeFile(t, root, "big.go", big)

	files, skipped, truncated, err := traverseDirectory(root, scanConfig{maxFiles: defaultMaxFiles})
	if err != nil {
		t.Fatalf("traverseDirectory returned a fatal error: %v", err)
	}
	if truncated {
		t.Fatal("expected truncated=false")
	}
	if len(files) != 1 {
		t.Fatalf("collected %d files, want 1", len(files))
	}
	if files[0].lineCount != 2 {
		t.Fatalf("line count = %d, want 2", files[0].lineCount)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped %d files, want 2 (binary.go, big.go)", len(skipped))
	}
	if !hasWrapped(skipped, errBinaryFile) || !hasWrapped(skipped, errFileTooLarge) {
		t.Fatalf("skipped errors missing binary/too-large: %v", skipped)
	}
}

func hasWrapped(errs []error, target error) bool {
	for _, err := range errs {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func TestSortFiles(t *testing.T) {
	files := []FileInfo{
		{path: "b.go", lineCount: 10},
		{path: "a.go", lineCount: 10},
		{path: "c.go", lineCount: 50},
		{path: "d.go", lineCount: 1},
	}
	sortFiles(files)

	want := []string{"c.go", "a.go", "b.go", "d.go"}
	for i, w := range want {
		if files[i].path != w {
			t.Fatalf("position %d = %q, want %q", i, files[i].path, w)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := map[string]int{
		"abc":       3,
		"a─b":       3, // box drawing is single width
		"日本語":       6, // CJK is double width
		"a日b":       4,
		"e\u0301":   1, // combining acute adds no column
		"":          0,
		"  main.go": 9,
	}
	for in, want := range cases {
		if got := displayWidth(in); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestPadRight(t *testing.T) {
	if got := padRight("日本語", 10); displayWidth(got) != 10 {
		t.Fatalf("padded width = %d, want 10 (%q)", displayWidth(got), got)
	}
	if got := padRight("longer-than-width", 3); got != "longer-than-width" {
		t.Fatalf("padRight truncated to %q", got)
	}
}

func TestFileIconWidth(t *testing.T) {
	exts := []string{".go", ".py", ".ts", ".c", ".html", ".css", ".rb"}
	for _, ext := range exts {
		if w := displayWidth(fileIcon(ext, false)); w != 2 {
			t.Errorf("nerd icon for %s has width %d, want 2", ext, w)
		}
		if w := displayWidth(fileIcon(ext, true)); w != 2 {
			t.Errorf("ascii icon for %s has width %d, want 2", ext, w)
		}
	}
}

func TestBuildReport(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package x\n")
	writeFile(t, root, "sub/b.go", "package x\n")

	files := []FileInfo{
		{path: filepath.Join(root, "a.go"), lineCount: 1},
		{path: filepath.Join(root, "sub", "b.go"), lineCount: 1},
	}
	skipped := []error{errors.New("weird.go: binary content")}

	report := buildReport(root, files, skipped, false)
	if report.Version != version {
		t.Errorf("version = %q, want %q", report.Version, version)
	}
	if report.TotalFiles != 2 || report.TotalLines != 2 {
		t.Errorf("totals = %d files / %d lines, want 2/2", report.TotalFiles, report.TotalLines)
	}
	if len(report.Skipped) != 1 {
		t.Errorf("skipped = %v, want 1 entry", report.Skipped)
	}
	if report.Files[1].Path != "sub/b.go" {
		t.Errorf("path = %q, want sub/b.go (slash separated, relative)", report.Files[1].Path)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["totalFiles"].(float64) != 2 {
		t.Errorf("json totalFiles = %v", decoded["totalFiles"])
	}
}

func relPaths(t *testing.T, root string, paths []string) []string {
	t.Helper()
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			t.Fatalf("rel: %v", err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
