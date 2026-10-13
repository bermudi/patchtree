package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testRepo(t *testing.T) *Repository {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required for repository integration tests")
	}
	// An exported alternate Git directory/index must never redirect a test
	// fixture into the developer's real repository.
	for _, variable := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX", "GIT_CONFIG",
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES"} {
		key := variable
		old, exists := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if exists {
				os.Setenv(key, old)
			} else {
				os.Unsetenv(key)
			}
		})
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	r := &Repository{Root: t.TempDir()}
	testGit(t, r, "init", "-q")
	testGit(t, r, "config", "user.name", "Patchtree Test")
	testGit(t, r, "config", "user.email", "patchtree@example.invalid")
	testGit(t, r, "config", "core.autocrlf", "false")
	testGit(t, r, "config", "core.filemode", "true")
	testGit(t, r, "config", "commit.gpgsign", "false")
	testGit(t, r, "config", "core.hooksPath", filepath.Join(r.Root, "no-hooks"))
	return r
}

func testGit(t *testing.T, r *Repository, args ...string) string {
	t.Helper()
	out, err := r.git(context.Background(), args...)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return string(out)
}

func writeWorktree(t *testing.T, r *Repository, name, content string) {
	t.Helper()
	filename := filepath.Join(r.Root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, r *Repository) {
	t.Helper()
	testGit(t, r, "add", "-A", "--", ".")
	testGit(t, r, "commit", "-qm", "fixture")
}

func snapshot(t *testing.T, r *Repository) Snapshot {
	t.Helper()
	s, err := r.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func diffFile(t *testing.T, r *Repository, name string) *FileDiff {
	t.Helper()
	f := snapshot(t, r).File(name)
	if f == nil {
		t.Fatalf("no unstaged diff for %q", name)
	}
	return f
}

func chooseLines(f *FileDiff, predicate func(DiffLine) bool) map[LineRef]bool {
	result := map[LineRef]bool{}
	for hi, h := range f.Hunks {
		for li, line := range h.Lines {
			if line.Kind != ' ' && predicate(line) {
				result[LineRef{hi, li}] = true
			}
		}
	}
	return result
}

func applySelection(t *testing.T, r *Repository, f *FileDiff, selected map[LineRef]bool) string {
	t.Helper()
	patchText, err := BuildPatch(f, selected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDiff(patchText); err != nil {
		t.Fatalf("generated an invalid patch: %v\n%s", err, patchText)
	}
	if err := r.ApplyPatch(context.Background(), patchText); err != nil {
		t.Fatalf("%v\n%s", err, patchText)
	}
	return patchText
}

func assertIndex(t *testing.T, r *Repository, name, want string) {
	t.Helper()
	got := testGit(t, r, "show", ":"+name)
	if got != want {
		t.Fatalf("index content\n got %q\nwant %q", got, want)
	}
}

func assertWorktree(t *testing.T, r *Repository, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(r.Root, filepath.FromSlash(name)))
	if err != nil || string(got) != want {
		t.Fatalf("worktree was changed: content=%q err=%v; want %q", got, err, want)
	}
}

func TestPartialReplacement(t *testing.T) {
	cases := []struct {
		name       string
		selectLine func(DiffLine) bool
		want       string
	}{
		{"full", func(DiffLine) bool { return true }, "alpha\nBETTER\nextra\ngamma\n"},
		{"additions_only", func(l DiffLine) bool { return l.Kind == '+' }, "alpha\nbeta\nBETTER\nextra\ngamma\n"},
		{"deletion_only", func(l DiffLine) bool { return l.Kind == '-' }, "alpha\ngamma\n"},
		{"one_addition", func(l DiffLine) bool { return l.Text == "extra" }, "alpha\nbeta\nextra\ngamma\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRepo(t)
			writeWorktree(t, r, "src/main.go", "alpha\nbeta\ngamma\n")
			commitAll(t, r)
			worktree := "alpha\nBETTER\nextra\ngamma\n"
			writeWorktree(t, r, "src/main.go", worktree)
			f := diffFile(t, r, "src/main.go")
			applySelection(t, r, f, chooseLines(f, tc.selectLine))
			assertIndex(t, r, "src/main.go", tc.want)
			assertWorktree(t, r, "src/main.go", worktree)
		})
	}
}

func TestMultiHunkCoordinatesUseOnlySelectedDeltas(t *testing.T) {
	for _, earlyAdditions := range []bool{false, true} {
		t.Run(fmt.Sprint(earlyAdditions), func(t *testing.T) {
			r := testRepo(t)
			var old, changed, want strings.Builder
			for i := 1; i <= 60; i++ {
				line := fmt.Sprintf("line%02d\n", i)
				old.WriteString(line)
				switch i {
				case 3:
					changed.WriteString("earlyA\nearlyB\nearlyC\n")
				case 4:
				case 25:
					changed.WriteString("middle\n")
				default:
					changed.WriteString(line)
				}
				if i == 50 {
					changed.WriteString("lateExtra\n")
				}
				if i != 25 {
					want.WriteString(line)
					if i == 4 && earlyAdditions {
						want.WriteString("earlyA\nearlyB\nearlyC\n")
					}
				} else if !earlyAdditions {
					want.WriteString("middle\n")
				}
				if i == 50 {
					want.WriteString("lateExtra\n")
				}
			}
			writeWorktree(t, r, "numbers.txt", old.String())
			commitAll(t, r)
			writeWorktree(t, r, "numbers.txt", changed.String())
			f := diffFile(t, r, "numbers.txt")
			if len(f.Hunks) != 3 {
				t.Fatalf("wanted 3 hunks, got %d", len(f.Hunks))
			}
			selected := chooseLines(f, func(l DiffLine) bool {
				if earlyAdditions {
					return (l.Kind == '+' && strings.HasPrefix(l.Text, "early")) || l.Text == "line25" || l.Text == "lateExtra"
				}
				return l.Text == "line25" || l.Text == "middle" || l.Text == "lateExtra"
			})
			applySelection(t, r, f, selected)
			assertIndex(t, r, "numbers.txt", want.String())
			assertWorktree(t, r, "numbers.txt", changed.String())
		})
	}
}

func TestNewFilePartialThenRemaining(t *testing.T) {
	r := testRepo(t) // No HEAD and no index yet: an unborn repository is supported.
	writeWorktree(t, r, "new.txt", "one\ntwo\nthree\n")
	f := diffFile(t, r, "new.txt")
	if !f.Untracked || !f.New {
		t.Fatal("new file was not marked untracked")
	}
	applySelection(t, r, f, chooseLines(f, func(l DiffLine) bool { return l.Text == "two" }))
	assertIndex(t, r, "new.txt", "two\n")
	f = diffFile(t, r, "new.txt")
	if f.Untracked || f.New {
		t.Fatal("partially staged file must now be diffed against the index")
	}
	applySelection(t, r, f, chooseLines(f, func(DiffLine) bool { return true }))
	assertIndex(t, r, "new.txt", "one\ntwo\nthree\n")
	assertWorktree(t, r, "new.txt", "one\ntwo\nthree\n")
	s := snapshot(t, r)
	if len(s.Files) != 0 || s.StagedFiles != 1 {
		t.Fatalf("final snapshot: %+v", s)
	}
}

func TestDeletedFilePartialAndFull(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprint(full), func(t *testing.T) {
			r := testRepo(t)
			writeWorktree(t, r, "gone.txt", "one\ntwo\nthree\n")
			commitAll(t, r)
			if err := os.Remove(filepath.Join(r.Root, "gone.txt")); err != nil {
				t.Fatal(err)
			}
			f := diffFile(t, r, "gone.txt")
			patchText := applySelection(t, r, f, chooseLines(f, func(l DiffLine) bool { return full || l.Text == "two" }))
			if full {
				if !strings.Contains(patchText, "deleted file mode") {
					t.Fatal("full deletion lost file metadata")
				}
				if _, err := r.git(context.Background(), "show", ":gone.txt"); err == nil {
					t.Fatal("fully deleted file is still in the index")
				}
			} else {
				if strings.Contains(patchText, "deleted file mode") {
					t.Fatal("partial deletion would incorrectly delete the entire file")
				}
				assertIndex(t, r, "gone.txt", "one\nthree\n")
			}
			if _, err := os.Stat(filepath.Join(r.Root, "gone.txt")); !os.IsNotExist(err) {
				t.Fatal("deleted worktree file was restored")
			}
		})
	}
}

