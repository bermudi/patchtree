package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-shellwords"
)

var version = "0.2.0"

type focus uint8

const (
	focusTree focus = iota
	focusDiff
)

type Options struct {
	Watch, SideBySide, NoDelta bool
	WatchInterval              time.Duration
}

type fileViewState struct {
	Fingerprint                    string
	Units                          []HunkUnit
	Unit, Line, Offset, Horizontal int
}

// Model owns only UI/session state. Git and delta run in asynchronous commands
// and return typed messages; no subprocess blocks keyboard handling (except the
// intentional terminal handoff to $EDITOR with tea.ExecProcess).
type Model struct {
	Repo                     *Repository
	Snapshot                 Snapshot
	Loaded                   bool
	Focus                    focus
	Tree                     TreeState
	Search                   textinput.Model
	Searching                bool
	Viewport                 viewport.Model
	Width, Height, TreeWidth int

	ActivePath string
	Units      []HunkUnit
	Unit, Line int
	Visual     bool
	Anchor     int
	Marks      map[LineRef]bool
	Horizontal int
	FileViews  map[string]fileViewState
	DiffRows   []DiffRow

	SideBySide        bool
	Delta             string
	DeltaFailed       bool
	Colors            map[string]Highlighted
	PendingHighlights map[string]string

	Watch         bool
	WatchInterval time.Duration
	Busy          string
	Reloading     bool
	Epoch         uint64 // Invalidates reads started before a staging/editor action.
	Notice        string
	NoticeError   bool
	Pending       *PendingRevert
	Help          bool
	HelpOffset    int
	Quitting      bool
}

type refreshMsg struct {
	Epoch    uint64
	Snapshot Snapshot
	Err      error
}

type watchMsg time.Time

type highlightMsg struct {
	Path, Fingerprint string
	Result            Highlighted
	Err               error
}

type stageRequest struct {
	Path, Expected, Patch string
	WholeFile             bool
	Anchor                int
	Epoch                 uint64
}

type stagedMsg struct {
	Request      stageRequest
	Snapshot     Snapshot
	HaveSnapshot bool
	Applied      bool
	Err          error
}

type editedMsg struct {
	Request      stageRequest
	Base, Edited string
	Err          error
}

// revertRequest restores worktree content from the index: either the patch's
// selected lines or, with WholeFile, the entire path. The index never changes.
type revertRequest struct {
	Path, Expected, Patch, Label string
	WholeFile                    bool
	Anchor                       int
	Epoch                        uint64
}

type revertedMsg struct {
	Request      revertRequest
	Snapshot     Snapshot
	HaveSnapshot bool
	Applied      bool
	Err          error
}

// PendingRevert is a confirmation prompt; nothing has been modified yet.
type PendingRevert struct {
	Label   string
	Request revertRequest
}

func NewModel(repo *Repository, options Options) Model {
	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "search filenames"
	search.CharLimit = 4096
	search.Width = 25
	interval := options.WatchInterval
	if interval <= 0 {
		interval = 1500 * time.Millisecond
	}
	m := Model{
		Repo: repo, Focus: focusTree, Search: search,
		Tree:     TreeState{Collapsed: make(map[string]bool)},
		Viewport: viewport.New(51, 18), Width: 80, Height: 24, TreeWidth: 28,
		Marks: make(map[LineRef]bool), FileViews: make(map[string]fileViewState),
		Colors: make(map[string]Highlighted), PendingHighlights: make(map[string]string),
		SideBySide: options.SideBySide, Watch: options.Watch, WatchInterval: interval,
		Busy: "loading", Reloading: true,
	}
	if !options.NoDelta {
		if binary, err := exec.LookPath("delta"); err == nil {
			m.Delta = binary
		} else {
			m.notice("delta not found: using basic colors. Install git-delta for syntax highlighting.", false)
		}
	}
	m.rebuildDiff(false)
	return m
}

func readSnapshotCmd(repo *Repository, epoch uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		snapshot, err := repo.ReadSnapshot(ctx)
		return refreshMsg{Epoch: epoch, Snapshot: snapshot, Err: err}
	}
}

func watchCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return watchMsg(t) })
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{readSnapshotCmd(m.Repo, m.Epoch)}
	if m.Watch {
		cmds = append(cmds, watchCmd(m.WatchInterval))
	}
	return tea.Batch(cmds...)
}

func (m Model) activeFile() *FileDiff { return m.Snapshot.File(m.ActivePath) }

func (m *Model) notice(s string, failure bool) {
	m.Notice, m.NoticeError = s, failure
}

func (m *Model) saveFileView() {
	if f := m.activeFile(); f != nil {
		m.FileViews[f.Path] = fileViewState{Fingerprint: f.Fingerprint,
			Units: append([]HunkUnit(nil), m.Units...), Unit: m.Unit, Line: m.Line,
			Offset: m.Viewport.YOffset, Horizontal: m.Horizontal}
	}
}

