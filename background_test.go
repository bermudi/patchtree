package main

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

var sgrForTest = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

func bgSequence(hex string) string {
	return "\x1b[" + termenv.RGBColor(hex).Sequence(true) + "m"
}

var (
	addedBgSeq   = bgSequence(colorAddedBg)
	removedBgSeq = bgSequence(colorRemovedBg)
)

// Model the terminal's background state at every printable column, rather
// than merely checking that a background escape occurs somewhere in a row.
func backgroundColumns(t *testing.T, text string) ([]string, string) {
	t.Helper()
	bg := ""
	var columns []string
	addText := func(s string) {
		for range ansi.StringWidth(s) {
			columns = append(columns, bg)
		}
	}
	end := 0
	for _, match := range sgrForTest.FindAllStringSubmatchIndex(text, -1) {
		addText(text[end:match[0]])
		params := strings.Split(text[match[2]:match[3]], ";")
		for i := 0; i < len(params); i++ {
			p := 0
			if params[i] != "" {
				var err error
				p, err = strconv.Atoi(params[i])
				if err != nil {
					t.Fatal(err)
				}
			}
			switch {
			case p == 0 || p == 49:
				bg = ""
			case p == 38 || p == 48:
				count := 2 // 38/48;5;index
				if i+1 < len(params) && params[i+1] == "2" {
					count = 4 // 38/48;2;r;g;b
				}
				if i+count >= len(params) {
					t.Fatalf("incomplete color sequence: %q", text[match[0]:match[1]])
				}
				if p == 48 {
					bg = strings.Join(params[i:i+count+1], ";")
				}
				i += count
			case p >= 40 && p <= 47 || p >= 100 && p <= 107:
				bg = strconv.Itoa(p)
			}
		}
		end = match[1]
	}
	addText(text[end:])
	return columns, bg
}

func assertBackground(t *testing.T, text string, width int, want string) {
	t.Helper()
	columns, endBG := backgroundColumns(t, text)
	if len(columns) != width {
		t.Fatalf("width=%d, want %d", len(columns), width)
	}
	for column, got := range columns {
		if got != want {
			t.Fatalf("background hole at column %d: got %q, want %q; row=%q", column, got, want, text)
		}
	}
	if endBG != "" {
		t.Fatalf("background leaks after row: %q", endBG)
	}
}

func TestBackgroundEveryColumn(t *testing.T) {
	forceColor(t)
	for _, backend := range []string{"fallback", "delta"} {
		t.Run(backend, func(t *testing.T) {
			m := renderModel(t)
			m.resize(100, 30)
			if backend == "delta" {
				delta, err := exec.LookPath("delta")
				if err != nil {
					t.Skip("delta not installed")
				}
				cache, err := highlightWithDelta(context.Background(), delta, t.TempDir(), m.activeFile())
				if err != nil {
					t.Fatal(err)
				}
				m.Colors[m.ActivePath] = cache
			}
			numbered := numberLines(m.activeFile())[0]
			for _, line := range numbered {
				for _, selection := range []string{"none", "marked", "visual"} {
					m.Marks = map[LineRef]bool{}
					m.Visual = selection == "visual"
					m.Unit, m.Anchor, m.Line = 0, line.ref.Line, line.ref.Line
					if selection == "marked" {
						m.Marks[line.ref] = true
					}
					want := ""
					switch {
					case selection != "none":
						want = strings.TrimSuffix(strings.TrimPrefix(bgSequence(colorVisual), "\x1b["), "m")
					case line.line.Kind == '+':
						want = strings.TrimSuffix(strings.TrimPrefix(addedBgSeq, "\x1b["), "m")
					case line.line.Kind == '-':
						want = strings.TrimSuffix(strings.TrimPrefix(removedBgSeq, "\x1b["), "m")
					}
					for _, pan := range []int{0, 8} {
						m.Horizontal = pan
						name := fmt.Sprintf("line=%d/%s/pan=%d", line.ref.Line, selection, pan)
						t.Run(name, func(t *testing.T) {
							assertBackground(t, m.unifiedRow(line, 0, 48, 3).Text, 48, want)
							assertBackground(t, m.sideCell(&line, line.line.Kind != '-', 32, 3), 32, want)
						})
					}
				}
			}
		})
	}
}

func TestBackgroundStateCheckerDetectsHole(t *testing.T) {
	columns, _ := backgroundColumns(t, "\x1b[48;2;1;2;3ma\x1b[0mb")
	if len(columns) != 2 || columns[0] != "48;2;1;2;3" || columns[1] != "" {
		t.Fatalf("checker missed reset hole: %v", columns)
	}
}

func TestBackgroundProfile(t *testing.T) {
	old := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	for _, profile := range []termenv.Profile{termenv.TrueColor, termenv.ANSI256, termenv.Ascii} {
		lipgloss.SetColorProfile(profile)
		m := renderModel(t)
		line := numberLines(m.activeFile())[0][3]
		expected, _ := backgroundColumns(t, addedBgStyle.Render("x"))
		assertBackground(t, m.unifiedRow(line, 0, 48, 3).Text, 48, expected[0])
	}
}
