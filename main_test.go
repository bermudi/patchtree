package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func keyMessage(key string) tea.KeyMsg {
	special := map[string]tea.KeyType{
		"enter": tea.KeyEnter, "tab": tea.KeyTab, "esc": tea.KeyEsc,
		"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
		"ctrl+d": tea.KeyCtrlD, "ctrl+u": tea.KeyCtrlU, "ctrl+c": tea.KeyCtrlC,
	}
	if kind, ok := special[key]; ok {
		return tea.KeyMsg{Type: kind}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

func press(m Model, key string) (Model, tea.Cmd) {
	updated, cmd := m.Update(keyMessage(key))
	return updated.(Model), cmd
}

func loadedModel(t *testing.T, r *Repository) Model {
	t.Helper()
	m := NewModel(r, Options{NoDelta: true})
	updated, _ := m.Update(readSnapshotCmd(r, m.Epoch)())
	m = updated.(Model)
	if !m.Loaded || m.Busy != "" || m.Reloading {
		t.Fatalf("load message did not persist model changes: %+v", m)
	}
	return m
}

func finishStage(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected an asynchronous staging command")
	}
	msg, ok := cmd().(stagedMsg)
	if !ok {
		t.Fatal("command did not return stagedMsg")
	}
	if msg.Err != nil {
		t.Fatal(msg.Err)
	}
	updated, _ := m.Update(msg)
	m = updated.(Model)
	if m.Busy != "" || m.Quitting {
		t.Fatal("stage exited or left the model busy")
	}
	return m
}

func TestFocusAwareNAndS(t *testing.T) {
	r := testRepo(t)
	for _, name := range []string{"a.txt", "b.txt"} {
		writeWorktree(t, r, name, "first\noldA\ngap\noldB\nlast\n")
	}
	commitAll(t, r)
	for _, name := range []string{"a.txt", "b.txt"} {
		writeWorktree(t, r, name, "first\nnewA\ngap\nnewB\nlast\n")
	}
	m := loadedModel(t, r)
	if m.ActivePath != "a.txt" || m.Focus != focusTree {
		t.Fatal("incorrect initial file/focus")
	}
	m, _ = press(m, "s")
	if !m.SideBySide || len(m.Units) != 1 {
		t.Fatal("tree s did not toggle layout only")
	}
	m, _ = press(m, "n")
	if m.ActivePath != "b.txt" {
		t.Fatal("tree n did not go to next file")
	}
	m, _ = press(m, "enter")
	if m.Focus != focusDiff {
		t.Fatal("Enter did not focus the diff")
	}
	m, _ = press(m, "s")
	if len(m.Units) != 2 || !m.SideBySide {
		t.Fatal("diff s must split, not toggle the layout")
	}
	m, _ = press(m, "n")
	if m.ActivePath != "b.txt" || m.Unit != 1 || !m.Units[0].Skipped {
		t.Fatal("diff n did not skip just one hunk")
	}
	assertIndex(t, r, "b.txt", "first\noldA\ngap\noldB\nlast\n")
	m, _ = press(m, "S")
	if m.SideBySide {
		t.Fatal("S did not toggle layout in diff focus")
	}
	m, _ = press(m, "N")
	if m.ActivePath != "a.txt" {
		t.Fatal("N did not navigate files globally")
	}
}

func TestVisualLineMarkingAndStageRefresh(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "f", "one\nold\nlast\n")
	commitAll(t, r)
	writeWorktree(t, r, "f", "one\nnew1\nnew2\nlast\n")
	m := loadedModel(t, r)
	m, _ = press(m, "enter")
	m, _ = press(m, "V")
	m, _ = press(m, "J")
	if !m.Visual || m.Anchor == m.Line {
		t.Fatal("J did not extend the visual selection")
	}
	m, _ = press(m, " ")
	if len(m.Marks) != 2 {
		t.Fatalf("wanted deletion + first addition marked: %v", m.Marks)
	}
	assertIndex(t, r, "f", "one\nold\nlast\n") // Space is deliberately non-mutating.
	m, cmd := press(m, "y")
	if m.Busy != "staging" {
		t.Fatal("model did not enter staging state")
	}
	if newer, second := press(m, "y"); second != nil || newer.Epoch != m.Epoch {
		t.Fatal("busy model accepted duplicate staging")
	}
	if newer, quit := press(m, "q"); newer.Quitting || quit != nil {
		t.Fatal("quit raced an index write")
	}
	m = finishStage(t, m, cmd)
	assertIndex(t, r, "f", "one\nnew1\nlast\n")
	assertWorktree(t, r, "f", "one\nnew1\nnew2\nlast\n")
	if len(m.Marks) != 0 || m.Visual || m.Snapshot.StagedFiles != 1 || m.activeFile() == nil {
		t.Fatal("partial stage did not refresh in place")
	}
	m, cmd = press(m, "y")
	m = finishStage(t, m, cmd)
	if len(m.Tree.Rows) != 0 || m.ActivePath != "" || m.Snapshot.StagedFiles != 1 {
		t.Fatal("fully staged file did not disappear while preserving index count")
	}
}