func (m *Model) highlight() tea.Cmd {
	f := m.activeFile()
	if f == nil || len(f.Hunks) == 0 || m.Delta == "" || m.DeltaFailed {
		return nil
	}
	if cache, exists := m.Colors[f.Path]; exists && cache.Fingerprint == f.Fingerprint {
		return nil
	}
	if m.PendingHighlights[f.Path] == f.Fingerprint {
		return nil
	}
	m.PendingHighlights[f.Path] = f.Fingerprint
	delta, root := m.Delta, m.Repo.Root
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result, err := highlightWithDelta(ctx, delta, root, f)
		return highlightMsg{Path: f.Path, Fingerprint: f.Fingerprint, Result: result, Err: err}
	}
}

// activateFile restores split/skipped hunks and scroll for unchanged files.
// Pending marks intentionally do not cross file switches; they are NOT staged.
func (m *Model) activateFile(name string, anchor int) tea.Cmd {
	m.ActivePath = name
	m.Marks = make(map[LineRef]bool)
	m.Visual = false
	m.Unit, m.Line, m.Horizontal = 0, 0, 0
	m.Viewport.SetYOffset(0)
	f := m.activeFile()
	m.Units = initialUnits(f)
	if f != nil {
		if state, exists := m.FileViews[name]; exists && state.Fingerprint == f.Fingerprint {
			m.Units = append([]HunkUnit(nil), state.Units...)
			m.Unit, m.Line, m.Horizontal = state.Unit, state.Line, state.Horizontal
			m.Viewport.SetYOffset(state.Offset)
		} else if len(m.Units) > 0 {
			if anchor >= 0 {
				bestDistance := int(^uint(0) >> 1)
				for i, unit := range m.Units {
					start := f.Hunks[unit.Hunk].OldStart
					distance := start - anchor
					if distance < 0 {
						distance = -distance
					}
					if distance < bestDistance {
						m.Unit, bestDistance = i, distance
					}
				}
			}
			m.Line = firstChange(f, m.Units[m.Unit])
		}
	}
	if len(m.Units) > 0 {
		m.Unit = min(max(0, m.Unit), len(m.Units)-1)
		u := m.Units[m.Unit]
		m.Line = min(max(u.Start, m.Line), u.End-1)
	}
	m.rebuildDiff(true)
	return m.highlight()
}

func (m *Model) replaceSnapshot(snapshot Snapshot, staged bool, anchor int) tea.Cmd {
	oldPath := m.ActivePath
	old := m.activeFile()
	oldFingerprint := ""
	if old != nil {
		oldFingerprint = old.Fingerprint
	}
	oldMarks := len(m.Marks)
	m.saveFileView()
	m.Snapshot, m.Loaded = snapshot, true
	m.Tree.Rebuild(snapshot.Files)
	newFile := snapshot.File(oldPath)
	if newFile != nil && newFile.Fingerprint == oldFingerprint && !staged {
		// A watch refresh with no changes must not reset cursor, marks, splits,
		// collapsed directories or viewport position.
		m.Tree.KeepVisible(m.treeHeight())
		m.rebuildDiff(false)
		return m.highlight()
	}
	name := oldPath
	if newFile == nil {
		name = ""
		if selected := m.Tree.Selected(); selected != nil && !selected.Dir {
			name = selected.Path
		} else {
			// Pick a nearby VISIBLE file, without opening collapsed directories.
			for i := m.Tree.Cursor; i < len(m.Tree.Rows); i++ {
				if !m.Tree.Rows[i].Dir {
					name = m.Tree.Rows[i].Path
					break
				}
			}
			if name == "" {
				for i := min(m.Tree.Cursor-1, len(m.Tree.Rows)-1); i >= 0; i-- {
					if !m.Tree.Rows[i].Dir {
						name = m.Tree.Rows[i].Path
						break
					}
				}
			}
			if name == "" && len(snapshot.Files) > 0 && m.Tree.Query == "" {
				name = snapshot.Files[0].Path
			}
		}
		if name != "" {
			m.Tree.SelectFile(snapshot.Files, name, false)
		}
	}
	if !staged && oldMarks > 0 {
		m.notice("Diff changed; pending line marks were cleared. Review the new preview.", false)
	}
	cmd := m.activateFile(name, anchor)
	m.Tree.KeepVisible(m.treeHeight())
	// Bound long watch sessions: retain only caches for files still unstaged.
	for name := range m.Colors {
		if snapshot.File(name) == nil {
			delete(m.Colors, name)
		}
	}
	for name := range m.FileViews {
		if snapshot.File(name) == nil {
			delete(m.FileViews, name)
		}
	}
	return cmd
}

func (m *Model) selectTreeFile() tea.Cmd {
	row := m.Tree.Selected()
	if row == nil || row.Dir || row.Path == m.ActivePath {
		m.Tree.KeepVisible(m.treeHeight())
		return nil
	}
	marked := len(m.Marks)
	m.saveFileView()
	cmd := m.activateFile(row.Path, -1)
	m.Tree.KeepVisible(m.treeHeight())
	if marked > 0 {
		m.notice(fmt.Sprintf("Cleared %d pending marks on file switch; nothing staged.", marked), false)
	}
	return cmd
}

