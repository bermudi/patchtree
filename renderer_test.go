package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDeltaInvocationNeverProbesTTYOrUsesPager(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake executable")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "delta")
	argsFile := filepath.Join(dir, "args")
	t.Setenv("PATCHTREE_TEST_ARGS", argsFile)
	// --color-only may legitimately return uncolored text. This fake backend
	// records arguments and preserves the input to test our mapping protocol.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$PATCHTREE_TEST_ARGS\"\ncat\n"
	if err := os.WriteFile(binary, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	f := fixtureDiff(t)
	if _, err := highlightWithDelta(context.Background(), binary, dir, f); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"--dark", "--color-only", "--no-gitconfig", "--paging=never", "--true-color=always"} {
		if !strings.Contains("\n"+string(data), "\n"+expected+"\n") {
			t.Fatalf("delta invocation is missing %s; args=%s", expected, data)
		}
	}
}

func TestTintSurvivesDeltaIntralineResets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake executable")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "delta")
	// Echo every line back colored, split at the midpoint with a hard reset
	// in between — the shape delta produces around intraline emphasis spans.
	// Stripped output equals the input, as the mapping protocol requires.
	script := "#!/bin/sh\nawk '{ n=int(length($0)/2); printf \"\\033[38;2;1;2;3m%s\\033[0m%s\\033[0m\\n\", substr($0,1,n), substr($0,n+1) }'\n"
	if err := os.WriteFile(binary, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	h, err := highlightWithDelta(context.Background(), binary, dir, fixtureDiff(t))
	if err != nil {
		t.Fatal(err)
	}
	// Hunk 0 lines: 0..1 and 5..6 context, 2 '-', 3..4 '+' ('+    println("世界")'
	// is line 4). '+func main() {' (14 runes) splits as '+func m' + 'ain() {',
	// marker then cut by the mapping protocol.
	added := h.Lines[LineRef{0, 3}]
	want := "\x1b[38;2;1;2;3mfunc m\x1b[0m" + addedBgSeq + "ain() {\x1b[0m" + addedBgSeq
	if added != want {
		t.Fatalf("added line tint broken:\n got %q\nwant %q", added, want)
	}
	if other := h.Lines[LineRef{0, 4}]; !strings.Contains(other, addedBgSeq) {
		t.Fatalf("second added line untinted: %q", other)
	}
	removed := h.Lines[LineRef{0, 2}]
	if !strings.Contains(removed, removedBgSeq) || strings.Contains(removed, addedBgSeq) {
		t.Fatalf("removed line tint wrong: %q", removed)
	}
	for _, li := range []int{0, 1, 5, 6} {
		ctx := h.Lines[LineRef{0, li}]
		if strings.Contains(ctx, addedBgSeq) || strings.Contains(ctx, removedBgSeq) {
			t.Fatalf("context line %d unexpectedly tinted: %q", li, ctx)
		}
	}
}

func TestChangedDeltaStructureIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake executable")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "delta")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho 'changed layout'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := highlightWithDelta(context.Background(), binary, dir, fixtureDiff(t)); err == nil {
		t.Fatal("a transformed diff could misalign staging; expected fallback error")
	}
}