func TestSplitHunkSharedContext(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint(all), func(t *testing.T) {
			r := testRepo(t)
			writeWorktree(t, r, "split.txt", "zero\nA\ngap\nB\nfinal\n")
			commitAll(t, r)
			worktree := "zero\nNEW_A\ngap\nNEW_B\nfinal\n"
			writeWorktree(t, r, "split.txt", worktree)
			f := diffFile(t, r, "split.txt")
			units, err := splitUnit(f, initialUnits(f)[0])
			if err != nil || len(units) != 2 {
				t.Fatalf("split: %v, units=%v", err, units)
			}
			chosen := units
			if !all {
				chosen = units[:1]
			}
			applySelection(t, r, f, unitSelection(f, chosen))
			want := worktree
			if !all {
				want = "zero\nNEW_A\ngap\nB\nfinal\n"
			}
			assertIndex(t, r, "split.txt", want)
			assertWorktree(t, r, "split.txt", worktree)
		})
	}
}

func TestNoNewlineAndCRLF(t *testing.T) {
	cases := []struct {
		name, old, worktree, want string
		selectLine                func(DiffLine) bool
		reject                    bool
	}{
		{"no_newline_full", "end", "replacement", "replacement", func(DiffLine) bool { return true }, false},
		{"no_newline_delete", "before\nend", "before\nreplacement", "before\n", func(l DiffLine) bool { return l.Kind == '-' }, false},
		{"no_newline_unsafe_add", "end", "replacement", "end", func(l DiffLine) bool { return l.Kind == '+' }, true},
		{"no_newline_context", "old\nlast", "new\nlast", "new\nlast", func(DiffLine) bool { return true }, false},
		{"crlf", "first\r\nold\r\nlast\r\n", "first\r\nnew\r\nlast\r\n", "first\r\nnew\r\nlast\r\n", func(DiffLine) bool { return true }, false},
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
				if _, err := BuildPatch(f, selected); err == nil || !strings.Contains(err.Error(), "no-newline") {
					t.Fatalf("wanted a safe EOF rejection, got %v", err)
				}
			} else {
				applySelection(t, r, f, selected)
			}
			assertIndex(t, r, "eof.txt", tc.want)
			assertWorktree(t, r, "eof.txt", tc.worktree)
		})
	}
}

