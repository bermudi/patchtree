package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	colorText      = "#D6DBE9"
	colorMuted     = "#79849C"
	colorAccent    = "#71DAC6"
	colorAdded     = "#9BE5AC"
	colorRemoved   = "#F39B9B"
	colorPurple    = "#B5A0F5"
	colorPanel     = "#253247"
	colorVisual    = "#30304E"
	colorAddedBg   = "#1d3d28"
	colorRemovedBg = "#46242c"
)

var (
	textStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color(colorText))
	mutedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(colorMuted))
	accentStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAccent)).Bold(true)
	addedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color(colorAdded))
	removedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(colorRemoved))
	addedBgStyle   = lipgloss.NewStyle().Background(lipgloss.Color(colorAddedBg))
	removedBgStyle = lipgloss.NewStyle().Background(lipgloss.Color(colorRemovedBg))
	purpleStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(colorPurple))
)

// renderRowBackground restores the chosen background after resets anywhere
// in the completed row: gutter, syntax spans, clipping marker and padding.
// Keep cached delta output background-free so selection can replace the tint.
func renderRowBackground(text string, style lipgloss.Style) string {
	// Derive the opening SGR from the active color profile, not a hard-coded
	// truecolor sequence. Ascii renders just the space, giving an empty prefix.
	prefix := strings.TrimSuffix(strings.TrimSuffix(style.Render(" "), "\x1b[0m"), " ")
	if prefix != "" {
		text = strings.NewReplacer(
			"\x1b[0m", "\x1b[0m"+prefix,
			"\x1b[m", "\x1b[m"+prefix,
			"\x1b[49m", "\x1b[49m"+prefix,
		).Replace(text)
	}
	return style.Render(text)
}

// Display repository text without passing its terminal controls through.
// Patch generation always uses the untouched original.
func safeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == 0x1b:
			b.WriteString(`\x1b`)
		case unicode.IsControl(r):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func displayPath(s string) string {
	return safeText(strings.ReplaceAll(s, "\t", `\t`))
}

func fit(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = ansi.Truncate(s, width, "")
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

func crop(s string, offset, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) > offset+width {
		return fit(ansi.Cut(s, offset, offset+max(0, width-1))+mutedStyle.Render("›"), width)
	}
	return fit(ansi.Cut(s, offset, offset+width), width)
}

func joinEnds(left, right string, width int) string {
	right = ansi.Truncate(right, width, "")
	left = fit(left, max(0, width-ansi.StringWidth(right)))
	return left + right
}

type TreeRow struct {
	Path, Name           string
	Depth                int
	Dir                  bool
	File                 *FileDiff
	Additions, Deletions int
}

func (r TreeRow) Key() string {
	if r.Dir {
		return r.Path + "/"
	}
	return r.Path
}

type TreeState struct {
	Collapsed        map[string]bool
	Rows             []TreeRow
	Cursor, Offset   int
	CursorKey, Query string
}

type treeNode struct {
	name, fullPath       string
	dir                  bool
	file                 *FileDiff
	children             map[string]*treeNode
	additions, deletions int
}

func treeRows(files []*FileDiff, collapsed map[string]bool, query string) []TreeRow {
	root := &treeNode{children: make(map[string]*treeNode)}
	for _, f := range files {
		if !strings.Contains(strings.ToLower(f.Path), strings.ToLower(query)) {
			continue
		}
		node := root
		components := strings.Split(f.Path, "/")
		for i, name := range components {
			dir := i < len(components)-1
			key := "F:" + name
			if dir {
				key = "D:" + name
			}
			if node.children[key] == nil {
				node.children[key] = &treeNode{name: name, fullPath: strings.Join(components[:i+1], "/"),
					dir: dir, children: make(map[string]*treeNode)}
			}
			node = node.children[key]
			node.additions += f.Additions
			node.deletions += f.Deletions
		}
		node.file = f
	}
	var rows []TreeRow
	var visit func(*treeNode, int)
	visit = func(parent *treeNode, depth int) {
		children := make([]*treeNode, 0, len(parent.children))
		for _, node := range parent.children {
			children = append(children, node)
		}
		sort.Slice(children, func(i, j int) bool {
			if children[i].dir != children[j].dir {
				return children[i].dir
			}
			return children[i].name < children[j].name
		})
		for _, node := range children {
			rows = append(rows, TreeRow{Path: node.fullPath, Name: node.name, Dir: node.dir,
				Depth: depth, File: node.file, Additions: node.additions, Deletions: node.deletions})
			if node.dir && (!collapsed[node.fullPath] || query != "") {
				visit(node, depth+1)
			}
		}
	}
	visit(root, 0)
	return rows
}

