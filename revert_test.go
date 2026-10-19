package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func revertSelection(t *testing.T, r *Repository, f *FileDiff, selected map[LineRef]bool) string {
	t.Helper()
	patchText, err := BuildRevertPatch(f, selected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDiff(patchText); err != nil {
		t.Fatalf("generated an invalid revert patch: %v\n%s", err, patchText)
	}
	if err := r.ApplyRevertPatch(context.Background(), patchText); err != nil {
		t.Fatalf("%v\n%s", err, patchText)
	}
	return patchText
}

func finishRevert(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected an asynchronous revert command")
	}
	msg, ok := cmd().(revertedMsg)
	if !ok {
		t.Fatal("command did not return revertedMsg")
	}
	if msg.Err != nil {
		t.Fatal(msg.Err)
	}
	updated, _ := m.Update(msg)
	m = updated.(Model)
	if m.Busy != "" || m.Quitting {
		t.Fatal("revert exited or left the model busy")
	}
	return m
}

// The mirror of TestPartialReplacement: reverting selected lines must restore
// the worktree to the index, leaving the index itself untouched.
func TestRevertPartialReplacements(t *testing.T) {
	base := "alpha\nbeta\ngamma\n"
	worktree := "alpha\nBETTER\nextra\ngamma\n"
	cases := []struct {
		name       string
		selectLine func(DiffLine) bool
		want       string
	}{
		{"full", func(DiffLine) bool { return true }, base},
		{"additions_only", func(l DiffLine) bool { return l.Kind == '+' }, "alpha\ngamma\n"},
		{"deletion_only", func(l DiffLine) bool { return l.Kind == '-' }, "alpha\nbeta\nBETTER\nextra\ngamma\n"},
		{"one_addition", func(l DiffLine) bool { return l.Text == "extra" }, "alpha\nBETTER\ngamma\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRepo(t)
			writeWorktree(t, r, "src/main.go", base)
			commitAll(t, r)
			writeWorktree(t, r, "src/main.go", worktree)
			f := diffFile(t, r, "src/main.go")
			revertSelection(t, r, f, chooseLines(f, tc.selectLine))
			assertWorktree(t, r, "src/main.go", tc.want)
			assertIndex(t, r, "src/main.go", base)
		})
	}
}

// Three hunks exercise the projected new-side coordinates: reverting an early
// hunk changes the file length seen by later reverted hunks.
func TestRevertMultiHunkCoordinates(t *testing.T) {
	base := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11\nl12\nl13\nl14\nl15\nl16\nl17\nl18\nl19\nl20\nl21\nl22\nl23\nl24\nl25\n"
	changed := "l1\nX2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11\nl12\nl13\nl15\nl16\nl17\nl18\nl19\nl20\nl21\nl22\nl23\nl24\nl25\nl26\nl27\n"
	cases := []struct {
		name     string
		selected func(DiffLine) bool
		want     string
	}{
		{
			"everything",
			func(DiffLine) bool { return true },
			base,
		},
		{
			"late_additions_only",
			func(l DiffLine) bool { return l.Text == "l26" || l.Text == "l27" },
			"l1\nX2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11\nl12\nl13\nl15\nl16\nl17\nl18\nl19\nl20\nl21\nl22\nl23\nl24\nl25\n",
		},
		{
			"middle_deletion_only",
			func(l DiffLine) bool { return l.Text == "l14" },
			"l1\nX2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11\nl12\nl13\nl14\nl15\nl16\nl17\nl18\nl19\nl20\nl21\nl22\nl23\nl24\nl25\nl26\nl27\n",
		},
		{
			// The middle hunk restores one line (+1) before the late hunk
			// removes two; only correct relative coordinates apply both.
			"middle_and_late",
			func(l DiffLine) bool { return l.Text == "l14" || l.Text == "l26" || l.Text == "l27" },
			"l1\nX2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11\nl12\nl13\nl14\nl15\nl16\nl17\nl18\nl19\nl20\nl21\nl22\nl23\nl24\nl25\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRepo(t)
			writeWorktree(t, r, "numbers.txt", base)
			commitAll(t, r)
			writeWorktree(t, r, "numbers.txt", changed)
			f := diffFile(t, r, "numbers.txt")
			if len(f.Hunks) != 3 {
				t.Fatalf("wanted 3 hunks, got %d", len(f.Hunks))
			}
			revertSelection(t, r, f, chooseLines(f, tc.selected))
			assertWorktree(t, r, "numbers.txt", tc.want)
			assertIndex(t, r, "numbers.txt", base)
		})
	}
}