func (m *Model) nextFile(delta int, wrap bool) tea.Cmd {
	paths := m.Tree.FilePaths(m.Snapshot.Files)
	if len(paths) == 0 {
		m.notice("No files match the current search.", false)
		return nil
	}
	index := -1
	for i, name := range paths {
		if name == m.ActivePath {
			index = i
			break
		}
	}
	next := index + delta
	if index < 0 && delta < 0 {
		next = len(paths) - 1
	}
	if wrap {
		next = (next%len(paths) + len(paths)) % len(paths)
	} else if next < 0 || next >= len(paths) {
		m.Focus = focusTree
		m.notice("No more files. Skipped changes are still unstaged.", false)
		return nil
	}
	m.Tree.SelectFile(m.Snapshot.Files, paths[next], true)
	return m.selectTreeFile()
}

func (m *Model) jumpUnit(index int) {
	if len(m.Units) == 0 {
		return
	}
	m.Unit = min(max(0, index), len(m.Units)-1)
	m.Line = firstChange(m.activeFile(), m.Units[m.Unit])
	m.Visual = false
	m.rebuildDiff(true)
}

func (m *Model) moveDiffLine(direction int, extend bool) {
	if len(m.Units) == 0 {
		return
	}
	if extend && !m.Visual {
		m.Anchor, m.Visual = m.Line, true
	}
	ref, valid := m.currentRef()
	if !valid {
		return
	}
	preferRight := m.activeFile().Hunks[ref.Hunk].Lines[ref.Line].Kind == '+'
	for i := m.cursorRow() + direction; i >= 0 && i < len(m.DiffRows); i += direction {
		row := m.DiffRows[i]
		if m.Visual && row.Unit != m.Unit {
			break // Visual ranges stay inside one selectable hunk.
		}
		if next, ok := row.Ref(preferRight); ok {
			m.Unit, m.Line = row.Unit, next.Line
			m.rebuildDiff(true)
			return
		}
	}
	m.rebuildDiff(true)
}

func (m *Model) chooseSide(right bool) {
	row := m.cursorRow()
	if row < 0 || !m.effectiveSideBySide() {
		return
	}
	ref := m.DiffRows[row].Left
	if right {
		ref = m.DiffRows[row].Right
	}
	if ref != nil {
		m.Line = ref.Line
		m.rebuildDiff(true)
	}
}

func (m *Model) scrollDiff(direction int) {
	if direction > 0 {
		m.Viewport.HalfViewDown()
	} else {
		m.Viewport.HalfViewUp()
	}
	m.Visual = false
	// Keep the selected hunk/line on screen after keyboard scrolling.
	start := m.Viewport.YOffset
	end := min(len(m.DiffRows), start+m.Viewport.Height)
	cursor := m.cursorRow()
	if cursor >= start && cursor < end {
		return
	}
	if direction < 0 {
		for i := end - 1; i >= start; i-- {
			if ref, ok := m.DiffRows[i].Ref(true); ok {
				m.Unit, m.Line = m.DiffRows[i].Unit, ref.Line
				m.rebuildDiff(false)
				return
			}
		}
	} else {
		for i := start; i < end; i++ {
			if ref, ok := m.DiffRows[i].Ref(true); ok {
				m.Unit, m.Line = m.DiffRows[i].Unit, ref.Line
				m.rebuildDiff(false)
				return
			}
		}
	}
}

func (m *Model) toggleMarks() {
	f := m.activeFile()
	if f == nil || len(m.Units) == 0 {
		return
	}
	u := m.Units[m.Unit]
	start, end := m.Line, m.Line
	if m.Visual {
		start, end = min(m.Anchor, m.Line), max(m.Anchor, m.Line)
	}
	var refs []LineRef
	allMarked := true
	for i := start; i <= end; i++ {
		if f.Hunks[u.Hunk].Lines[i].Kind != ' ' {
			ref := LineRef{u.Hunk, i}
			refs = append(refs, ref)
			allMarked = allMarked && m.Marks[ref]
		}
	}
	if len(refs) == 0 {
		m.notice("Context lines cannot be staged. Select + or − lines.", false)
		return
	}
	for _, ref := range refs {
		if allMarked {
			delete(m.Marks, ref)
		} else {
			m.Marks[ref] = true
		}
	}
	if len(m.Marks) == 0 {
		m.notice("No lines marked. y stages the whole hunk; Space marks lines.", false)
	} else {
		m.notice(fmt.Sprintf("%d lines marked. y/Enter stages marks; u clears them.", len(m.Marks)), false)
	}
	m.rebuildDiff(false)
}