func (t *TreeState) Rebuild(files []*FileDiff) {
	if t.Collapsed == nil {
		t.Collapsed = make(map[string]bool)
	}
	t.Rows = treeRows(files, t.Collapsed, t.Query)
	found := false
	for i, row := range t.Rows {
		if row.Key() == t.CursorKey {
			t.Cursor, found = i, true
			break
		}
	}
	if !found {
		t.Cursor = min(max(0, t.Cursor), max(0, len(t.Rows)-1))
	}
	t.rememberCursor()
}

func (t *TreeState) rememberCursor() {
	if len(t.Rows) > 0 {
		t.CursorKey = t.Rows[t.Cursor].Key()
	} else {
		t.CursorKey = ""
		t.Offset = 0
	}
}

func (t *TreeState) Selected() *TreeRow {
	if t.Cursor < 0 || t.Cursor >= len(t.Rows) {
		return nil
	}
	return &t.Rows[t.Cursor]
}

func (t *TreeState) Move(delta int) {
	t.Cursor = min(max(0, t.Cursor+delta), max(0, len(t.Rows)-1))
	t.rememberCursor()
}

func (t *TreeState) KeepVisible(height int) {
	height = max(1, height)
	t.Offset = min(max(0, t.Offset), max(0, len(t.Rows)-height))
	if t.Cursor < t.Offset {
		t.Offset = t.Cursor
	} else if t.Cursor >= t.Offset+height {
		t.Offset = t.Cursor - height + 1
	}
}

func (t *TreeState) SelectFile(files []*FileDiff, name string, expand bool) {
	if expand {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			t.Collapsed[parent] = false
		}
	}
	t.CursorKey = name
	t.Rebuild(files)
}

func (t *TreeState) FilePaths(files []*FileDiff) []string {
	var result []string
	for _, row := range treeRows(files, nil, t.Query) {
		if !row.Dir {
			result = append(result, row.Path)
		}
	}
	return result
}

// Units are selectable UI ranges into an original hunk. Their context can
// overlap after splitting. We stage a UNION of original LineRefs, never a
// concatenation of split patches (which would double-apply shared context).
type HunkUnit struct {
	Hunk, Start, End int // End is exclusive.
	ID               string
	Skipped          bool
}

func makeUnit(f *FileDiff, hi, start, end int) HunkUnit {
	var b strings.Builder
	for _, line := range f.Hunks[hi].Lines[start:end] {
		fmt.Fprintf(&b, "%c%s\n%t\n", line.Kind, line.Text, line.NoNewline)
	}
	return HunkUnit{Hunk: hi, Start: start, End: end, ID: fingerprint(f.Path + "\x00" + b.String())}
}

func initialUnits(f *FileDiff) []HunkUnit {
	if f == nil || f.PartialReason != "" {
		return nil
	}
	units := make([]HunkUnit, 0, len(f.Hunks))
	for hi, h := range f.Hunks {
		units = append(units, makeUnit(f, hi, 0, len(h.Lines)))
	}
	return units
}