// A deterministic property test mirroring the staging one: random subsets of a
// three-hunk diff must revert the worktree to exactly the independently
// projected content, never touching the index.
func TestRandomRevertSelectionsMatchWorktreeProjection(t *testing.T) {
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
	rng := rand.New(rand.NewSource(9751))
	work := strings.Split(strings.TrimSuffix(changed.String(), "\n"), "\n")
	for iteration := 0; iteration < 100; iteration++ {
		writeWorktree(t, r, "random.txt", changed.String()) // Reset ONLY the fixture's worktree.
		selected := chooseLines(f, func(DiffLine) bool { return rng.Intn(2) == 1 })
		var want []string
		pos := 0
		for hi, h := range f.Hunks {
			start := h.NewStart
			if h.NewLines != 0 {
				start--
			}
			want = append(want, work[pos:start]...)
			pos = start
			for li, line := range h.Lines {
				switch line.Kind {
				case ' ':
					want = append(want, work[pos])
					pos++
				case '+':
					if !selected[LineRef{hi, li}] {
						want = append(want, work[pos])
					}
					pos++
				case '-':
					if selected[LineRef{hi, li}] {
						want = append(want, line.Text)
					}
				}
			}
		}
		want = append(want, work[pos:]...)
		patchText, err := BuildRevertPatch(f, selected)
		if errors.Is(err, ErrNoChanges) {
			continue
		}
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		if err := r.ApplyRevertPatch(context.Background(), patchText); err != nil {
			t.Fatalf("iteration %d: %v\n%s", iteration, err, patchText)
		}
		assertWorktree(t, r, "random.txt", strings.Join(want, "\n")+"\n")
	}
	assertIndex(t, r, "random.txt", original.String())
}

func TestRevertNoNewlineAndCRLF(t *testing.T) {
	cases := []struct {
		name, old, worktree, want string
		selectLine                func(DiffLine) bool
		reject                    bool
	}{
		{"no_newline_full", "end", "replacement", "end", func(DiffLine) bool { return true }, false},
		{"no_newline_restore_only", "end", "replacement", "replacement", func(l DiffLine) bool { return l.Kind == '-' }, true},
		{"no_newline_context", "old\nlast", "new\nlast", "old\nlast", func(DiffLine) bool { return true }, false},
		{"no_newline_remove_addition", "before\nend", "before\nreplacement", "before\n", func(l DiffLine) bool { return l.Kind == '+' }, false},
		{"crlf", "first\r\nold\r\nlast\r\n", "first\r\nnew\r\nlast\r\n", "first\r\nold\r\nlast\r\n", func(DiffLine) bool { return true }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRepo(t)
			writeWorktree(t, r, "eof.txt", tc.old)
			commitAll(t, r)
			writeWorktree(t, r, "eof.txt", tc.worktree)
			f := diffFile(t, r, "eof.txt")
			selected := chooseLines(f, tc.selectLine)
			if tc.reject {
				if _, err := BuildRevertPatch(f, selected); err == nil || !strings.Contains(err.Error(), "no-newline") {
					t.Fatalf("wanted a safe EOF rejection, got %v", err)
				}
			} else {
				revertSelection(t, r, f, selected)
			}
			assertWorktree(t, r, "eof.txt", tc.want)
			assertIndex(t, r, "eof.txt", tc.old)
		})
	}
}