func runStageCmd(repo *Repository, request stageRequest) tea.Cmd {
	return func() tea.Msg {
		msg := stagedMsg{Request: request}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		// Reject stale previews, including edits made while $EDITOR was open.
		before, err := repo.ReadSnapshot(ctx)
		if err != nil {
			msg.Err = err
			return msg
		}
		msg.Snapshot, msg.HaveSnapshot = before, true
		f := before.File(request.Path)
		if f == nil || f.Fingerprint != request.Expected {
			msg.Err = fmt.Errorf("file or index changed since preview; refreshed without staging. Review and retry")
			return msg
		}
		if f.Conflict {
			msg.Err = fmt.Errorf("resolve unmerged paths outside Patchtree")
			return msg
		}
		if request.WholeFile {
			err = repo.StageFile(ctx, request.Path)
		} else {
			err = repo.ApplyPatch(ctx, request.Patch)
		}
		if err != nil {
			msg.Err = err
			return msg
		}
		msg.Applied = true
		after, err := repo.ReadSnapshot(ctx)
		if err != nil {
			msg.HaveSnapshot = false
			msg.Err = fmt.Errorf("staged successfully, but refresh failed: %w", err)
			return msg
		}
		msg.Snapshot = after
		if request.WholeFile {
			if remaining := after.File(request.Path); remaining != nil && remaining.Fingerprint == f.Fingerprint {
				// Adding a dirty-only submodule is a successful Git no-op, not
				// staging its nested changes. Do not claim those were staged.
				msg.Applied = false
				msg.Err = fmt.Errorf("Git made no stageable index change; nested repository contents must be staged inside that repository")
			}
		}
		return msg
	}
}

func (m *Model) startStage(request stageRequest) tea.Cmd {
	m.Epoch++
	request.Epoch = m.Epoch
	m.Busy, m.Reloading = "staging", false
	m.Visual = false
	m.notice("", false)
	m.rebuildDiff(false)
	return runStageCmd(m.Repo, request)
}

// runRevertCmd re-reads the snapshot first: a stale preview (or a concurrent
// edit) must never revert content the user has not seen.
func runRevertCmd(repo *Repository, request revertRequest) tea.Cmd {
	return func() tea.Msg {
		msg := revertedMsg{Request: request}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		before, err := repo.ReadSnapshot(ctx)
		if err != nil {
			msg.Err = err
			return msg
		}
		msg.Snapshot, msg.HaveSnapshot = before, true
		f := before.File(request.Path)
		if f == nil || f.Fingerprint != request.Expected {
			msg.Err = fmt.Errorf("file or index changed since preview; nothing reverted. Review and retry")
			return msg
		}
		if f.Conflict {
			msg.Err = fmt.Errorf("resolve unmerged paths outside Patchtree")
			return msg
		}
		if f.Untracked || f.New {
			msg.Err = fmt.Errorf("untracked or intent-to-add file cannot be reverted; there is no stored content to restore")
			return msg
		}
		if request.WholeFile {
			err = repo.RestoreFile(ctx, request.Path)
		} else {
			err = repo.ApplyRevertPatch(ctx, request.Patch)
		}
		if err != nil {
			msg.Err = err
			return msg
		}
		msg.Applied = true
		after, err := repo.ReadSnapshot(ctx)
		if err != nil {
			msg.HaveSnapshot = false
			msg.Err = fmt.Errorf("reverted successfully, but refresh failed: %w", err)
			return msg
		}
		msg.Snapshot = after
		return msg
	}
}

func (m *Model) startRevert(request revertRequest) tea.Cmd {
	m.Epoch++
	request.Epoch = m.Epoch
	m.Busy, m.Reloading = "reverting", false
	m.Visual = false
	m.notice("", false)
	m.rebuildDiff(false)
	return runRevertCmd(m.Repo, request)
}

// requestRevert stages nothing: it prepares the mirror of y and asks for a
// confirmation. Whole-file X reuses requestRevertFile.
func (m *Model) requestRevert() {
	f := m.activeFile()
	if f == nil {
		m.notice("No unstaged file selected.", false)
		return
	}
	if len(m.Units) == 0 {
		m.notice("Cannot partially revert: "+f.PartialReason+". X reverts the whole file.", true)
		return
	}
	switch {
	case f.Conflict:
		m.notice(f.PartialReason, true)
		return
	case f.Untracked || f.New:
		m.notice("Untracked or intent-to-add file has no stored content to restore; remove it outside Patchtree.", true)
		return
	case f.Deleted:
		m.notice("This file is deleted in the worktree; X restores the whole file.", true)
		return
	case f.PartialReason != "":
		m.notice("Cannot revert part of this change: "+f.PartialReason+". X reverts the whole file.", true)
		return
	}
	selected := m.Marks
	label := fmt.Sprintf("hunk %d/%d of %s", m.Unit+1, len(m.Units), displayPath(f.Path))
	if len(selected) == 0 {
		selected = unitSelection(f, m.Units[m.Unit:m.Unit+1])
	} else if len(selected) == 1 {
		label = "1 marked line in " + displayPath(f.Path)
	} else {
		label = fmt.Sprintf("%d marked lines in %s", len(selected), displayPath(f.Path))
	}
	patchText, err := BuildRevertPatch(f, selected)
	if err != nil {
		m.notice(err.Error(), true)
		return
	}
	m.Pending = &PendingRevert{Label: label, Request: revertRequest{
		Path: f.Path, Expected: f.Fingerprint, Patch: patchText, Label: label,
		Anchor: f.Hunks[m.Units[m.Unit].Hunk].OldStart,
	}}
}