func splitUnit(f *FileDiff, unit HunkUnit) ([]HunkUnit, error) {
	type group struct{ start, end int }
	var groups []group
	lines := f.Hunks[unit.Hunk].Lines
	for i := unit.Start; i < unit.End; {
		if lines[i].Kind == ' ' {
			i++
			continue
		}
		start := i
		for i < unit.End && lines[i].Kind != ' ' {
			i++
		}
		groups = append(groups, group{start, i})
	}
	if len(groups) < 2 {
		return nil, fmt.Errorf("no context gap to split; use V and Space to select individual lines")
	}
	result := make([]HunkUnit, 0, len(groups))
	for i, g := range groups {
		start, end := max(unit.Start, g.start-3), min(unit.End, g.end+3)
		if i > 0 {
			start = max(start, groups[i-1].end)
		}
		if i+1 < len(groups) {
			end = min(end, groups[i+1].start)
		}
		result = append(result, makeUnit(f, unit.Hunk, start, end))
	}
	return result, nil
}

func unitSelection(f *FileDiff, units []HunkUnit) map[LineRef]bool {
	result := make(map[LineRef]bool)
	for _, u := range units {
		for i := u.Start; i < u.End; i++ {
			if f.Hunks[u.Hunk].Lines[i].Kind != ' ' {
				result[LineRef{u.Hunk, i}] = true
			}
		}
	}
	return result
}

func firstChange(f *FileDiff, unit HunkUnit) int {
	for i := unit.Start; i < unit.End; i++ {
		if f.Hunks[unit.Hunk].Lines[i].Kind != ' ' {
			return i
		}
	}
	return unit.Start
}

type Highlighted struct {
	Fingerprint string
	Lines       map[LineRef]string // Colored code only, without patch +/- markers.
}

