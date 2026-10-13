package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func fixtureDiff(t *testing.T) *FileDiff {
	t.Helper()
	raw := "diff --git a/main.go b/main.go\nindex 1111111..2222222 100644\n--- a/main.go\n+++ b/main.go\n" +
		"@@ -1,5 +1,6 @@ func main\n package main\n \n-func old() {\n+func main() {\n+    println(\"世界\")\n     println(\"last\")\n }\n"
	files, err := ParseDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	return files[0]
}

func renderModel(t *testing.T) Model {
	t.Helper()
	f := fixtureDiff(t)
	m := NewModel(&Repository{Root: "/demo/project"}, Options{NoDelta: true})
	updated, _ := m.Update(refreshMsg{Snapshot: Snapshot{Files: []*FileDiff{f}, StagedFiles: 3}})
	return updated.(Model)
}

// forceColor switches lipgloss to truecolor for assertions on escape
// sequences; under the test runner's default Ascii profile styles render
// empty. Restored on cleanup so style-free tests stay style-free.
func forceColor(t *testing.T) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
}

func TestLayoutsStayInsideTerminalAndMapEverySourceLine(t *testing.T) {
	for _, side := range []bool{false, true} {
		for _, size := range [][2]int{{64, 12}, {80, 24}, {120, 32}, {160, 50}} {
			m := renderModel(t)
			m.SideBySide = side
			updated, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m = updated.(Model)
			lines := strings.Split(m.View(), "\n")
			if len(lines) != size[1] {
				t.Fatalf("side=%t %v: got %d rows", side, size, len(lines))
			}
			for i, line := range lines {
				if width := ansi.StringWidth(line); width != size[0] {
					t.Fatalf("side=%t %v row%d width=%d: %q", side, size, i, width, line)
				}
			}
			for hi, h := range m.activeFile().Hunks {
				for li := range h.Lines {
					ref, found := LineRef{hi, li}, false
					for _, row := range m.DiffRows {
						found = found || row.Contains(ref)
					}
					if !found {
						t.Fatalf("side=%t %v: missing source map for %v", side, size, ref)
					}
				}
			}
			if !strings.Contains(ansi.Strip(m.View()), "3 files staged") {
				t.Fatal("persistent staged count not visible")
			}
		}
	}
}

func TestSideBySideSelectsEitherSideOfReplacement(t *testing.T) {
	m := renderModel(t)
	m.resize(140, 30)
	m.SideBySide = true
	m.Focus = focusDiff
	m.rebuildDiff(true)
	ref, _ := m.currentRef()
	if m.activeFile().Hunks[ref.Hunk].Lines[ref.Line].Kind != '-' {
		t.Fatal("fixture should begin on a deletion")
	}
	m, _ = press(m, "right")
	ref, _ = m.currentRef()
	if m.activeFile().Hunks[ref.Hunk].Lines[ref.Line].Kind != '+' {
		t.Fatal("right arrow did not select the added side")
	}
	m, _ = press(m, " ")
	if !m.Marks[ref] || len(m.Marks) != 1 {
		t.Fatal("paired row marked the wrong source line")
	}
	m, _ = press(m, "left")
	left, _ := m.currentRef()
	if left == ref || m.activeFile().Hunks[left.Hunk].Lines[left.Line].Kind != '-' {
		t.Fatal("left arrow did not select the removed side")
	}
}

func TestTreeSortStatsCollapsingAndFiltering(t *testing.T) {
	files := []*FileDiff{
		{Path: "z.txt", Additions: 3, Deletions: 1},
		{Path: "src/z.go", Additions: 2, Deletions: 4},
		{Path: "src/nested/a.go", Additions: 5, Deletions: 2},
		{Path: "a.txt", Additions: 1},
	}
	tree := TreeState{Collapsed: map[string]bool{"src/nested": true}}
	tree.Rebuild(files)
	if tree.Rows[0].Path != "src" || tree.Rows[0].Additions != 7 || tree.Rows[0].Deletions != 6 {
		t.Fatalf("directory sort/stats: %+v", tree.Rows[0])
	}
	for _, row := range tree.Rows {
		if row.Path == "src/nested/a.go" {
			t.Fatal("collapsed child was visible")
		}
	}
	tree.Query = "A.GO"
	tree.Rebuild(files)
	if len(tree.Rows) != 3 || tree.Rows[2].Path != "src/nested/a.go" || !tree.Collapsed["src/nested"] {
		t.Fatal("filter did not reveal matches without altering collapsed state")
	}
	tree.Query = ""
	tree.SelectFile(files, "src/z.go", true)
	cursor := tree.CursorKey
	tree.Rebuild(files)
	if tree.CursorKey != cursor {
		t.Fatal("tree refresh lost the cursor path")
	}
}

func TestTreeCanShowDeletedFileAndReplacementDirectory(t *testing.T) {
	rows := treeRows([]*FileDiff{{Path: "a", Deleted: true}, {Path: "a/new.txt", Untracked: true}}, nil, "")
	if len(rows) != 3 || !rows[0].Dir || rows[0].Key() == rows[2].Key() || rows[2].Dir {
		t.Fatalf("file/directory collision: %+v", rows)
	}
}