func (m *Model) requestRevertFile() {
	f := m.activeFile()
	if f == nil {
		m.notice("No unstaged file selected.", false)
		return
	}
	switch {
	case f.Conflict:
		m.notice(f.PartialReason, true)
		return
	case f.Untracked || f.New:
		m.notice("Untracked or intent-to-add file has no stored content to restore; remove it outside Patchtree.", true)
		return
	}
	label := "all unstaged changes in " + displayPath(f.Path)
	anchor := -1
	if len(m.Units) > 0 {
		anchor = f.Hunks[m.Units[m.Unit].Hunk].OldStart
	}
	m.Pending = &PendingRevert{Label: label, Request: revertRequest{
		Path: f.Path, Expected: f.Fingerprint, WholeFile: true, Label: label, Anchor: anchor,
	}}
}

func (m *Model) stageSelection(remaining, wholeFile bool) tea.Cmd {
	f := m.activeFile()
	if f == nil {
		m.notice("No unstaged file selected.", false)
		return nil
	}
	if f.Conflict {
		m.notice(f.PartialReason, true)
		return nil
	}
	request := stageRequest{Path: f.Path, Expected: f.Fingerprint, WholeFile: wholeFile, Anchor: -1}
	if len(m.Units) > 0 {
		request.Anchor = f.Hunks[m.Units[m.Unit].Hunk].OldStart
	}
	if !wholeFile {
		if len(m.Units) == 0 {
			m.notice(f.PartialReason, true)
			return nil
		}
		selected := m.Marks
		if remaining {
			selected = unitSelection(f, m.Units[m.Unit:])
		} else if len(selected) == 0 {
			selected = unitSelection(f, m.Units[m.Unit:m.Unit+1])
		}
		patchText, err := BuildPatch(f, selected)
		if err != nil {
			m.notice(err.Error(), true)
			return nil
		}
		request.Patch = patchText
	}
	return m.startStage(request)
}

func editorCommand(filename, root string) (*exec.Cmd, error) {
	editor := ""
	for _, variable := range []string{"GIT_EDITOR", "VISUAL", "EDITOR"} {
		if value := os.Getenv(variable); value != "" {
			editor = value
			break
		}
	}
	if editor == "" {
		editor = "vi"
	}
	args, err := shellwords.Parse(editor)
	if err != nil || len(args) == 0 {
		return nil, fmt.Errorf("invalid editor command %q", editor)
	}
	cmd := exec.Command(args[0], append(args[1:], filename)...)
	cmd.Dir = root
	return cmd, nil
}

func (m *Model) editHunk() tea.Cmd {
	f := m.activeFile()
	if f == nil || len(m.Units) == 0 {
		m.notice("No editable text hunk. Use f for whole-file staging.", false)
		return nil
	}
	selected := m.Marks
	if len(selected) == 0 {
		selected = unitSelection(f, m.Units[m.Unit:m.Unit+1])
	}
	base, err := BuildPatch(f, selected)
	if err != nil {
		m.notice(err.Error(), true)
		return nil
	}
	file, err := os.CreateTemp("", "patchtree-edit-*.patch")
	if err != nil {
		m.notice(err.Error(), true)
		return nil
	}
	instructions := "# Patchtree: edit this INDEX patch; saving applies it to the index only.\n" +
		"# Remove '+' lines to omit additions; turn '-' into ' ' to keep deletions.\n" +
		"# Hunk counts are recounted; adjust old starts if you remove leading context.\n" +
		"# File headers/modes must not change. Empty the file to cancel.\n" +
		"# Use an editor's blocking/wait option (e.g. code --wait).\n"
	_, writeErr := file.WriteString(instructions + base)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(file.Name())
		m.notice(fmt.Sprintf("write editor patch: %v", errors.Join(writeErr, closeErr)), true)
		return nil
	}
	cmd, err := editorCommand(file.Name(), m.Repo.Root)
	if err != nil {
		os.Remove(file.Name())
		m.notice(err.Error(), true)
		return nil
	}
	m.Epoch++
	request := stageRequest{Epoch: m.Epoch, Path: f.Path, Expected: f.Fingerprint,
		Anchor: f.Hunks[m.Units[m.Unit].Hunk].OldStart}
	m.Busy, m.Reloading = "editing", false
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.Remove(file.Name())
		msg := editedMsg{Request: request, Base: base, Err: err}
		if err == nil {
			data, readErr := os.ReadFile(file.Name())
			msg.Edited, msg.Err = string(data), readErr
		}
		return msg
	})
}

func (m *Model) skipHunks(remaining bool) tea.Cmd {
	if len(m.Units) == 0 {
		return m.nextFile(1, false)
	}
	start, end := m.Unit, m.Unit+1
	if remaining {
		end = len(m.Units)
	}
	for i := start; i < end; i++ {
		m.Units[i].Skipped = true
		u := m.Units[i]
		for line := u.Start; line < u.End; line++ {
			delete(m.Marks, LineRef{u.Hunk, line})
		}
	}
	m.Visual = false
	m.notice("Skipped; changes remain in the working tree.", false)
	if end < len(m.Units) {
		m.jumpUnit(end)
		return nil
	}
	m.rebuildDiff(false)
	return m.nextFile(1, false)
}