// --color-only is deliberately used instead of delta's transformed layout.
// It preserves a one-to-one source-line map for line selection. Patchtree owns
// the split layout and gutters; delta is the syntax/intraline highlighting
// backend. Users' pager, side-by-side and hyperlink settings cannot interfere.
func highlightWithDelta(ctx context.Context, delta, root string, f *FileDiff) (Highlighted, error) {
	result := Highlighted{Fingerprint: f.Fingerprint, Lines: make(map[LineRef]string)}
	if len(f.Hunks) == 0 {
		return result, nil
	}
	var input []string
	refs := make(map[int]LineRef)
	for _, line := range f.Header {
		input = append(input, safeText(line))
	}
	for hi, h := range f.Hunks {
		input = append(input, safeText(h.HeaderLine()))
		for li, line := range h.Lines {
			refs[len(input)] = LineRef{hi, li}
			input = append(input, string(line.Kind)+safeText(line.Text))
			if line.NoNewline {
				input = append(input, `\ No newline at end of file`)
			}
		}
	}
	// Explicit --dark also prevents delta's background-color autodetection
	// from opening /dev/tty and consuming Bubble Tea's keystrokes.
	cmd := exec.CommandContext(ctx, delta, "--color-only", "--no-gitconfig", "--paging=never", "--dark",
		"--true-color=always", "--syntax-theme=Monokai Extended", "--keep-plus-minus-markers",
		"--minus-style=syntax", "--plus-style=syntax", "--zero-style=syntax",
		"--minus-emph-style=syntax bold", "--plus-emph-style=syntax bold")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "DELTA_PAGER=cat", "PAGER=cat")
	cmd.Stdin = strings.NewReader(strings.Join(input, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return result, fmt.Errorf("delta: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}
	output := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(output) != len(input) {
		return result, fmt.Errorf("delta changed line structure; using the built-in diff renderer")
	}
	for i, original := range input {
		if ansi.Strip(output[i]) != original {
			return result, fmt.Errorf("delta changed source text; using the built-in diff renderer")
		}
		if ref, ok := refs[i]; ok {
			result.Lines[ref] = ansi.Cut(output[i], 1, ansi.StringWidth(output[i]))
		}
	}
	return result, nil
}

type DiffRow struct {
	Text        string
	Unit        int
	Left, Right *LineRef
}

func (r DiffRow) Contains(ref LineRef) bool {
	return (r.Left != nil && *r.Left == ref) || (r.Right != nil && *r.Right == ref)
}

func (r DiffRow) Ref(preferRight bool) (LineRef, bool) {
	if preferRight && r.Right != nil {
		return *r.Right, true
	}
	if r.Left != nil {
		return *r.Left, true
	}
	if r.Right != nil {
		return *r.Right, true
	}
	return LineRef{}, false
}

type numberedLine struct {
	ref        LineRef
	line       DiffLine
	old, newer int
}

func numberLines(f *FileDiff) [][]numberedLine {
	result := make([][]numberedLine, len(f.Hunks))
	for hi, h := range f.Hunks {
		old, newer := h.OldStart, h.NewStart
		for li, line := range h.Lines {
			n := numberedLine{ref: LineRef{hi, li}, line: line}
			if line.Kind != '+' {
				n.old = old
				old++
			}
			if line.Kind != '-' {
				n.newer = newer
				newer++
			}
			result[hi] = append(result[hi], n)
		}
	}
	return result
}

func (m Model) currentRef() (LineRef, bool) {
	if m.Unit < 0 || m.Unit >= len(m.Units) {
		return LineRef{}, false
	}
	u := m.Units[m.Unit]
	return LineRef{u.Hunk, m.Line}, m.Line >= u.Start && m.Line < u.End
}

func (m Model) inVisual(ref LineRef) bool {
	if !m.Visual || m.Unit < 0 || m.Unit >= len(m.Units) {
		return false
	}
	u := m.Units[m.Unit]
	return ref.Hunk == u.Hunk && ref.Line >= min(m.Anchor, m.Line) && ref.Line <= max(m.Anchor, m.Line)
}

func (m Model) lineMarker(ref LineRef, kind byte) string {
	cursor, valid := m.currentRef()
	left := " "
	if valid && cursor == ref {
		left = purpleStyle.Bold(true).Render("›")
	}
	right := " "
	if m.Marks[ref] && kind != ' ' {
		right = addedStyle.Bold(true).Render("✓")
	} else if m.inVisual(ref) {
		right = purpleStyle.Render("▌")
	}
	return left + right
}

func (m Model) code(n numberedLine) string {
	code := ""
	if cache, ok := m.Colors[m.ActivePath]; ok && cache.Fingerprint == m.activeFile().Fingerprint {
		code = cache.Lines[n.ref]
	}
	if code == "" {
		style := textStyle
		if n.line.Kind == '+' {
			style = addedStyle
		} else if n.line.Kind == '-' {
			style = removedStyle
		}
		code = style.Render(safeText(n.line.Text))
	}
	if n.line.NoNewline {
		code += mutedStyle.Render("  ⏎ no newline")
	}
	return code
}

func lineNumber(n, width int) string {
	if n == 0 {
		return strings.Repeat(" ", width)
	}
	return fmt.Sprintf("%*d", width, n)
}

func changeSign(kind byte) string {
	switch kind {
	case '+':
		return addedStyle.Bold(true).Render("+")
	case '-':
		return removedStyle.Bold(true).Render("−")
	default:
		return " "
	}
}

func (m Model) unifiedRow(n numberedLine, ui, width, digits int) DiffRow {
	gutter := m.lineMarker(n.ref, n.line.Kind) + mutedStyle.Render(lineNumber(n.old, digits)+" "+lineNumber(n.newer, digits)+" ") +
		changeSign(n.line.Kind) + mutedStyle.Render(" │ ")
	text := gutter + crop(m.code(n), m.Horizontal, width-ansi.StringWidth(gutter))
	rowText := fit(text, width)
	switch {
	case m.inVisual(n.ref) || m.Marks[n.ref]:
		rowText = renderRowBackground(rowText, lipgloss.NewStyle().Background(lipgloss.Color(colorVisual)))
	case n.line.Kind == '+':
		rowText = renderRowBackground(rowText, addedBgStyle)
	case n.line.Kind == '-':
		rowText = renderRowBackground(rowText, removedBgStyle)
	}
	ref := n.ref
	row := DiffRow{Text: rowText, Unit: ui}
	if n.line.Kind != '+' {
		row.Left = &ref
	}
	if n.line.Kind != '-' {
		row.Right = &ref
	}
	return row
}

func (m Model) sideCell(n *numberedLine, right bool, width, digits int) string {
	if n == nil {
		return strings.Repeat(" ", max(0, width))
	}
	num := n.old
	if right {
		num = n.newer
	}
	gutter := m.lineMarker(n.ref, n.line.Kind) + mutedStyle.Render(lineNumber(num, digits)+" ") + changeSign(n.line.Kind) + " "
	text := gutter + crop(m.code(*n), m.Horizontal, width-ansi.StringWidth(gutter))
	rowText := fit(text, width)
	switch {
	case m.inVisual(n.ref) || m.Marks[n.ref]:
		rowText = renderRowBackground(rowText, lipgloss.NewStyle().Background(lipgloss.Color(colorVisual)))
	case n.line.Kind == '+':
		rowText = renderRowBackground(rowText, addedBgStyle)
	case n.line.Kind == '-':
		rowText = renderRowBackground(rowText, removedBgStyle)
	}
	return rowText
}

func (m Model) sideRow(left, right *numberedLine, ui, width, digits int) DiffRow {
	lw := (width - 1) / 2
	row := DiffRow{Text: m.sideCell(left, false, lw, digits) + mutedStyle.Render("│") +
		m.sideCell(right, true, width-lw-1, digits), Unit: ui}
	if left != nil {
		ref := left.ref
		row.Left = &ref
	}
	if right != nil {
		ref := right.ref
		row.Right = &ref
	}
	return row
}

func (m Model) effectiveSideBySide() bool { return m.SideBySide && m.Viewport.Width >= 64 }

func (m Model) buildDiffRows() []DiffRow {
	f := m.activeFile()
	if f == nil || len(m.Units) == 0 {
		return nil
	}
	width := m.Viewport.Width
	numbered := numberLines(f)
	largest := 1
	for _, h := range f.Hunks {
		largest = max(largest, h.OldStart+h.OldLines, h.NewStart+h.NewLines)
	}
	digits := max(3, len(strconv.Itoa(largest)))
	var rows []DiffRow
	for ui, u := range m.Units {
		h := f.Hunks[u.Hunk]
		plus, minus := 0, 0
		for _, line := range h.Lines[u.Start:u.End] {
			if line.Kind == '+' {
				plus++
			} else if line.Kind == '-' {
				minus++
			}
		}
		prefix, style := "  ", mutedStyle
		if ui == m.Unit {
			prefix, style = "▸ ", accentStyle.Background(lipgloss.Color(colorPanel))
		}
		label := fmt.Sprintf("%sHUNK %d/%d  +%d −%d", prefix, ui+1, len(m.Units), plus, minus)
		if u.Start != 0 || u.End != len(h.Lines) {
			label += "  split"
		}
		if u.Skipped {
			label += "  skipped"
		}
		if h.Section != "" {
			label += "  " + safeText(h.Section)
		}
		rows = append(rows, DiffRow{Text: style.Render(fit(label, width)), Unit: ui})
		part := numbered[u.Hunk][u.Start:u.End]
		if !m.effectiveSideBySide() {
			for _, n := range part {
				rows = append(rows, m.unifiedRow(n, ui, width, digits))
			}
		} else {
			for i := 0; i < len(part); {
				if part[i].line.Kind == ' ' {
					rows = append(rows, m.sideRow(&part[i], &part[i], ui, width, digits))
					i++
					continue
				}
				var removed, added []numberedLine
				for i < len(part) && part[i].line.Kind != ' ' {
					if part[i].line.Kind == '-' {
						removed = append(removed, part[i])
					} else {
						added = append(added, part[i])
					}
					i++
				}
				for j := 0; j < max(len(removed), len(added)); j++ {
					var left, right *numberedLine
					if j < len(removed) {
						left = &removed[j]
					}
					if j < len(added) {
						right = &added[j]
					}
					rows = append(rows, m.sideRow(left, right, ui, width, digits))
				}
			}
		}
		rows = append(rows, DiffRow{Text: strings.Repeat(" ", width), Unit: ui})
	}
	return rows
}

func (m *Model) rebuildDiff(ensureCursor bool) {
	oldOffset := m.Viewport.YOffset
	m.DiffRows = m.buildDiffRows()
	var content []string
	for _, row := range m.DiffRows {
		content = append(content, row.Text)
	}
	if len(content) == 0 {
		content = m.emptyDiff()
	}
	m.Viewport.SetContent(strings.Join(content, "\n"))
	m.Viewport.SetYOffset(oldOffset)
	if ensureCursor {
		m.ensureDiffCursor()
	}
}

func (m Model) emptyDiff() []string {
	f := m.activeFile()
	if f == nil {
		if m.Busy == "loading" {
			return []string{"", accentStyle.Render("  Reading the working tree…")}
		}
		if !m.Loaded {
			return []string{"", removedStyle.Render("  Could not read a repository snapshot."), "",
				mutedStyle.Render("  r retries the refresh; q quits.")}
		}
		if len(m.Snapshot.Files) > 0 {
			return []string{"", mutedStyle.Render("  No file selected (or no search results)."), "",
				mutedStyle.Render("  Enter opens a file; Esc clears search.")}
		}
		return []string{"", accentStyle.Render("  Nothing left to stage."), "",
			mutedStyle.Render("  The working tree has no unstaged changes."),
			mutedStyle.Render("  Already-staged changes stay in your index."), "",
			mutedStyle.Render("  r refresh    ? help    q quit")}
	}
	result := []string{"", purpleStyle.Render("  " + safeText(f.PartialReason)), "",
		mutedStyle.Render("  Text hunk selection is unavailable for this change.")}
	if !f.Conflict {
		result = append(result, mutedStyle.Render("  f stages the entire file; the worktree is untouched."))
	}
	for _, line := range f.Header {
		result = append(result, "  "+safeText(line))
	}
	// Links and submodule pointers may have readable patch bodies even
	// though line-level staging is deliberately disabled for their modes.
	for hi, h := range f.Hunks {
		result = append(result, mutedStyle.Render("  "+safeText(h.HeaderLine())))
		for li, line := range h.Lines {
			code := m.code(numberedLine{ref: LineRef{hi, li}, line: line})
			result = append(result, "  "+changeSign(line.Kind)+" "+crop(code, m.Horizontal, m.Viewport.Width-4))
		}
	}
	return result
}

func (m Model) cursorRow() int {
	ref, ok := m.currentRef()
	if !ok {
		return -1
	}
	for i, row := range m.DiffRows {
		if row.Unit == m.Unit && row.Contains(ref) {
			return i
		}
	}
	return -1
}

func (m *Model) ensureDiffCursor() {
	row := m.cursorRow()
	if row < 0 {
		return
	}
	if row < m.Viewport.YOffset {
		m.Viewport.SetYOffset(max(0, row-1))
	} else if row >= m.Viewport.YOffset+m.Viewport.Height {
		m.Viewport.SetYOffset(row - m.Viewport.Height + 1)
	}
}

func (m Model) treeHeight() int {
	h := m.Viewport.Height
	if m.Searching || m.Tree.Query != "" {
		h--
	}
	return max(1, h)
}

func (m Model) renderTree() string {
	width, height := m.TreeWidth, m.Viewport.Height
	var lines []string
	for i := 0; i < m.treeHeight(); i++ {
		index := m.Tree.Offset + i
		if index >= len(m.Tree.Rows) {
			lines = append(lines, strings.Repeat(" ", width))
			continue
		}
		row := m.Tree.Rows[index]
		icon := "·"
		if row.Dir {
			icon = "▾"
			if m.Tree.Collapsed[row.Path] && m.Tree.Query == "" {
				icon = "▸"
			}
		} else if row.File.Conflict {
			icon = "!"
		} else if row.File.Untracked {
			icon = "?"
		} else if row.File.Binary {
			icon = "B"
		}
		cursor := " "
		if index == m.Tree.Cursor {
			cursor = "›"
		}
		label := cursor + strings.Repeat("  ", row.Depth) + icon + " " + displayPath(row.Name)
		stats := ""
		if row.Additions+row.Deletions != 0 {
			stats = addedStyle.Render(fmt.Sprintf(" +%d", row.Additions)) + removedStyle.Render(fmt.Sprintf(" −%d", row.Deletions))
		}
		// Leave room for stats, but avoid eating a deeply indented filename.
		if ansi.StringWidth(stats) > width/2 || row.Depth*2+8+ansi.StringWidth(stats) > width {
			stats = ""
		}
		text := joinEnds(label, stats, width)
		if index == m.Tree.Cursor {
			style := textStyle.Background(lipgloss.Color(colorPanel))
			if m.Focus == focusTree {
				style = style.Foreground(lipgloss.Color(colorAccent)).Bold(true)
			}
			text = style.Render(text)
		}
		lines = append(lines, text)
	}
	if m.Searching {
		lines = append(lines, fit(m.Search.View(), width))
	} else if m.Tree.Query != "" {
		lines = append(lines, fit(mutedStyle.Render("/"+displayPath(m.Tree.Query)+"  Esc clear"), width))
	}
	return strings.Join(lines[:min(height, len(lines))], "\n")
}

func (m Model) modeLabel() string {
	if m.effectiveSideBySide() {
		return "split view"
	}
	if m.SideBySide {
		return "unified · narrow"
	}
	return "unified"
}

func (m Model) helpView() string {
	lines := []string{
		"PATCHTREE / KEYBINDINGS", "",
		"Navigation                     Staging (diff focus)",
		"  Tab       change focus          y       stage hunk / marked lines",
		"  j/k ↑/↓   tree or diff lines     n       skip hunk, advance",
		"  Enter     open file / marks     p [ ]   previous / next hunk",
		"  h/l ←/→   collapse / expand      s       split at context gaps",
		"  n/p       next/prev file (tree)  e       edit patch in $EDITOR",
		"  N/P       next/prev file (any)   a       stage current + remaining",
		"  /         filename search       d       skip current + remaining",
		"  Esc       cancel search/visual  f       stage WHOLE file + mode",
		"  s (tree)  toggle diff layout",
		"  S (any)   toggle diff layout   Line selection (diff focus)",
		"  Ctrl-d/u  half-page scroll       V       enter/leave visual range",
		"  h/l       pan diff sideways      J/K     extend range down/up",
		"  ←/→       old/new side (split)    Space   mark/unmark changed lines",
		"  r         refresh               y/Enter apply marked lines",
		"  q Ctrl-c  quit                  u       clear all line marks", "",
		"Space only queues marks. Nothing reaches the index until y/Enter.",
		"Skipping never discards changes. q keeps already-staged changes.",
		"Binary, links, submodules and mode-only changes use f, not y.", "",
		"? or Esc closes help.  Keys n and s depend on the focused pane.",
	}
	width := min(76, m.Width-4)
	var clipped []string
	for _, line := range lines {
		clipped = append(clipped, fit(line, width))
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(colorAccent)).
		Padding(1, 1).Render(strings.Join(clipped, "\n"))
	if ansi.StringWidth(strings.Split(box, "\n")[0]) > m.Width || lipgloss.Height(box) > m.Height-2 {
		// A paginated help viewport is used on smaller terminals.
		start := min(m.HelpOffset, max(0, len(clipped)-(m.Height-5)))
		end := min(len(clipped), start+max(1, m.Height-5))
		box = accentStyle.Render("KEYBINDINGS · j/k scroll · ?/Esc close") + "\n\n" + strings.Join(clipped[start:end], "\n")
	}
	return lipgloss.Place(m.Width, m.Height, lipgloss.Center, lipgloss.Center, box)
}

func (m Model) View() string {
	if m.Quitting {
		return ""
	}
	if m.Width < 64 || m.Height < 12 {
		return fmt.Sprintf("Patchtree needs a terminal of at least 64×12.\nCurrent: %d×%d. Resize, or press q to quit.", m.Width, m.Height)
	}
	if m.Help {
		return m.helpView()
	}
	watch := ""
	if m.Watch {
		watch = accentStyle.Render("  ◉ watch")
	}
	backend := "delta"
	if m.Delta == "" || m.DeltaFailed {
		backend = "basic renderer"
	}
	header := joinEnds(accentStyle.Render(" ◆ PATCHTREE")+mutedStyle.Render(" / "+displayPath(filepath.Base(m.Repo.Root)))+watch,
		mutedStyle.Render(backend+" · "+m.modeLabel()+" "), m.Width)
	divider := mutedStyle.Render(strings.Repeat("─", m.Width))
	treeTitle := fmt.Sprintf(" UNSTAGED · %d", len(m.Snapshot.Files))
	diffTitle := " DIFF"
	if f := m.activeFile(); f != nil {
		diffTitle += " · " + displayPath(f.Path)
	}
	titleStyleTree, titleStyleDiff := mutedStyle, mutedStyle
	if m.Focus == focusTree {
		titleStyleTree = accentStyle
	} else {
		titleStyleDiff = accentStyle
	}
	left := titleStyleTree.Render(fit(treeTitle, m.TreeWidth)) + "\n" + m.renderTree()
	viewLines := strings.Split(m.Viewport.View(), "\n")
	for i := range viewLines {
		viewLines[i] = fit(viewLines[i], m.Viewport.Width)
	}
	right := titleStyleDiff.Render(fit(diffTitle, m.Viewport.Width)) + "\n" + strings.Join(viewLines, "\n")
	leftLines, rightLines := strings.Split(left, "\n"), strings.Split(right, "\n")
	body := make([]string, m.Viewport.Height+1)
	for i := range body {
		l, r := "", ""
		if i < len(leftLines) {
			l = leftLines[i]
		}
		if i < len(rightLines) {
			r = rightLines[i]
		}
		body[i] = fit(l, m.TreeWidth) + mutedStyle.Render("│") + fit(r, m.Viewport.Width)
	}
	location := " clean working tree"
	if !m.Loaded {
		location = " waiting for repository snapshot"
	} else if len(m.Snapshot.Files) > 0 && m.activeFile() == nil {
		location = " no file selected"
	}
	if f := m.activeFile(); f != nil {
		location = " " + displayPath(f.Path)
		if len(m.Units) > 0 {
			location += fmt.Sprintf(" — hunk %d of %d", m.Unit+1, len(m.Units))
		}
	}
	if len(m.Marks) > 0 {
		location += fmt.Sprintf(" · %d marked → y", len(m.Marks))
	}
	if m.Visual {
		location += " · VISUAL"
	}
	if m.Busy != "" {
		location += " · " + m.Busy + "…"
	} else if m.Reloading {
		location += " · refreshing…"
	}
	fileWord := "files"
	if m.Snapshot.StagedFiles == 1 {
		fileWord = "file"
	}
	staged := addedStyle.Bold(true).Render(fmt.Sprintf(" %d %s staged ", m.Snapshot.StagedFiles, fileWord))
	status := joinEnds(textStyle.Render(location), staged, m.Width)
	hint := " Tab focus · Enter open · n/p files · s layout · ? help · q quit"
	if m.Focus == focusDiff {
		hint = " y stage · n skip · s split hunk · S layout · V select · ? help · q quit"
	}
	if m.Notice != "" {
		style := accentStyle
		if m.NoticeError {
			style = removedStyle
		}
		hint = " " + style.Render(safeText(m.Notice))
		hint = joinEnds(hint, mutedStyle.Render("  ? help "), m.Width)
	}
	return header + "\n" + divider + "\n" + strings.Join(body, "\n") + "\n" + divider + "\n" + status + "\n" + fit(hint, m.Width)
}
