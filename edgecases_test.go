package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmptyTrackedFileTransitions(t *testing.T) {
	for _, adding := range []bool{true, false} {
		t.Run(fmt.Sprint(adding), func(t *testing.T) {
			r := testRepo(t)
			old, worktree := "", "content\n"
			if !adding {
				old, worktree = worktree, old
			}
			writeWorktree(t, r, "f", old)
			commitAll(t, r)
			writeWorktree(t, r, "f", worktree)
			f := diffFile(t, r, "f")
			if f.New || f.Deleted {
				t.Fatal("empty content transition was misclassified as file creation/deletion")
			}
			applySelection(t, r, f, chooseLines(f, func(DiffLine) bool { return true }))
			assertIndex(t, r, "f", worktree)
			assertWorktree(t, r, "f", worktree)
		})
	}
}

func TestIntentToAddIsPartiallyStageable(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "new.txt", "first\nsecond\n")
	testGit(t, r, "add", "-N", "--", "new.txt")
	f := diffFile(t, r, "new.txt")
	applySelection(t, r, f, chooseLines(f, func(l DiffLine) bool { return l.Text == "second" }))
	assertIndex(t, r, "new.txt", "second\n")
	assertWorktree(t, r, "new.txt", "first\nsecond\n")
}

func TestUntrackedCRLFUsesGitNormalization(t *testing.T) {
	r := testRepo(t)
	testGit(t, r, "config", "core.autocrlf", "true")
	writeWorktree(t, r, "new.txt", "first\r\nsecond\r\n")
	f := diffFile(t, r, "new.txt")
	for _, line := range f.Hunks[0].Lines {
		if strings.HasSuffix(line.Text, "\r") {
			t.Fatal("git diff --no-index did not normalize the file")
		}
	}
	applySelection(t, r, f, chooseLines(f, func(l DiffLine) bool { return l.Text == "first" }))
	assertIndex(t, r, "new.txt", "first\n")
	assertWorktree(t, r, "new.txt", "first\r\nsecond\r\n")
}

func TestEditedDeletedFileCanRetainLines(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "f", "first\nsecond\nlast\n")
	commitAll(t, r)
	if err := os.Remove(filepath.Join(r.Root, "f")); err != nil {
		t.Fatal(err)
	}
	f := diffFile(t, r, "f")
	base, err := BuildPatch(f, chooseLines(f, func(DiffLine) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(base, "-first\n", " first\n", 1)
	edited = strings.Replace(edited, "-last\n", " last\n", 1)
	normalized, err := ValidateEditedPatch(base, edited)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(normalized, "deleted file mode") {
		t.Fatal("editor retention left whole-file deletion metadata")
	}
	if err := r.ApplyPatch(context.Background(), normalized); err != nil {
		t.Fatal(err)
	}
	assertIndex(t, r, "f", "first\nlast\n")
}

func TestEmbeddedRepositoriesDoNotBreakPreview(t *testing.T) {
	r := testRepo(t)
	child := &Repository{Root: filepath.Join(r.Root, "embedded")}
	if err := os.Mkdir(child.Root, 0755); err != nil {
		t.Fatal(err)
	}
	testGit(t, child, "init", "-q")
	writeWorktree(t, r, "ordinary.txt", "preview\n")
	s := snapshot(t, r)
	f := s.File("embedded")
	if f == nil || f.PartialReason == "" || len(s.Files) != 2 {
		t.Fatalf("embedded repo broke preview: %+v", s)
	}
	if len(initialUnits(f)) != 0 {
		t.Fatal("embedded repo was exposed as a text patch")
	}
}

// A deterministic, repository-backed property test: random subsets of a
// three-hunk diff must produce exactly the projected index, not the worktree.
func TestRandomLineSelectionsMatchProjection(t *testing.T) {
	r := testRepo(t)
	var original, changed strings.Builder
	for i := 1; i <= 65; i++ {
		line := fmt.Sprintf("line%02d\n", i)
		original.WriteString(line)
		switch i {
		case 3:
			changed.WriteString("early1\nearly2\nearly3\n")
		case 4:
		case 29:
			changed.WriteString("middle1\nmiddle2\n")
		case 30:
		case 54:
			changed.WriteString("late1\nlate2\nlate3\nlate4\n")
		case 55:
		default:
			changed.WriteString(line)
		}
	}
	writeWorktree(t, r, "random.txt", original.String())
	commitAll(t, r)
	writeWorktree(t, r, "random.txt", changed.String())
	f := diffFile(t, r, "random.txt")
	rng := rand.New(rand.NewSource(7241))
	old := strings.Split(strings.TrimSuffix(original.String(), "\n"), "\n")
	for iteration := 0; iteration < 100; iteration++ {
		testGit(t, r, "read-tree", "HEAD") // Reset ONLY the fixture's index, never its worktree.
		selected := chooseLines(f, func(DiffLine) bool { return rng.Intn(2) == 1 })
		var want []string
		pos := 0
		for hi, h := range f.Hunks {
			start := h.OldStart
			if h.OldLines != 0 {
				start--
			}
			want = append(want, old[pos:start]...)
			pos = start
			for li, line := range h.Lines {
				switch line.Kind {
				case ' ':
					want = append(want, old[pos])
					pos++
				case '-':
					if !selected[LineRef{hi, li}] {
						want = append(want, old[pos])
					}
					pos++
				case '+':
					if selected[LineRef{hi, li}] {
						want = append(want, line.Text)
					}
				}
			}
		}
		want = append(want, old[pos:]...)
		patchText, err := BuildPatch(f, selected)
		if errors.Is(err, ErrNoChanges) {
			continue
		}
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		if err := r.ApplyPatch(context.Background(), patchText); err != nil {
			t.Fatalf("iteration %d: %v\n%s", iteration, err, patchText)
		}
		assertIndex(t, r, "random.txt", strings.Join(want, "\n")+"\n")
	}
	assertWorktree(t, r, "random.txt", changed.String())
}

func FuzzParseDiff(f *testing.F) {
	f.Add("diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-old\n+new\n")
	f.Add("diff --git a/f b/f\nnew file mode 100644\n--- /dev/null\n+++ b/f\n@@ -0,0 +1,1 @@\n+new\n")
	f.Add("malformed input")
	f.Fuzz(func(t *testing.T, raw string) {
		files, err := ParseDiff(raw)
		if err != nil {
			return
		}
		for _, file := range files {
			if !validRepoPath(file.Path) {
				t.Fatal("parser accepted unsafe path")
			}
			for _, h := range file.Hunks {
				old, newer := countLines(h.Lines)
				if old != h.OldLines || newer != h.NewLines {
					t.Fatal("parser accepted incorrect counts")
				}
			}
		}
	})
}
