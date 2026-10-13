package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// All patch coordinates refer to the current index, NOT to HEAD. This matters
// when a file already contains staged changes before Patchtree is launched.
type Repository struct{ Root string }

type DiffLine struct {
	Kind      byte // ' ', '+', or '-'
	Text      string
	NoNewline bool // Git's "\ No newline at end of file" applies to this line.
}

type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Section            string
	Lines              []DiffLine
}

type LineRef struct{ Hunk, Line int }

type FileDiff struct {
	Path, OldPath, NewPath string
	OldMode, NewMode       string
	Header                 []string
	Hunks                  []Hunk
	Raw, Fingerprint       string
	Additions, Deletions   int
	New, Deleted           bool
	Untracked, Binary      bool
	PartialReason          string
	Conflict               bool
}

type Snapshot struct {
	Files       []*FileDiff
	StagedFiles int // Distinct paths in git diff --cached (including existing stages).
}

func (s Snapshot) File(name string) *FileDiff {
	for _, f := range s.Files {
		if f.Path == name {
			return f
		}
	}
	return nil
}

var ErrNoChanges = errors.New("no changed lines selected")

func OpenRepository(ctx context.Context, dir string) (*Repository, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	r := &Repository{Root: abs}
	out, err := r.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a Git working tree: %w", err)
	}
	// Remove just Git's output terminator; spaces can be part of the root name.
	r.Root = strings.TrimSuffix(string(out), "\n")
	return r, nil
}

func (r *Repository) command(ctx context.Context, args ...string) *exec.Cmd {
	// Literal pathspecs protect names such as ":(glob)*"; no shell is involved.
	base := []string{"--no-pager", "--no-optional-locks", "--literal-pathspecs",
		"-c", "core.quotepath=false", "-c", "color.ui=false"}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Dir = r.Root
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "GIT_TERMINAL_PROMPT=0")
	return cmd
}

func (r *Repository) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := r.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %s (%w)", args[0], strings.TrimSpace(stderr.String()), err)
	}
	return stdout.Bytes(), nil
}

var diffArgs = []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv",
	"--no-renames", "--ignore-submodules=none", "--submodule=short", "--src-prefix=a/", "--dst-prefix=b/",
	"--unified=3", "--diff-filter=ACDMRTXB"}

// ReadSnapshot is read-only. In particular, untracked files are NOT added with
// intent-to-add: diff --no-index lets us preview them without mutating the index.
func (r *Repository) ReadSnapshot(ctx context.Context) (Snapshot, error) {
	var result Snapshot
	index, err := r.git(ctx, "ls-files", "--stage", "-z")
	if err != nil {
		return result, err
	}
	modes, conflicts := map[string]string{}, map[string]bool{}
	for _, record := range nulRecords(index) {
		parts := strings.SplitN(record, "\t", 2)
		if len(parts) != 2 {
			return result, fmt.Errorf("invalid ls-files record")
		}
		fields := strings.Fields(parts[0])
		if len(fields) != 3 {
			return result, fmt.Errorf("invalid ls-files metadata")
		}
		modes[parts[1]] = fields[0]
		if fields[2] != "0" {
			conflicts[parts[1]] = true
		}
	}
	raw, err := r.git(ctx, append(append([]string{}, diffArgs...), "--")...)
	if err != nil {
		return result, err
	}
	tracked, err := ParseDiff(withoutCombinedDiffs(string(raw)))
	if err != nil {
		return result, fmt.Errorf("parse unstaged diff: %w", err)
	}
	byPath := make(map[string]*FileDiff)
	for _, f := range tracked {
		if conflicts[f.Path] {
			continue
		}
		if f.OldMode == "" {
			f.OldMode = modes[f.Path]
		}
		if f.NewMode == "" {
			f.NewMode = f.OldMode
		}
		classifyFile(f)
		if previous := byPath[f.Path]; previous != nil {
			// Git can produce a deletion and an addition for one type-changing
			// path. Do not attempt text-only staging of that compound change.
			previous.Raw += f.Raw
			previous.Fingerprint = fingerprint(previous.Raw)
			previous.PartialReason = "file-type change; stage the whole file with f"
			previous.Additions += f.Additions
			previous.Deletions += f.Deletions
			continue
		}
		byPath[f.Path] = f
	}
	for name := range conflicts {
		byPath[name] = &FileDiff{Path: name, Conflict: true,
			Fingerprint: "conflict", PartialReason: "unmerged path; resolve conflicts outside Patchtree"}
	}

	untracked, err := r.git(ctx, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return result, err
	}
	for _, name := range nulRecords(untracked) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		f, err := r.readUntracked(ctx, name)
		if err != nil {
			// A file can disappear between ls-files and diff during watch mode.
			if _, statErr := os.Lstat(filepath.Join(r.Root, filepath.FromSlash(name))); os.IsNotExist(statErr) {
				continue
			}
			return result, err
		}
		byPath[name] = f
	}
	for _, f := range byPath {
		result.Files = append(result.Files, f)
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })

	staged, err := r.git(ctx, "diff", "--cached", "--no-renames", "--no-ext-diff",
		"--ignore-submodules=none", "--name-only", "--diff-filter=ACDMRTXB", "-z", "--")
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	for _, name := range nulRecords(staged) {
		seen[name] = true
	}
	result.StagedFiles = len(seen)
	return result, nil
}