func TestRevertGuardsAndWholeFile(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "kept.txt", "old\n")
	commitAll(t, r)

	// Untracked files have no index version: both forms must refuse.
	writeWorktree(t, r, "new.txt", "content\n")
	f := diffFile(t, r, "new.txt")
	if _, err := BuildRevertPatch(f, chooseLines(f, func(DiffLine) bool { return true })); err == nil {
		t.Fatal("untracked file accepted a partial revert")
	}
	msg := runRevertCmd(r, revertRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: true})().(revertedMsg)
	if msg.Applied || msg.Err == nil {
		t.Fatalf("untracked whole-file revert was not refused: %+v", msg)
	}
	assertWorktree(t, r, "new.txt", "content\n")

	// A deleted worktree file can only be restored whole.
	writeWorktree(t, r, "gone.txt", "one\ntwo\nthree\n")
	commitAll(t, r)
	if err := os.Remove(filepath.Join(r.Root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	f = diffFile(t, r, "gone.txt")
	if !f.Deleted {
		t.Fatal("fixture is not a deleted file")
	}
	if _, err := BuildRevertPatch(f, chooseLines(f, func(DiffLine) bool { return true })); err == nil {
		t.Fatal("deleted file accepted a partial revert")
	}
	msg = runRevertCmd(r, revertRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: true})().(revertedMsg)
	if !msg.Applied || msg.Err != nil {
		t.Fatalf("whole-file restore failed: %+v", msg)
	}
	assertWorktree(t, r, "gone.txt", "one\ntwo\nthree\n")
	if s := snapshot(t, r); s.File("gone.txt") != nil {
		t.Fatal("restored file still appears unstaged")
	}

	// A binary change rejects partial reverts but restores whole.
	writeWorktree(t, r, "data.bin", "old\x00bytes")
	commitAll(t, r)
	writeWorktree(t, r, "data.bin", "new\x00bytes")
	f = diffFile(t, r, "data.bin")
	if !f.Binary {
		t.Fatal("fixture is not binary")
	}
	if _, err := BuildRevertPatch(f, chooseLines(f, func(DiffLine) bool { return true })); err == nil {
		t.Fatal("binary file accepted a partial revert")
	}
	msg = runRevertCmd(r, revertRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: true})().(revertedMsg)
	if !msg.Applied || msg.Err != nil {
		t.Fatalf("binary whole-file revert failed: %+v", msg)
	}
	assertWorktree(t, r, "data.bin", "old\x00bytes")

	// A mode-only change restores content and mode from the index.
	if runtime.GOOS != "windows" {
		writeWorktree(t, r, "script", "body\n")
		commitAll(t, r)
		if err := os.Chmod(filepath.Join(r.Root, "script"), 0755); err != nil {
			t.Fatal(err)
		}
		f = diffFile(t, r, "script")
		if f.PartialReason == "" {
			t.Fatal("mode-only change was not classified")
		}
		msg = runRevertCmd(r, revertRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: true})().(revertedMsg)
		if !msg.Applied || msg.Err != nil {
			t.Fatalf("mode-only whole-file revert failed: %+v", msg)
		}
		info, err := os.Stat(filepath.Join(r.Root, "script"))
		if err != nil || info.Mode().Perm() != 0644 {
			t.Fatalf("mode was not restored: %v %v", info.Mode().Perm(), err)
		}
	}

	// Unmerged paths stay protected.
	writeWorktree(t, r, "conflict.txt", "base\n")
	commitAll(t, r)
	branch := strings.TrimSpace(testGit(t, r, "branch", "--show-current"))
	testGit(t, r, "checkout", "-qb", "other")
	writeWorktree(t, r, "conflict.txt", "other\n")
	commitAll(t, r)
	testGit(t, r, "checkout", "-q", branch)
	writeWorktree(t, r, "conflict.txt", "current\n")
	commitAll(t, r)
	if _, err := r.git(context.Background(), "merge", "--no-edit", "other"); err == nil {
		t.Fatal("expected merge conflict")
	}
	f = diffFile(t, r, "conflict.txt")
	msg = runRevertCmd(r, revertRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: true})().(revertedMsg)
	if msg.Applied || msg.Err == nil {
		t.Fatal("whole-file revert resolved an unmerged path")
	}
}