func (m *Model) splitHunk() {
	if len(m.Units) == 0 {
		m.notice("No text hunk to split.", false)
		return
	}
	parts, err := splitUnit(m.activeFile(), m.Units[m.Unit])
	if err != nil {
		m.notice(err.Error(), false)
		return
	}
	units := append([]HunkUnit(nil), m.Units[:m.Unit]...)
	units = append(units, parts...)
	units = append(units, m.Units[m.Unit+1:]...)
	m.Units = units
	m.jumpUnit(m.Unit)
	m.notice(fmt.Sprintf("Split into %d selectable hunks.", len(parts)), false)
}

func (m *Model) resize(width, height int) {
	m.Width, m.Height = width, height
	m.TreeWidth = min(38, max(22, width/4))
	m.Viewport.Width = max(1, width-m.TreeWidth-1)
	m.Viewport.Height = max(1, height-6)
	m.Search.Width = max(1, m.TreeWidth-2)
	m.Tree.KeepVisible(m.treeHeight())
	m.rebuildDiff(true)
}

func (m *Model) beginSearch() tea.Cmd {
	m.Searching = true
	m.Focus = focusTree
	m.Search.SetValue(m.Tree.Query)
	cmd := m.Search.Focus()
	m.Tree.KeepVisible(m.treeHeight())
	return cmd
}

func (m *Model) updateSearch(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "esc":
		m.Searching = false
		m.Search.Blur()
		m.Search.SetValue("")
		m.Tree.Query = ""
		m.Tree.Rebuild(m.Snapshot.Files)
		m.Tree.KeepVisible(m.treeHeight())
		return nil
	case "enter":
		m.Searching = false
		m.Search.Blur()
		if row := m.Tree.Selected(); row != nil && !row.Dir {
			m.Focus = focusDiff
		}
		m.Tree.KeepVisible(m.treeHeight())
		return m.selectTreeFile()
	case "up", "down":
		delta := 1
		if key.String() == "up" {
			delta = -1
		}
		m.Tree.Move(delta)
		return m.selectTreeFile()
	}
	var cmd tea.Cmd
	m.Search, cmd = m.Search.Update(key)
	query := m.Search.Value()
	if query != m.Tree.Query {
		m.Tree.Query = query
		m.Tree.Rebuild(m.Snapshot.Files)
		// Auto-preview the first matching file rather than a directory row.
		if row := m.Tree.Selected(); row == nil || row.Dir {
			for i, row := range m.Tree.Rows {
				if !row.Dir {
					m.Tree.Cursor, m.Tree.CursorKey = i, row.Key()
					break
				}
			}
		}
		return tea.Batch(cmd, m.selectTreeFile())
	}
	return cmd
}

func (m *Model) updateTree(key string) tea.Cmd {
	switch key {
	case "j", "down":
		m.notice("", false)
		m.Tree.Move(1)
		return m.selectTreeFile()
	case "k", "up":
		m.notice("", false)
		m.Tree.Move(-1)
		return m.selectTreeFile()
	case "g", "home":
		m.Tree.Cursor = 0
		m.Tree.rememberCursor()
		return m.selectTreeFile()
	case "G", "end":
		m.Tree.Cursor = max(0, len(m.Tree.Rows)-1)
		m.Tree.rememberCursor()
		return m.selectTreeFile()
	case "enter", "l", "right":
		row := m.Tree.Selected()
		if row == nil {
			return nil
		}
		if row.Dir {
			name := row.Path
			if key == "enter" || m.Tree.Collapsed[name] {
				m.Tree.Collapsed[name] = !m.Tree.Collapsed[name]
				m.Tree.Rebuild(m.Snapshot.Files)
				m.Tree.KeepVisible(m.treeHeight())
			} else {
				m.Tree.Move(1)
				return m.selectTreeFile()
			}
		} else {
			m.Focus = focusDiff
			return m.selectTreeFile()
		}
	case "h", "left":
		row := m.Tree.Selected()
		if row == nil {
			return nil
		}
		if row.Dir && !m.Tree.Collapsed[row.Path] {
			m.Tree.Collapsed[row.Path] = true
		} else if parent := path.Dir(row.Path); parent != "." {
			m.Tree.CursorKey = parent + "/"
		}
		m.Tree.Rebuild(m.Snapshot.Files)
		m.Tree.KeepVisible(m.treeHeight())
	case "n":
		m.notice("", false)
		return m.nextFile(1, true)
	case "p":
		m.notice("", false)
		return m.nextFile(-1, true)
	case "s":
		m.SideBySide = !m.SideBySide
		m.rebuildDiff(true)
	case "y", "a", "d", "e":
		m.notice("Enter or Tab focuses the diff pane for hunk staging.", false)
	case "R":
		m.notice("Tab to the diff pane to revert a hunk, or X reverts the whole file.", false)
	}
	return nil
}