func TestExistingStagedContentAndReadOnlyPreview(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "file.txt", "base\n")
	commitAll(t, r)
	writeWorktree(t, r, "file.txt", "base\nstaged\n")
	testGit(t, r, "add", "--", "file.txt")
	writeWorktree(t, r, "file.txt", "base\nstaged\nunstaged1\nunstaged2\n")
	writeWorktree(t, r, "untracked.txt", "preview only\n")
	indexPath := filepath.Join(r.Root, ".git", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, r)
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("preview changed the index")
	}
	if s.StagedFiles != 1 {
		t.Fatalf("staged count = %d", s.StagedFiles)
	}
	f := s.File("file.txt")
	applySelection(t, r, f, chooseLines(f, func(l DiffLine) bool { return l.Text == "unstaged1" }))
	assertIndex(t, r, "file.txt", "base\nstaged\nunstaged1\n")
	assertWorktree(t, r, "file.txt", "base\nstaged\nunstaged1\nunstaged2\n")
	if _, err := r.git(context.Background(), "show", ":untracked.txt"); err == nil {
		t.Fatal("untracked preview accidentally added a file")
	}
}

func TestUnusualFilenamesAndLiteralPathspecs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows disallows some fixture filenames")
	}
	for _, name := range []string{"space name.go", "dir/naïve.go", "tab\tname.txt", "line\nbreak.txt", `quote"and\slash.txt`, ":(glob)*", "-leading-dash.txt", "dir b/a/file.txt"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			r := testRepo(t)
			writeWorktree(t, r, name, "old\n")
			commitAll(t, r)
			writeWorktree(t, r, name, "new\n")
			f := diffFile(t, r, name)
			applySelection(t, r, f, chooseLines(f, func(DiffLine) bool { return true }))
			assertIndex(t, r, name, "new\n")
			writeWorktree(t, r, name, "whole\n")
			if err := r.StageFile(context.Background(), name); err != nil {
				t.Fatal(err)
			}
			assertIndex(t, r, name, "whole\n")
		})
	}
}

func TestHunkStagingDoesNotStageModeChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bit is a Unix fixture")
	}
	r := testRepo(t)
	writeWorktree(t, r, "script", "old\n")
	commitAll(t, r)
	writeWorktree(t, r, "script", "new\n")
	if err := os.Chmod(filepath.Join(r.Root, "script"), 0755); err != nil {
		t.Fatal(err)
	}
	f := diffFile(t, r, "script")
	patchText := applySelection(t, r, f, chooseLines(f, func(DiffLine) bool { return true }))
	if strings.Contains(patchText, "old mode") || strings.Contains(patchText, "new mode") {
		t.Fatal("hunk patch included a mode change")
	}
	if !strings.HasPrefix(testGit(t, r, "ls-files", "--stage", "--", "script"), "100644 ") {
		t.Fatal("hunk staging changed the index mode")
	}
	f = diffFile(t, r, "script")
	if len(f.Hunks) != 0 || f.PartialReason == "" {
		t.Fatal("mode-only change was not classified")
	}
	if err := r.StageFile(context.Background(), "script"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(testGit(t, r, "ls-files", "--stage", "--", "script"), "100755 ") {
		t.Fatal("whole file did not stage the mode")
	}
}