func TestStalePreviewRefusesToRevert(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "file", "old\n")
	commitAll(t, r)
	writeWorktree(t, r, "file", "preview\n")
	f := diffFile(t, r, "file")
	patchText, err := BuildRevertPatch(f, chooseLines(f, func(DiffLine) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	writeWorktree(t, r, "file", "changed while previewing\n")
	msg := runRevertCmd(r, revertRequest{Path: f.Path, Expected: f.Fingerprint, Patch: patchText})().(revertedMsg)
	if msg.Err == nil || msg.Applied || !msg.HaveSnapshot {
		t.Fatalf("bad stale result: %+v", msg)
	}
	assertWorktree(t, r, "file", "changed while previewing\n")
	assertIndex(t, r, "file", "old\n")
}

func TestRevertTrackedCRLFWithAutocrlf(t *testing.T) {
	r := testRepo(t)
	testGit(t, r, "config", "core.autocrlf", "true")
	writeWorktree(t, r, "f", "first\r\nsecond\r\nthird\r\n")
	commitAll(t, r)
	writeWorktree(t, r, "f", "first\r\nCHANGED\r\nthird\r\n")
	f := diffFile(t, r, "f")
	for _, line := range f.Hunks[0].Lines {
		if strings.HasSuffix(line.Text, "\r") {
			t.Fatal("git diff --no-index did not normalize the tracked file")
		}
	}
	revertSelection(t, r, f, chooseLines(f, func(DiffLine) bool { return true }))
	assertWorktree(t, r, "f", "first\r\nsecond\r\nthird\r\n")
}

// Intent-to-add entries carry a "new file" diff against the index, but the
// index stores no content for them: reverting would silently empty the file,
// so they are refused just like untracked files.
func TestRevertIntentToAddIsRefused(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "base.txt", "base\n")
	commitAll(t, r)
	writeWorktree(t, r, "n.txt", "one\ntwo\n")
	testGit(t, r, "add", "-N", "--", "n.txt")
	f := diffFile(t, r, "n.txt")
	if !f.New || f.Untracked {
		t.Fatalf("expected an intent-to-add classification: %+v", f)
	}
	if _, err := BuildRevertPatch(f, chooseLines(f, func(DiffLine) bool { return true })); err == nil {
		t.Fatal("intent-to-add file accepted a partial revert")
	}
	msg := runRevertCmd(r, revertRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: true})().(revertedMsg)
	if msg.Applied || msg.Err == nil {
		t.Fatalf("intent-to-add whole-file revert was not refused: %+v", msg)
	}
	assertWorktree(t, r, "n.txt", "one\ntwo\n")
}

func TestPartialRevertLeavesRemainingChangeStageable(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "f", "one\nold\nlast\n")
	commitAll(t, r)
	writeWorktree(t, r, "f", "one\nnew1\nnew2\nlast\n")
	f := diffFile(t, r, "f")
	revertSelection(t, r, f, chooseLines(f, func(l DiffLine) bool { return l.Text == "new2" }))
	assertWorktree(t, r, "f", "one\nnew1\nlast\n")
	f = diffFile(t, r, "f")
	applySelection(t, r, f, chooseLines(f, func(DiffLine) bool { return true }))
	assertIndex(t, r, "f", "one\nnew1\nlast\n")
	assertWorktree(t, r, "f", "one\nnew1\nlast\n")
}