func (r *Repository) readUntracked(ctx context.Context, name string) (*FileDiff, error) {
	// ls-files emits an embedded repository as a trailing-slash entry, not
	// as its contents. Keep it visible without recursively diffing its .git.
	if strings.HasSuffix(name, "/") {
		name = strings.TrimSuffix(name, "/")
		if !validRepoPath(name) {
			return nil, fmt.Errorf("unsafe Git path %q", name)
		}
		head, err := r.git(ctx, "-C", name, "rev-parse", "--verify", "HEAD")
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		identity := strings.TrimSpace(string(head))
		reason := "embedded repository; f adds only its current commit, not its contents"
		if err != nil {
			identity = "no-HEAD"
			reason = "embedded repository without a commit; stage files inside that repository first"
		}
		return &FileDiff{Path: name, Untracked: true, New: true, NewMode: "160000",
			PartialReason: reason, Fingerprint: fingerprint(name + "\x00gitlink:" + identity),
			Header: []string{"Embedded repository: " + name, "HEAD: " + identity}}, nil
	}
	if !validRepoPath(name) {
		return nil, fmt.Errorf("unsafe Git path %q", name)
	}
	args := []string{"diff", "--no-index", "--no-color", "--no-ext-diff", "--no-textconv",
		"--src-prefix=a/", "--dst-prefix=b/", "--unified=3", "--", os.DevNull, name}
	cmd := r.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// --no-index exits 1 when there are differences, not just on error.
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("preview %q: %s (%w)", name, strings.TrimSpace(stderr.String()), err)
		}
	}
	files, err := ParseDiff(stdout.String())
	if err != nil {
		return nil, err
	}
	var f *FileDiff
	if len(files) == 0 {
		// An empty untracked file can have no text diff; retain it in the tree.
		f = &FileDiff{Path: name, New: true, NewMode: "100644"}
	} else if len(files) == 1 {
		f = files[0]
	} else {
		return nil, fmt.Errorf("unexpected compound diff for untracked path %q", name)
	}
	f.Path, f.Untracked, f.New = name, true, true
	f.Fingerprint = fingerprint(f.Raw + "\x00untracked:" + name)
	classifyFile(f)
	return f, nil
}

func classifyFile(f *FileDiff) {
	switch {
	case f.Conflict:
		f.PartialReason = "unmerged path; resolve conflicts outside Patchtree"
	case f.Binary:
		f.PartialReason = "binary file; stage the whole file with f"
	case f.OldMode == "160000" || f.NewMode == "160000":
		f.PartialReason = "submodule; stage its recorded commit with f (not its contents)"
	case f.OldMode == "120000" || f.NewMode == "120000":
		f.PartialReason = "symbolic link; stage the whole file with f"
	case len(f.Hunks) == 0 && f.PartialReason == "":
		f.PartialReason = "empty file or mode-only change; stage the whole file with f"
	}
}