func TestChangedLinesCarryFullWidthBackground(t *testing.T) {
	forceColor(t)
	// Unified: every changed line owns a whole row.
	m := renderModel(t)
	m.resize(100, 30)
	f := m.activeFile()
	rows := m.buildDiffRows()
	rowFor := func(ref LineRef) (DiffRow, bool) {
		for _, r := range rows {
			if r.Contains(ref) {
				return r, true
			}
		}
		return DiffRow{}, false
	}
	checkTint := func(where string, kind byte, text string) {
		t.Helper()
		hasAdded := strings.Contains(text, addedBgSeq)
		hasRemoved := strings.Contains(text, removedBgSeq)
		switch kind {
		case '+':
			if !hasAdded || hasRemoved {
				t.Fatalf("%s: added line tint wrong: %q", where, text)
			}
		case '-':
			if !hasRemoved || hasAdded {
				t.Fatalf("%s: removed line tint wrong: %q", where, text)
			}
		default:
			if hasAdded || hasRemoved {
				t.Fatalf("%s: context line unexpectedly tinted: %q", where, text)
			}
		}
	}
	for hi, h := range f.Hunks {
		for li := range h.Lines {
			row, found := rowFor(LineRef{hi, li})
			if !found {
				t.Fatalf("unified: no row for %v", LineRef{hi, li})
			}
			checkTint("unified", h.Lines[li].Kind, row.Text)
		}
	}
	// Split: a '-' and its paired '+' share one row, so the tint belongs to
	// the cell, not the row. Assert per cell on the line's natural side.
	m.SideBySide = true
	m.resize(100, 30)
	numbered := numberLines(m.activeFile())
	for hi, h := range m.activeFile().Hunks {
		for li := range h.Lines {
			n := numbered[hi][li]
			cell := m.sideCell(&n, h.Lines[li].Kind != '-', 40, 4)
			checkTint("split", h.Lines[li].Kind, cell)
		}
	}
}

func TestChangeTintCoversPaddingAndSelectionWins(t *testing.T) {
	forceColor(t)
	m := renderModel(t)
	m.resize(100, 30)
	ref := LineRef{Hunk: 0, Line: 3} // '+func main() {'
	rows := m.buildDiffRows()
	for _, row := range rows {
		if !row.Contains(ref) {
			continue
		}
		// The tint is applied after padding, so it must reach the right edge:
		// background sequence first, padded plain text, single trailing reset.
		if !strings.HasPrefix(row.Text, addedBgSeq) {
			t.Fatalf("tint does not start the row: %q", row.Text)
		}
		if strings.HasSuffix(row.Text, addedBgSeq) {
			t.Fatalf("padding left untinted outside the styled span: %q", row.Text)
		}
		if trailing := row.Text[len(row.Text)-len("\x1b[0m"):]; trailing != "\x1b[0m" {
			t.Fatalf("row does not close its styles: %q", row.Text)
		}
	}
	// Visual selection replaces the change tint.
	m.Visual = true
	m.Unit = 0
	m.Line = 3
	for _, row := range m.buildDiffRows() {
		if !row.Contains(ref) {
			continue
		}
		if strings.Contains(row.Text, addedBgSeq) || !strings.Contains(row.Text, bgSequence(colorVisual)) {
			t.Fatalf("selection did not replace change tint: %q", row.Text)
		}
	}
}

func TestControlCharactersCannotEscapeIntoRendering(t *testing.T) {
	m := renderModel(t)
	f := m.activeFile()
	f.Path = "\x1b]0;evil\x07.go"
	m.ActivePath = f.Path
	f.Hunks[0].Lines[3].Text = "\x1b[2Jmalicious\r\x00\ttext"
	m.Tree.Rebuild(m.Snapshot.Files)
	m.rebuildDiff(false)
	view := ansi.Strip(m.View())
	if strings.Contains(view, "\x1b") || strings.Contains(view, "\x00") || strings.Contains(view, "\r") || strings.Contains(view, "\x07") {
		t.Fatal("raw terminal controls escaped sanitizer")
	}
	if !strings.Contains(view, `\x1b[2J`) {
		t.Fatal("unsafe code should be displayed visibly, not silently changed")
	}
}

func TestDeltaPreservesSourceMap(t *testing.T) {
	delta, err := exec.LookPath("delta")
	if err != nil {
		t.Skip("delta not installed; run with delta on PATH to exercise its integration")
	}
	f := fixtureDiff(t)
	highlighted, err := highlightWithDelta(context.Background(), delta, t.TempDir(), f)
	if err != nil {
		t.Fatal(err)
	}
	for hi, h := range f.Hunks {
		for li, line := range h.Lines {
			code, exists := highlighted.Lines[LineRef{hi, li}]
			if !exists || ansi.Strip(code) != safeText(line.Text) {
				t.Fatalf("wrong delta map for hunk %d line %d: %q", hi, li, code)
			}
		}
	}
	if !strings.Contains(highlighted.Lines[LineRef{0, 3}], "\x1b[") {
		t.Fatal("delta did not return syntax colors")
	}
}

func TestLongLineHorizontalClippingAndUnicode(t *testing.T) {
	m := renderModel(t)
	m.activeFile().Hunks[0].Lines[3].Text = strings.Repeat("世界", 100)
	m.Horizontal = 8
	m.resize(80, 24)
	for _, row := range m.DiffRows {
		if ansi.StringWidth(row.Text) != m.Viewport.Width {
			t.Fatal("long Unicode line overflowed diff pane")
		}
	}
}

func TestHelpAndSmallTerminal(t *testing.T) {
	m := renderModel(t)
	m.Help = true
	for _, size := range [][2]int{{64, 12}, {80, 24}, {120, 40}} {
		m.resize(size[0], size[1])
		if !strings.Contains(ansi.Strip(m.View()), "KEYBINDINGS") {
			t.Fatal("help was not visible")
		}
		for _, line := range strings.Split(m.View(), "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("help overflowed terminal")
			}
		}
	}
	m.Help = false
	m.resize(40, 8)
	if !strings.Contains(m.View(), "64×12") {
		t.Fatal("small terminal did not show the resize hint")
	}
}