func TestRevertConfirmationFlowAndShortcuts(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "f", "one\nold\nlast\n")
	commitAll(t, r)
	writeWorktree(t, r, "f", "one\nnew\nlast\n")
	m := loadedModel(t, r)
	m, _ = press(m, "enter")
	if m.Focus != focusDiff {
		t.Fatal("Enter did not focus the diff")
	}

	// R must only ask; canceling with any non-confirming key changes nothing.
	m, _ = press(m, "R")
	if m.Pending == nil || !strings.Contains(m.Pending.Label, "hunk 1/1 of f") {
		t.Fatalf("R did not prepare a confirmation: %+v", m.Pending)
	}
	if !strings.Contains(m.View(), "will be lost") {
		t.Fatal("confirmation prompt is not visible")
	}
	m, _ = press(m, "q")
	if m.Quitting || m.Pending != nil {
		t.Fatal("q did not cancel the pending revert")
	}
	assertWorktree(t, r, "f", "one\nnew\nlast\n")
	assertIndex(t, r, "f", "one\nold\nlast\n")

	// Confirming reverts the worktree; a very busy model refuses q.
	m, _ = press(m, "R")
	m, cmd := press(m, "y")
	if m.Busy != "reverting" || cmd == nil {
		t.Fatal("model did not enter reverting state")
	}
	newer, quit := press(m, "q")
	if newer.Quitting || quit != nil {
		t.Fatal("quit raced a worktree write")
	}
	m = finishRevert(t, m, cmd)
	assertWorktree(t, r, "f", "one\nold\nlast\n")
	assertIndex(t, r, "f", "one\nold\nlast\n")
	if m.activeFile() != nil || len(m.Snapshot.Files) != 0 {
		t.Fatal("fully reverted file did not leave the tree")
	}
	if !strings.Contains(m.Notice, "Reverted hunk 1/1 of f") {
		t.Fatalf("misleading notice after revert: %q", m.Notice)
	}

	// Marked lines revert alone; R on an untracked file is refused.
	writeWorktree(t, r, "f", "one\nnew1\nnew2\nlast\n")
	updated, _ := m.Update(readSnapshotCmd(r, m.Epoch)())
	m = updated.(Model)
	m, _ = press(m, "enter")
	m, _ = press(m, "j")
	m, _ = press(m, "j")
	if ref, ok := m.currentRef(); !ok || m.activeFile().Hunks[ref.Hunk].Lines[ref.Line].Text != "new2" {
		t.Fatalf("cursor not on new2: %v %v", ref, ok)
	}
	m, _ = press(m, " ")
	if len(m.Marks) != 1 {
		t.Fatal("Space did not mark the line")
	}
	m, _ = press(m, "R")
	if m.Pending == nil || !strings.Contains(m.Pending.Label, "1 marked line in f") {
		t.Fatalf("marks were not carried into the confirmation: %+v", m.Pending)
	}
	m, cmd = press(m, "y")
	m = finishRevert(t, m, cmd)
	assertWorktree(t, r, "f", "one\nnew1\nlast\n")
	assertIndex(t, r, "f", "one\nold\nlast\n")
	if len(m.Marks) != 0 || m.Visual {
		t.Fatal("marks survived a revert")
	}

	// X reverts every unstaged change in the file.
	m, _ = press(m, "X")
	if m.Pending == nil || !strings.Contains(m.Pending.Label, "all unstaged changes in f") {
		t.Fatalf("X did not prepare a whole-file confirmation: %+v", m.Pending)
	}
	m, cmd = press(m, "y")
	m = finishRevert(t, m, cmd)
	assertWorktree(t, r, "f", "one\nold\nlast\n")
	assertIndex(t, r, "f", "one\nold\nlast\n")

	// Untracked files cannot be reverted by either key.
	writeWorktree(t, r, "u.txt", "new\n")
	updated, _ = m.Update(readSnapshotCmd(r, m.Epoch)())
	m = updated.(Model)
	if m.Focus != focusTree {
		m, _ = press(m, "tab")
	}
	m.Tree.SelectFile(m.Snapshot.Files, "u.txt", true)
	m, _ = press(m, "enter")
	m, _ = press(m, "R")
	if m.Pending != nil || !strings.Contains(m.Notice, "Untracked") {
		t.Fatalf("R on an untracked file must explain, not prompt: %+v / %q", m.Pending, m.Notice)
	}
	m, _ = press(m, "X")
	if m.Pending != nil || !strings.Contains(m.Notice, "Untracked") {
		t.Fatalf("X on an untracked file must explain, not prompt: %+v / %q", m.Pending, m.Notice)
	}
	assertWorktree(t, r, "u.txt", "new\n")
}

func TestRevertRequestValidation(t *testing.T) {
	if _, err := BuildRevertPatch(nil, map[LineRef]bool{}); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("nil file: %v", err)
	}
	f := &FileDiff{Path: "f", Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Lines: []DiffLine{{Kind: ' ', Text: "context"}}}}}
	if _, err := BuildRevertPatch(f, map[LineRef]bool{{0, 0}: true}); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("context selection: %v", err)
	}
	if _, err := BuildRevertPatch(f, map[LineRef]bool{{5, 0}: true}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale selection: %v", err)
	}
	r := testRepo(t)
	if err := r.RestoreFile(context.Background(), "../outside"); err == nil {
		t.Fatal("unsafe path accepted for restore")
	}
}