// An unmerged path may be emitted as a combined diff even with a diff filter.
// Conflicts are represented separately from ls-files' nonzero index stages;
// combined hunks must never enter the two-way partial-patch parser.
func withoutCombinedDiffs(raw string) string {
	var b strings.Builder
	dropping := false
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --cc ") || strings.HasPrefix(line, "diff --combined "):
			dropping = true
		case strings.HasPrefix(line, "diff --git "):
			dropping = false
		case strings.HasPrefix(line, "* Unmerged path "):
			continue
		}
		if !dropping && line != "" {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func nulRecords(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

func validRepoPath(name string) bool {
	return name != "" && name != "." && !strings.ContainsRune(name, '\x00') && !filepath.IsAbs(name) &&
		path.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}

func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:16])
}

var hunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

func ParseDiff(raw string) ([]*FileDiff, error) { return parseDiff(raw, true) }

func parseDiff(raw string, verifyCounts bool) ([]*FileDiff, error) {
	if raw == "" {
		return nil, nil
	}
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	var blocks [][]string
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			blocks = append(blocks, []string{line})
		} else if len(blocks) != 0 {
			blocks[len(blocks)-1] = append(blocks[len(blocks)-1], line)
		} else if line != "" {
			return nil, fmt.Errorf("expected a diff --git header")
		}
	}
	var files []*FileDiff
	for _, block := range blocks {
		f, err := parseFile(block, verifyCounts)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

func parseFile(lines []string, verifyCounts bool) (*FileDiff, error) {
	f := &FileDiff{Raw: strings.Join(lines, "\n") + "\n"}
	var current *Hunk
	for _, line := range lines {
		if strings.HasPrefix(line, "@@ ") {
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			f.Hunks = append(f.Hunks, h)
			current = &f.Hunks[len(f.Hunks)-1]
			continue
		}
		if current != nil {
			if line == `\ No newline at end of file` {
				if len(current.Lines) == 0 || current.Lines[len(current.Lines)-1].NoNewline {
					return nil, fmt.Errorf("orphan no-newline marker")
				}
				current.Lines[len(current.Lines)-1].NoNewline = true
				continue
			}
			if line == "" || !strings.ContainsRune(" +-", rune(line[0])) {
				return nil, fmt.Errorf("invalid hunk body line %q", line)
			}
			current.Lines = append(current.Lines, DiffLine{Kind: line[0], Text: line[1:]})
			if line[0] == '+' {
				f.Additions++
			} else if line[0] == '-' {
				f.Deletions++
			}
			continue
		}
		f.Header = append(f.Header, line)
		switch {
		case strings.HasPrefix(line, "--- "):
			p, err := patchPath(strings.TrimPrefix(line, "--- "), "a/")
			if err != nil {
				return nil, err
			}
			f.OldPath = p
		case strings.HasPrefix(line, "+++ "):
			p, err := patchPath(strings.TrimPrefix(line, "+++ "), "b/")
			if err != nil {
				return nil, err
			}
			f.NewPath = p
		case strings.HasPrefix(line, "new file mode "):
			f.New, f.NewMode = true, strings.TrimPrefix(line, "new file mode ")
		case strings.HasPrefix(line, "deleted file mode "):
			f.Deleted, f.OldMode = true, strings.TrimPrefix(line, "deleted file mode ")
		case strings.HasPrefix(line, "old mode "):
			f.OldMode = strings.TrimPrefix(line, "old mode ")
		case strings.HasPrefix(line, "new mode "):
			f.NewMode = strings.TrimPrefix(line, "new mode ")
		case strings.HasPrefix(line, "index "):
			fields := strings.Fields(line)
			if len(fields) == 3 && len(fields[2]) == 6 {
				f.OldMode, f.NewMode = fields[2], fields[2]
			}
		case line == "GIT binary patch" || strings.HasPrefix(line, "Binary files "):
			f.Binary = true
		case strings.HasPrefix(line, "rename ") || strings.HasPrefix(line, "copy "):
			f.PartialReason = "rename/copy metadata requires whole-file staging"
		}
	}
	if f.NewPath != "" && f.NewPath != os.DevNull {
		f.Path = f.NewPath
	} else if f.OldPath != "" && f.OldPath != os.DevNull {
		f.Path = f.OldPath
	} else {
		old, next, err := diffHeaderPaths(lines[0])
		if err != nil {
			return nil, err
		}
		f.OldPath, f.NewPath, f.Path = old, next, next
	}
	if !validRepoPath(f.Path) {
		return nil, fmt.Errorf("unsafe path in patch: %q", f.Path)
	}
	if f.OldPath != "" && f.OldPath != os.DevNull && f.OldPath != f.Path {
		f.PartialReason = "rename metadata requires whole-file staging"
	}
	if verifyCounts {
		for _, h := range f.Hunks {
			old, next := countLines(h.Lines)
			if old != h.OldLines || next != h.NewLines {
				return nil, fmt.Errorf("%s: hunk counts disagree: header %d/%d, body %d/%d",
					f.Path, h.OldLines, h.NewLines, old, next)
			}
		}
	}
	f.Fingerprint = fingerprint(f.Raw)
	return f, nil
}

func parseHunkHeader(line string) (Hunk, error) {
	m := hunkRE.FindStringSubmatch(line)
	if m == nil {
		return Hunk{}, fmt.Errorf("invalid hunk header %q", line)
	}
	var nums [4]int
	for i := range nums {
		if m[i+1] == "" { // Omitted count means one, not zero.
			nums[i] = 1
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Hunk{}, fmt.Errorf("invalid hunk coordinate: %w", err)
		}
		nums[i] = n
	}
	return Hunk{OldStart: nums[0], OldLines: nums[1], NewStart: nums[2], NewLines: nums[3],
		Section: strings.TrimPrefix(m[5], " ")}, nil
}

func patchPath(s, prefix string) (string, error) {
	// Git appends a structural tab to unquoted ---/+++ paths containing
	// spaces. Real tabs in filenames are C-quoted, so this is unambiguous.
	s = strings.TrimSuffix(s, "\t")
	if strings.HasPrefix(s, `"`) {
		p, err := strconv.Unquote(s)
		if err != nil {
			return "", fmt.Errorf("invalid Git-quoted path: %w", err)
		}
		s = p
	}
	if s == os.DevNull || s == "/dev/null" {
		return os.DevNull, nil
	}
	if !strings.HasPrefix(s, prefix) {
		return "", fmt.Errorf("expected path prefix %q in %q", prefix, s)
	}
	return strings.TrimPrefix(s, prefix), nil
}

func diffHeaderPaths(header string) (string, string, error) {
	s := strings.TrimPrefix(header, "diff --git ")
	if strings.HasPrefix(s, `"`) {
		// Consume a C-quoted path, allowing escaped quotation marks.
		end, escaped := -1, false
		for i := 1; i < len(s); i++ {
			if escaped {
				escaped = false
			} else if s[i] == '\\' {
				escaped = true
			} else if s[i] == '"' {
				end = i + 1
				break
			}
		}
		if end > 0 && end < len(s) && s[end] == ' ' {
			old, err := patchPath(s[:end], "a/")
			if err != nil {
				return "", "", err
			}
			next, err := patchPath(s[end+1:], "b/")
			return old, next, err
		}
	} else {
		// --no-renames makes both paths identical. This disambiguates spaces
		// (even a filename containing the literal substring " b/").
		for i := 0; i < len(s); i++ {
			if strings.HasPrefix(s[i:], " b/") && strings.HasPrefix(s, "a/") && s[2:i] == s[i+3:] {
				return s[2:i], s[i+3:], nil
			}
		}
	}
	return "", "", fmt.Errorf("cannot parse diff header paths: %q", header)
}

func countLines(lines []DiffLine) (old, next int) {
	for _, line := range lines {
		if line.Kind != '+' {
			old++
		}
		if line.Kind != '-' {
			next++
		}
	}
	return
}

func (h Hunk) HeaderLine() string {
	s := fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
	if h.Section != "" {
		s += " " + h.Section
	}
	return s
}

func writeHunk(b *strings.Builder, h Hunk) {
	b.WriteString(h.HeaderLine() + "\n")
	for _, line := range h.Lines {
		b.WriteByte(line.Kind)
		b.WriteString(line.Text)
		b.WriteByte('\n')
		if line.NoNewline {
			b.WriteString("\\ No newline at end of file\n")
		}
	}
}

// quoteGitPath emits Git's C-style byte quoting, not shell quoting. Using
// octal escapes also handles invalid UTF-8 filenames without lossy conversion.
func quoteGitPath(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 32 || c >= 127:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// BuildPatch projects the worktree diff onto just the selected changed lines.
// Unselected '-' lines become context; unselected '+' lines disappear. Taking
// a slice of the raw patch would silently delete unselected lines and is WRONG.
func BuildPatch(f *FileDiff, selected map[LineRef]bool) (string, error) {
	if f == nil {
		return "", ErrNoChanges
	}
	if f.PartialReason != "" || f.Binary || f.Conflict {
		return "", fmt.Errorf("cannot partially stage: %s", f.PartialReason)
	}
	chosen, total := 0, 0
	for hi, h := range f.Hunks {
		for li, line := range h.Lines {
			if line.Kind == ' ' {
				continue
			}
			total++
			if selected[LineRef{hi, li}] {
				chosen++
			}
		}
	}
	for ref, on := range selected {
		if on && (ref.Hunk < 0 || ref.Hunk >= len(f.Hunks) || ref.Line < 0 || ref.Line >= len(f.Hunks[ref.Hunk].Lines)) {
			return "", fmt.Errorf("stale line selection; refresh the diff")
		}
	}
	if chosen == 0 {
		return "", ErrNoChanges
	}
	deleteFile := f.Deleted && chosen == total
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git %s %s\n", quoteGitPath("a/"+f.Path), quoteGitPath("b/"+f.Path))
	oldPath, newPath := quoteGitPath("a/"+f.Path), quoteGitPath("b/"+f.Path)
	if f.New {
		mode := f.NewMode
		if mode == "" {
			mode = "100644"
		}
		fmt.Fprintf(&b, "new file mode %s\n", mode)
		oldPath = "/dev/null"
	}
	if deleteFile {
		mode := f.OldMode
		if mode == "" {
			mode = "100644"
		}
		fmt.Fprintf(&b, "deleted file mode %s\n", mode)
		newPath = "/dev/null"
	}
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", oldPath, newPath)

	delta := 0 // Only SELECTED changes in earlier hunks shift the new coordinates.
	for hi, original := range f.Hunks {
		h := original
		h.Lines = nil
		hasChange := false
		for li, line := range original.Lines {
			if line.Kind != ' ' && selected[LineRef{hi, li}] {
				hasChange = true
			} else if line.Kind == '+' {
				continue
			} else if line.Kind == '-' {
				line.Kind = ' '
			}
			h.Lines = append(h.Lines, line)
		}
		if !hasChange {
			continue
		}
		h.OldLines, h.NewLines = countLines(h.Lines)
		oldOffset := h.OldStart
		if original.OldLines != 0 {
			oldOffset--
		}
		h.NewStart = oldOffset + delta
		if h.NewLines != 0 {
			h.NewStart++
		}
		if err := validateNoNewline(h); err != nil {
			return "", err
		}
		writeHunk(&b, h)
		delta += h.NewLines - h.OldLines
	}
	return b.String(), nil
}

func validateNoNewline(h Hunk) error {
	oldLast, newLast := -1, -1
	for i, line := range h.Lines {
		if line.Kind != '+' {
			oldLast = i
		}
		if line.Kind != '-' {
			newLast = i
		}
	}
	for i, line := range h.Lines {
		if !line.NoNewline {
			continue
		}
		if (line.Kind != '+' && i != oldLast) || (line.Kind != '-' && i != newLast) {
			return fmt.Errorf("selection would put a no-newline-at-EOF line inside the file; select the entire final replacement")
		}
	}
	return nil
}

// ValidateEditedPatch restricts $EDITOR to the original file and metadata. Hunk
// counts are recounted, as in git add -p; old starts still belong to the editor.
func ValidateEditedPatch(base, edited string) (string, error) {
	var clean strings.Builder
	for _, line := range strings.Split(edited, "\n") {
		if !strings.HasPrefix(line, "#") {
			clean.WriteString(line + "\n")
		}
	}
	raw := strings.TrimRight(clean.String(), "\n")
	if raw == "" {
		return "", ErrNoChanges
	}
	// Preserve a final patch newline without appending an empty body line.
	raw += "\n"
	before, err := ParseDiff(base)
	if err != nil || len(before) != 1 {
		return "", fmt.Errorf("invalid original editor patch")
	}
	after, err := parseDiff(raw, false)
	if err != nil {
		return "", fmt.Errorf("edited patch: %w", err)
	}
	if len(after) != 1 || strings.Join(before[0].Header, "\n") != strings.Join(after[0].Header, "\n") {
		return "", fmt.Errorf("do not edit file headers or add other files; edit hunk bodies and boundaries only")
	}
	if len(after[0].Hunks) == 0 {
		return "", ErrNoChanges
	}
	var b strings.Builder
	headers := after[0].Header
	if before[0].Deleted {
		retainsLines := false
		for _, h := range after[0].Hunks {
			_, count := countLines(h.Lines)
			retainsLines = retainsLines || count > 0
		}
		if retainsLines {
			// Turning a '-' into context keeps part of a deleted file. The
			// user needn't (and mustn't) change paths/modes to express this.
			var normalized []string
			for _, header := range headers {
				if strings.HasPrefix(header, "deleted file mode ") {
					continue
				}
				if strings.HasPrefix(header, "+++ ") {
					header = "+++ " + quoteGitPath("b/"+before[0].Path)
				}
				normalized = append(normalized, header)
			}
			headers = normalized
		}
	}
	b.WriteString(strings.Join(headers, "\n") + "\n")
	delta, changes := 0, 0
	for _, original := range after[0].Hunks {
		h := original
		h.OldLines, h.NewLines = countLines(h.Lines)
		if h.OldStart == 0 && h.OldLines > 0 {
			return "", fmt.Errorf("nonempty old range must start at line 1 or later")
		}
		offset := h.OldStart
		if h.OldLines != 0 {
			offset--
		}
		h.NewStart = offset + delta
		if h.NewLines != 0 {
			h.NewStart++
		}
		for _, line := range h.Lines {
			if line.Kind != ' ' {
				changes++
			}
		}
		if err := validateNoNewline(h); err != nil {
			return "", err
		}
		writeHunk(&b, h)
		delta += h.NewLines - h.OldLines
	}
	if changes == 0 {
		return "", ErrNoChanges
	}
	return b.String(), nil
}

// ApplyPatch never changes the worktree. Git verifies context and takes its own
// index lock during the real application, even if another Git process races the
// preflight check. A failed apply is surfaced to the user, not retried blindly.
func (r *Repository) ApplyPatch(ctx context.Context, patchText string) error {
	if patchText == "" {
		return ErrNoChanges
	}
	f, err := os.CreateTemp("", "patchtree-*.patch")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(patchText); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if _, err = r.git(ctx, "apply", "--cached", "--check", "--whitespace=nowarn", "--", f.Name()); err != nil {
		return fmt.Errorf("patch check failed (nothing staged): %w", err)
	}
	if _, err = r.git(ctx, "apply", "--cached", "--whitespace=nowarn", "--", f.Name()); err != nil {
		return fmt.Errorf("patch application failed: %w", err)
	}
	return nil
}

func (r *Repository) StageFile(ctx context.Context, name string) error {
	if !validRepoPath(name) {
		return fmt.Errorf("unsafe file path %q", name)
	}
	_, err := r.git(ctx, "add", "-A", "--", name)
	return err
}