func TestCollapsedDirectoryAndNearbyCursorSurviveStaging(t *testing.T) {
	r := testRepo(t)
	for _, name := range []string{"aaa/one", "bbb/two", "last"} {
		writeWorktree(t, r, name, "old\n")
	}
	commitAll(t, r)
	for _, name := range []string{"aaa/one", "bbb/two", "last"} {
		writeWorktree(t, r, name, "new\n")
	}
	m := loadedModel(t, r)
	m.Tree.Collapsed["bbb"] = true
	m.Tree.Rebuild(m.Snapshot.Files)
	m, _ = press(m, "enter")
	m, cmd := press(m, "f")
	m = finishStage(t, m, cmd)
	if !m.Tree.Collapsed["bbb"] {
		t.Fatal("staging lost the collapsed state")
	}
	if m.ActivePath != "last" || m.Tree.CursorKey != "last" {
		t.Fatalf("did not select a nearby visible file: active=%q cursor=%q", m.ActivePath, m.Tree.CursorKey)
	}
	if m.Snapshot.StagedFiles != 1 {
		t.Fatal("staged count did not persist")
	}
}

func TestUnchangedWatchRefreshPreservesSelectionsAndSplits(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "dir/f", "first\noldA\ngap\noldB\nlast\n")
	writeWorktree(t, r, "other/x", "old\n")
	commitAll(t, r)
	writeWorktree(t, r, "dir/f", "first\nnewA\ngap\nnewB\nlast\n")
	writeWorktree(t, r, "other/x", "new\n")
	m := loadedModel(t, r)
	m, _ = press(m, "enter")
	m, _ = press(m, "s")
	m, _ = press(m, "]")
	m, _ = press(m, "V")
	m, _ = press(m, " ")
	m.Tree.Collapsed["other"] = true
	m.Tree.Rebuild(m.Snapshot.Files)
	m.Viewport.SetYOffset(2)
	oldPath, oldKey, oldUnit, oldLine, oldOffset := m.ActivePath, m.Tree.CursorKey, m.Unit, m.Line, m.Viewport.YOffset
	updated, _ := m.Update(refreshMsg{Epoch: m.Epoch, Snapshot: snapshot(t, r)})
	m = updated.(Model)
	if m.ActivePath != oldPath || m.Tree.CursorKey != oldKey || m.Unit != oldUnit || m.Line != oldLine || m.Viewport.YOffset != oldOffset || !m.Visual || len(m.Marks) != 1 || len(m.Units) != 2 || !m.Tree.Collapsed["other"] {
		t.Fatal("unchanged watch refresh reset UI state")
	}
	writeWorktree(t, r, "dir/f", "external edit\n")
	updated, _ = m.Update(refreshMsg{Epoch: m.Epoch, Snapshot: snapshot(t, r)})
	m = updated.(Model)
	if len(m.Marks) != 0 || m.Visual || !strings.Contains(m.Notice, "cleared") {
		t.Fatal("changed watch preview retained stale line references")
	}
}

func TestRefreshEpochRejectsPreStageRead(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "f", "old\n")
	commitAll(t, r)
	writeWorktree(t, r, "f", "new\n")
	m := loadedModel(t, r)
	oldRead := refreshMsg{Epoch: m.Epoch, Snapshot: m.Snapshot}
	m, _ = press(m, "enter")
	m, cmd := press(m, "y")
	updated, _ := m.Update(oldRead)
	m = updated.(Model)
	if m.Busy != "staging" {
		t.Fatal("old watch read reset staging state")
	}
	m = finishStage(t, m, cmd)
	updated, _ = m.Update(oldRead)
	m = updated.(Model)
	if len(m.Snapshot.Files) != 0 || m.Snapshot.StagedFiles != 1 {
		t.Fatal("old watch read resurrected already-staged files")
	}
}

func TestSearchEscapeAndQAreTextWhileSearching(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "src/query.go", "hello\n")
	writeWorktree(t, r, "src/other.go", "hello\n")
	m := loadedModel(t, r)
	m, _ = press(m, "/")
	m, _ = press(m, "q")
	if m.Quitting || !m.Searching || m.Tree.Query != "q" || m.ActivePath != "src/query.go" {
		t.Fatal("search input was dispatched as a navigation/quit command")
	}
	m, _ = press(m, "enter")
	if m.Searching || m.Focus != focusDiff || m.Tree.Query != "q" {
		t.Fatal("search confirmation did not retain the filter")
	}
	m, _ = press(m, "esc")
	if m.Tree.Query != "" || len(m.Tree.Rows) != 3 {
		t.Fatal("Escape did not clear the filename filter")
	}
	m, _ = press(m, "/")
	m, _ = press(m, "no match")
	m, _ = press(m, "esc")
	if m.Searching || m.Tree.Query != "" || len(m.Tree.Rows) != 3 {
		t.Fatal("Escape did not cancel a live search")
	}
}