func TestBinaryAndEmptyFilesAreWholeOnly(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "binary.dat", "\x00\x01\x02")
	writeWorktree(t, r, "empty.txt", "")
	for _, name := range []string{"binary.dat", "empty.txt"} {
		f := diffFile(t, r, name)
		if f.PartialReason == "" {
			t.Fatalf("%s must require whole-file staging", name)
		}
		if _, err := BuildPatch(f, map[LineRef]bool{}); err == nil {
			t.Fatal("unsupported partial stage accepted")
		}
		if err := r.StageFile(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	assertIndex(t, r, "binary.dat", "\x00\x01\x02")
	assertIndex(t, r, "empty.txt", "")
	if s := snapshot(t, r); len(s.Files) != 0 || s.StagedFiles != 2 {
		t.Fatalf("snapshot %+v", s)
	}
}

func TestSymlinkIsWholeOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions are platform-dependent")
	}
	r := testRepo(t)
	if err := os.Symlink("target", filepath.Join(r.Root, "link")); err != nil {
		t.Fatal(err)
	}
	f := diffFile(t, r, "link")
	if !strings.Contains(f.PartialReason, "symbolic link") {
		t.Fatalf("wrong classification: %+v", f)
	}
	if err := r.StageFile(context.Background(), "link"); err != nil {
		t.Fatal(err)
	}
	assertIndex(t, r, "link", "target")
}

func TestUnmergedPathsAreProtected(t *testing.T) {
	r := testRepo(t)
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
	f := diffFile(t, r, "conflict.txt")
	if !f.Conflict {
		t.Fatal("unmerged file was not protected")
	}
	msg := runStageCmd(r, stageRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: true})().(stagedMsg)
	if msg.Applied || msg.Err == nil {
		t.Fatal("whole-file staging resolved an unmerged path")
	}
}

func TestStalePreviewRefusesToStage(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "file", "old\n")
	commitAll(t, r)
	writeWorktree(t, r, "file", "preview\n")
	f := diffFile(t, r, "file")
	patchText, err := BuildPatch(f, chooseLines(f, func(DiffLine) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	writeWorktree(t, r, "file", "changed while previewing\n")
	msg := runStageCmd(r, stageRequest{Path: f.Path, Expected: f.Fingerprint, Patch: patchText})().(stagedMsg)
	if msg.Err == nil || msg.Applied || !msg.HaveSnapshot {
		t.Fatalf("bad stale result: %+v", msg)
	}
	assertIndex(t, r, "file", "old\n")
	assertWorktree(t, r, "file", "changed while previewing\n")
}

func TestParserDefaultsLargeLinesAndValidation(t *testing.T) {
	large := strings.Repeat("x", 100000)
	raw := "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@ section\n-old\n+" + large + "\n"
	files, err := ParseDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	h := files[0].Hunks[0]
	if h.OldLines != 1 || h.NewLines != 1 || h.Section != "section" || h.Lines[1].Text != large {
		t.Fatal("parser lost content or default count")
	}
	for _, bad := range []string{
		strings.Replace(raw, "@@ -1 +1 @@", "@@ -1,4 +1,4 @@", 1),
		"diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n\\ No newline at end of file\n",
		"diff --git a/f b/f\n--- a/../outside\n+++ b/../outside\n@@ -1 +1 @@\n-old\n+new\n",
	} {
		if _, err := ParseDiff(bad); err == nil {
			t.Fatal("accepted a malformed or unsafe patch")
		}
	}
}

func TestEditedPatchRecountAndFileRestriction(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "f", "old\n")
	commitAll(t, r)
	writeWorktree(t, r, "f", "new\nextra\n")
	f := diffFile(t, r, "f")
	base, err := BuildPatch(f, chooseLines(f, func(DiffLine) bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	edited := "# instructions\n" + strings.Replace(base, "+extra\n", "", 1)
	normalized, err := ValidateEditedPatch(base, edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyPatch(context.Background(), normalized); err != nil {
		t.Fatal(err)
	}
	assertIndex(t, r, "f", "new\n")
	assertWorktree(t, r, "f", "new\nextra\n")
	for _, bad := range []string{
		strings.ReplaceAll(base, `"a/f"`, `"a/outside"`),
		base + strings.ReplaceAll(base, `/f"`, `/another"`),
	} {
		if _, err := ValidateEditedPatch(base, bad); err == nil {
			t.Fatal("editor was allowed to alter file headers or other files")
		}
	}
	if _, err := ValidateEditedPatch(base, "# cancel\n"); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestNoSelectionDoesNotBuildPatch(t *testing.T) {
	f := &FileDiff{Path: "f", Hunks: []Hunk{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1, Lines: []DiffLine{{Kind: ' ', Text: "context"}}}}}
	if _, err := BuildPatch(f, map[LineRef]bool{{0, 0}: true}); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("context selection: %v", err)
	}
}
