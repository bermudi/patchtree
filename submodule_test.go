package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubmoduleDirtyContentsAreNotClaimedStaged(t *testing.T) {
	r := testRepo(t)
	child := &Repository{Root: filepath.Join(r.Root, "sub")}
	if err := os.Mkdir(child.Root, 0755); err != nil {
		t.Fatal(err)
	}
	testGit(t, child, "init", "-q")
	testGit(t, child, "config", "user.name", "Test")
	testGit(t, child, "config", "user.email", "test@example.invalid")
	testGit(t, child, "config", "commit.gpgsign", "false")
	testGit(t, child, "config", "core.hooksPath", filepath.Join(child.Root, "no-hooks"))
	writeWorktree(t, child, "f", "old\n")
	commitAll(t, child)
	commitAll(t, r)
	// Presentation settings must not cause nested/log-style diffs to enter
	// the two-way parser.
	testGit(t, r, "config", "diff.submodule", "log")
	writeWorktree(t, child, "f", "dirty\n")
	f := diffFile(t, r, "sub")
	if !strings.Contains(f.PartialReason, "submodule") {
		t.Fatal("submodule was exposed as a text hunk")
	}
	msg := runStageCmd(r, stageRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: true})().(stagedMsg)
	if msg.Applied || msg.Err == nil || !msg.HaveSnapshot {
		t.Fatal("a successful Git no-op was claimed to stage nested contents")
	}
	assertIndex(t, child, "f", "old\n")
	assertWorktree(t, child, "f", "dirty\n")
	if snapshot(t, r).StagedFiles != 0 {
		t.Fatal("dirty-only pointer was added to the parent index")
	}
	commitAll(t, child)
	f = diffFile(t, r, "sub")
	msg = runStageCmd(r, stageRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: true})().(stagedMsg)
	if !msg.Applied || msg.Err != nil || msg.Snapshot.StagedFiles != 1 {
		t.Fatalf("recorded commit was not staged: %+v", msg)
	}
}

func TestWholeOnlyTextBodyStillHasPreview(t *testing.T) {
	raw := "diff --git a/link b/link\nindex 1111111..2222222 120000\n--- a/link\n+++ b/link\n@@ -1 +1 @@\n-old-target\n\\ No newline at end of file\n+new-target\n\\ No newline at end of file\n"
	files, err := ParseDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	classifyFile(files[0])
	m := NewModel(&Repository{Root: "/unused"}, Options{NoDelta: true})
	updated, _ := m.Update(refreshMsg{Snapshot: Snapshot{Files: files}})
	m = updated.(Model)
	view := m.View()
	if len(m.Units) != 0 || !strings.Contains(view, "old-target") || !strings.Contains(view, "new-target") {
		t.Fatal("whole-file-only protection hid the readable diff")
	}
}

func TestEmptyFilenameFilterDoesNotClaimCleanWorktree(t *testing.T) {
	m := renderModel(t)
	m.Tree.Query = "no-matching-filename"
	m.Tree.Rebuild(m.Snapshot.Files)
	m.activateFile("", -1)
	if strings.Contains(m.View(), "clean working tree") || !strings.Contains(m.View(), "no file selected") {
		t.Fatal("an empty filtered tree was incorrectly called clean")
	}
}