func TestSkipRemainingNeverStagesAndQuitIsNonMutating(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "f", "first\noldA\ngap\noldB\nlast\n")
	commitAll(t, r)
	writeWorktree(t, r, "f", "first\nnewA\ngap\nnewB\nlast\n")
	m := loadedModel(t, r)
	m, _ = press(m, "enter")
	m, _ = press(m, "s")
	m, cmd := press(m, "d")
	if cmd != nil || !m.Units[0].Skipped || !m.Units[1].Skipped {
		t.Fatal("d did not skip all remaining units")
	}
	assertIndex(t, r, "f", "first\noldA\ngap\noldB\nlast\n")
	m, cmd = press(m, "q")
	if !m.Quitting || cmd == nil {
		t.Fatal("q did not quit")
	}
	assertIndex(t, r, "f", "first\noldA\ngap\noldB\nlast\n")
}

func TestStageAllRemainingLeavesEarlierHunkUnstaged(t *testing.T) {
	r := testRepo(t)
	writeWorktree(t, r, "f", "first\noldA\ngap\noldB\nlast\n")
	commitAll(t, r)
	writeWorktree(t, r, "f", "first\nnewA\ngap\nnewB\nlast\n")
	m := loadedModel(t, r)
	m, _ = press(m, "enter")
	m, _ = press(m, "s")
	m, _ = press(m, "n")
	m, cmd := press(m, "a")
	m = finishStage(t, m, cmd)
	assertIndex(t, r, "f", "first\noldA\ngap\nnewB\nlast\n")
	if m.activeFile() == nil || len(m.Units) == 0 {
		t.Fatal("earlier skipped change vanished")
	}
}

func TestErrorAndHelpStates(t *testing.T) {
	m := NewModel(&Repository{Root: "/unused"}, Options{NoDelta: true})
	updated, _ := m.Update(refreshMsg{Err: errors.New("fixture failure")})
	m = updated.(Model)
	if !m.NoticeError || m.Busy != "" || strings.Contains(m.View(), "Nothing left to stage") {
		t.Fatal("failed read displayed a falsely clean state")
	}
	m, _ = press(m, "?")
	if !m.Help {
		t.Fatal("? did not open help")
	}
	m, _ = press(m, "esc")
	if m.Help {
		t.Fatal("Escape did not close help")
	}
	m, cmd := press(m, "r")
	if cmd == nil || !m.Reloading {
		t.Fatal("r did not schedule a refresh")
	}
}

func TestWatchDoesNotOverlapReads(t *testing.T) {
	m := NewModel(&Repository{Root: "/unused"}, Options{NoDelta: true, Watch: true, WatchInterval: time.Second})
	m.Busy, m.Reloading = "", false
	updated, cmd := m.Update(watchMsg(time.Now()))
	m = updated.(Model)
	if !m.Reloading || cmd == nil {
		t.Fatal("watch did not schedule a refresh")
	}
	batch := cmd().(tea.BatchMsg)
	if len(batch) != 2 {
		t.Fatal("watch should schedule one timer and one read")
	}
	updated, cmd = m.Update(watchMsg(time.Now()))
	m = updated.(Model)
	// A single-command tea.Batch returns that command directly. Inspecting the
	// timer by executing it would sleep, so only assert that refresh stays busy.
	if !m.Reloading || cmd == nil {
		t.Fatal("watch lost its timer while read was pending")
	}
}

func TestEditorCommandQuotedArguments(t *testing.T) {
	t.Setenv("GIT_EDITOR", "")
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", `"/some/editor with spaces" --wait "an argument"`)
	cmd, err := editorCommand("/tmp/patch with spaces.patch", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[0] != "/some/editor with spaces" || len(cmd.Args) != 4 || cmd.Args[2] != "an argument" || cmd.Args[3] != "/tmp/patch with spaces.patch" || cmd.Dir != "/repo" {
		t.Fatalf("editor args: %v", cmd.Args)
	}
}

func TestCommandLineValidation(t *testing.T) {
	if err := run([]string{"--help"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--watch-interval=10ms"}); err == nil {
		t.Fatal("too-short interval accepted")
	}
	if err := run([]string{"one", "two"}); err == nil {
		t.Fatal("two repository directories accepted")
	}
}

func TestUnmarkingAllLinesClarifiesHunkAction(t *testing.T) {
	m := renderModel(t)
	m.Focus = focusDiff
	m, _ = press(m, " ")
	m, _ = press(m, " ")
	if len(m.Marks) != 0 || !strings.Contains(m.Notice, "whole hunk") {
		t.Fatal("clearing the last mark left a misleading staging hint")
	}
	m, cmd := press(m, "enter")
	if cmd != nil || m.Busy != "" {
		t.Fatal("Enter with no line marks should not stage anything")
	}
}
