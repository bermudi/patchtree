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