func (m *Model) updateDiff(key string) tea.Cmd {
	switch key {
	case "j", "down":
		m.notice("", false)
		m.moveDiffLine(1, false)
	case "k", "up":
		m.notice("", false)
		m.moveDiffLine(-1, false)
	case "J", "shift+down":
		m.moveDiffLine(1, true)
	case "K", "shift+up":
		m.moveDiffLine(-1, true)
	case "V":
		if len(m.Units) > 0 {
			m.Visual = !m.Visual
			m.Anchor = m.Line
			m.rebuildDiff(false)
		}
	case " ", "space":
		m.toggleMarks()
	case "u":
		m.Marks = make(map[LineRef]bool)
		m.Visual = false
		m.notice("Cleared line marks; nothing staged.", false)
		m.rebuildDiff(false)
	case "y":
		return m.stageSelection(false, false)
	case "enter":
		if len(m.Marks) > 0 {
			return m.stageSelection(false, false)
		}
		m.notice("y stages this hunk; Space marks individual lines first.", false)
	case "a":
		return m.stageSelection(true, false)
	case "n":
		return m.skipHunks(false)
	case "d":
		return m.skipHunks(true)
	case "p", "[":
		m.jumpUnit(m.Unit - 1)
	case "]":
		m.jumpUnit(m.Unit + 1)
	case "s":
		m.splitHunk()
	case "e":
		return m.editHunk()
	case "R":
		m.requestRevert()
	case "ctrl+d", "pgdown":
		m.scrollDiff(1)
	case "ctrl+u", "pgup":
		m.scrollDiff(-1)
	case "h":
		m.Horizontal = max(0, m.Horizontal-8)
		m.rebuildDiff(false)
	case "l":
		m.Horizontal += 8
		m.rebuildDiff(false)
	case "left":
		if m.effectiveSideBySide() {
			m.chooseSide(false)
		} else {
			m.Horizontal = max(0, m.Horizontal-8)
			m.rebuildDiff(false)
		}
	case "right":
		if m.effectiveSideBySide() {
			m.chooseSide(true)
		} else {
			m.Horizontal += 8
			m.rebuildDiff(false)
		}
	case "g", "home":
		m.jumpUnit(0)
	case "G", "end":
		m.jumpUnit(len(m.Units) - 1)
	}
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case watchMsg:
		cmds := []tea.Cmd{watchCmd(m.WatchInterval)}
		if m.Busy == "" && !m.Reloading {
			m.Reloading = true
			cmds = append(cmds, readSnapshotCmd(m.Repo, m.Epoch))
		}
		return m, tea.Batch(cmds...)
	case refreshMsg:
		if msg.Epoch != m.Epoch {
			return m, nil
		}
		m.Reloading = false
		if m.Busy == "loading" {
			m.Busy = ""
		}
		if msg.Err != nil {
			m.notice("Refresh failed: "+msg.Err.Error(), true)
			m.rebuildDiff(false)
			return m, nil
		}
		cmd := m.replaceSnapshot(msg.Snapshot, false, -1)
		return m, cmd
	case highlightMsg:
		if m.PendingHighlights[msg.Path] == msg.Fingerprint {
			delete(m.PendingHighlights, msg.Path)
		}
		f := m.Snapshot.File(msg.Path)
		if f == nil || f.Fingerprint != msg.Fingerprint {
			return m, nil
		}
		if msg.Err != nil {
			m.DeltaFailed = true
			m.notice(msg.Err.Error(), false)
			return m, nil
		}
		m.Colors[msg.Path] = msg.Result
		if m.ActivePath == msg.Path {
			m.rebuildDiff(false)
		}
		return m, nil
	case stagedMsg:
		if msg.Request.Epoch != m.Epoch {
			return m, nil
		}
		m.Busy = ""
		var cmd tea.Cmd
		if msg.HaveSnapshot {
			cmd = m.replaceSnapshot(msg.Snapshot, msg.Applied, msg.Request.Anchor)
		}
		if msg.Applied {
			m.Marks = make(map[LineRef]bool)
			m.Visual = false
			m.notice("Staged "+displayPath(msg.Request.Path)+". Worktree unchanged.", false)
		}
		if msg.Err != nil {
			m.notice(msg.Err.Error(), true)
			if msg.Applied && !msg.HaveSnapshot {
				m.Reloading = true
				cmd = tea.Batch(cmd, readSnapshotCmd(m.Repo, m.Epoch))
			}
		}
		m.rebuildDiff(false)
		return m, cmd
	case revertedMsg:
		if msg.Request.Epoch != m.Epoch {
			return m, nil
		}
		m.Busy = ""
		var cmd tea.Cmd
		if msg.HaveSnapshot {
			cmd = m.replaceSnapshot(msg.Snapshot, msg.Applied, msg.Request.Anchor)
		}
		if msg.Applied {
			m.Marks = make(map[LineRef]bool)
			m.Visual = false
			m.notice("Reverted "+msg.Request.Label+". The index is untouched.", false)
		}
		if msg.Err != nil {
			m.notice(msg.Err.Error(), true)
			if msg.Applied && !msg.HaveSnapshot {
				m.Reloading = true
				cmd = tea.Batch(cmd, readSnapshotCmd(m.Repo, m.Epoch))
			}
		}
		m.rebuildDiff(false)
		return m, cmd
	case editedMsg:
		if msg.Request.Epoch != m.Epoch {
			return m, nil
		}
		m.Busy = ""
		if msg.Err != nil {
			m.notice("Editor failed; nothing staged: "+msg.Err.Error(), true)
			return m, nil
		}
		patchText, err := ValidateEditedPatch(msg.Base, msg.Edited)
		if errors.Is(err, ErrNoChanges) {
			m.notice("Editor patch canceled; nothing staged.", false)
			return m, nil
		}
		if err != nil {
			m.notice(err.Error(), true)
			return m, nil
		}
		request := msg.Request
		request.Patch = patchText
		cmd := m.startStage(request)
		return m, cmd
	case tea.KeyMsg:
		key := msg.String()
		if m.Pending != nil {
			switch key {
			case "ctrl+c":
				m.Quitting = true
				return m, tea.Quit
			case "y", "Y", "enter":
				pending := m.Pending
				m.Pending = nil
				return m, m.startRevert(pending.Request)
			default:
				m.Pending = nil
				m.notice("Revert canceled; nothing changed.", false)
				return m, nil
			}
		}
		if key == "ctrl+c" || (!m.Searching && key == "q") {
			if m.Busy != "" {
				m.notice("Finishing the current operation; press q again when it completes.", false)
				return m, nil
			}
			m.Quitting = true
			return m, tea.Quit
		}
		if m.Help {
			switch key {
			case "?", "esc":
				m.Help = false
			case "j", "down", "ctrl+d":
				m.HelpOffset++
			case "k", "up", "ctrl+u":
				m.HelpOffset = max(0, m.HelpOffset-1)
			}
			return m, nil
		}
		if m.Busy != "" {
			if key == "?" {
				m.Help = true
			}
			return m, nil
		}
		if m.Searching {
			cmd := m.updateSearch(msg)
			return m, cmd
		}
		switch key {
		case "?":
			m.Help, m.HelpOffset = true, 0
			return m, nil
		case "tab", "shift+tab":
			if m.Focus == focusTree {
				m.Focus = focusDiff
			} else {
				m.Focus = focusTree
				m.Visual = false
				m.rebuildDiff(false)
			}
			m.notice("", false)
			return m, nil
		case "esc":
			if m.Visual {
				m.Visual = false
				m.rebuildDiff(false)
			} else if m.Tree.Query != "" {
				m.Tree.Query = ""
				m.Search.SetValue("")
				m.Tree.Rebuild(m.Snapshot.Files)
				m.Tree.KeepVisible(m.treeHeight())
			} else {
				m.Focus = focusTree
			}
			return m, nil
		case "/":
			cmd := m.beginSearch()
			return m, cmd
		case "N":
			cmd := m.nextFile(1, true)
			return m, cmd
		case "P":
			cmd := m.nextFile(-1, true)
			return m, cmd
		case "S":
			m.SideBySide = !m.SideBySide
			m.rebuildDiff(true)
			return m, nil
		case "r":
			if !m.Reloading {
				m.Reloading = true
				m.notice("", false)
				return m, readSnapshotCmd(m.Repo, m.Epoch)
			}
			return m, nil
		case "f":
			if m.Focus == focusTree {
				if row := m.Tree.Selected(); row == nil || row.Dir {
					m.notice("Select a file, not a directory, for whole-file staging.", false)
					return m, nil
				}
			}
			cmd := m.stageSelection(false, true)
			return m, cmd
		case "X":
			if m.Focus == focusTree {
				if row := m.Tree.Selected(); row == nil || row.Dir {
					m.notice("Select a file, not a directory, for whole-file revert.", false)
					return m, nil
				}
			}
			m.requestRevertFile()
			return m, nil
		}
		if m.Focus == focusTree {
			cmd := m.updateTree(key)
			return m, cmd
		}
		cmd := m.updateDiff(key)
		return m, cmd
	}
	if m.Searching {
		var cmd tea.Cmd
		m.Search, cmd = m.Search.Update(msg)
		return m, cmd
	}
	return m, nil
}

func run(args []string) error {
	flags := flag.NewFlagSet("patchtree", flag.ContinueOnError)
	options := Options{}
	flags.BoolVar(&options.Watch, "watch", false, "poll for unstaged changes")
	flags.DurationVar(&options.WatchInterval, "watch-interval", 1500*time.Millisecond, "watch polling interval (minimum 100ms)")
	flags.BoolVar(&options.SideBySide, "side-by-side", false, "start in side-by-side diff mode")
	flags.BoolVar(&options.NoDelta, "no-delta", false, "use basic colors without invoking delta")
	showVersion := flags.Bool("version", false, "print version")
	flags.Usage = func() {
		fmt.Fprint(flags.Output(), "Patchtree — interactive hunk and line staging\n\nUsage: patchtree [flags] [repository-directory]\n\n")
		flags.PrintDefaults()
		fmt.Fprintln(flags.Output(), "\nEnter a file to focus its diff. ? shows the full keymap.")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Println("patchtree " + version)
		return nil
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("expected at most one repository directory")
	}
	if options.WatchInterval < 100*time.Millisecond {
		return fmt.Errorf("--watch-interval must be at least 100ms")
	}
	dir := "."
	if flags.NArg() == 1 {
		dir = flags.Arg(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo, err := OpenRepository(ctx, dir)
	if err != nil {
		return err
	}
	model := NewModel(repo, options)
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "patchtree:", strings.ReplaceAll(safeText(err.Error()), "\n", " "))
		os.Exit(1)
	}
}
