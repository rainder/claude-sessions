package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// Resume-session picker: collect past (ended) Claude Code transcripts on this
// host and every configured server, let the user filter and pick one, then
// resume it via `claude --resume <id>` in a fresh tmux session on the host that
// owns the transcript.

const (
	// resumableMaxAge caps how far back a transcript can be modified and still
	// show up — old sessions are rarely worth resuming and the window keeps the
	// per-file line count (below) cheap.
	resumableMaxAge = 30 * 24 * time.Hour
	// resumableMaxCount bounds the picker list after mtime-desc sorting.
	resumableMaxCount = 100
	// resumeHeadLines is how many transcript lines to scan for cwd / branch /
	// first prompt. That metadata lives in the first few entries.
	resumeHeadLines = 30
	// resumePromptsScanLines bounds how far the scan runs looking for
	// resumePromptsMax prompts, once resumeHeadLines' cheaper fields are
	// already found. A real second or third prompt routinely lands past line
	// 30 — each turn's tool_use/tool_result entries push the next user line
	// well beyond the metadata window — so collecting more than the first
	// prompt needs a wider read.
	resumePromptsScanLines = 400
	// resumePromptMax is the rune budget for the first-prompt column.
	resumePromptMax = 60
	// resumePromptsMax is how many user prompts the scan keeps per
	// transcript, for the → detail overlay. Three is enough to tell two
	// same-repo sessions apart without turning the scan into a full read.
	resumePromptsMax = 3
	// resumePromptDetailMax is the rune budget for one prompt in that overlay.
	// Wider than the column budget: the box has most of the terminal to work
	// with, and the whole point of the overlay is to see more than the column.
	resumePromptDetailMax = 200
)

// ResumableSession is one past transcript the picker can resume. Host is
// serialized out (json:"-"); the client tags it after fetching a remote list.
type ResumableSession struct {
	SessionID    string    `json:"session_id"`
	CWD          string    `json:"cwd"`
	GitBranch    string    `json:"git_branch,omitempty"`
	Name         string    `json:"name,omitempty"` // best-effort session name (user-set name or summary)
	FirstPrompt  string    `json:"first_prompt,omitempty"`
	Prompts      []string  `json:"prompts,omitempty"` // first few user prompts, for the → overlay
	MessageCount int       `json:"message_count"`
	ModifiedAt   time.Time `json:"modified_at"`
	Host         string    `json:"-"` // "" local, set client-side for remote rows
}

// errResumeSessionLive is returned by ResumeSession when the requested session
// is already running. The server handler maps it to HTTP 409.
var errResumeSessionLive = errors.New("session is already live")

// CollectResumable scans this host's transcripts for resumable sessions,
// excluding any that are currently live. Cheap enough to run on demand.
func CollectResumable() []ResumableSession {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return collectResumableFrom(home, liveSessionIDs(), time.Now())
}

// collectResumableFrom is the testable core of CollectResumable: home is the
// directory holding .claude/projects, live is the set of session ids to skip,
// and now anchors the 30-day cutoff.
//
// Rules (per the resume design): glob ~/.claude/projects/*/*.jsonl; skip
// zero-byte files, transcripts modified more than resumableMaxAge ago, sessions
// in live, and unreadable/corrupt files or ones with no cwd in their head
// (cwd is required to spawn the resumed tmux session). A session id appearing
// under several project dirs (a worktree move) is deduped to its newest
// transcript, mirroring findTranscript. Sorted mtime-desc, capped at
// resumableMaxCount.
func collectResumableFrom(home string, live map[string]bool, now time.Time) []ResumableSession {
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	cutoff := now.Add(-resumableMaxAge)
	names := resumableNameMap(home)

	byID := make(map[string]ResumableSession, len(matches))
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Size() == 0 {
			continue
		}
		mtime := info.ModTime()
		if mtime.Before(cutoff) {
			continue
		}
		sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if sid == "" || live[sid] {
			continue
		}
		// Keep only the newest transcript per session id.
		if existing, ok := byID[sid]; ok && !mtime.After(existing.ModifiedAt) {
			continue
		}
		head, ok := readResumableHead(path)
		if !ok || head.cwd == "" || scratchCwd(head.cwd) || head.agentTranscript() {
			continue
		}
		// NAME is best-effort: a user-set name from a still-present session file
		// wins; otherwise a transcript summary; otherwise empty (rendered "-").
		name := names[sid]
		if name == "" {
			name = head.summary
		}
		byID[sid] = ResumableSession{
			SessionID:    sid,
			CWD:          head.cwd,
			GitBranch:    head.gitBranch,
			Name:         name,
			FirstPrompt:  head.firstPrompt,
			Prompts:      head.prompts,
			MessageCount: countFileLines(path),
			ModifiedAt:   mtime,
		}
	}

	out := make([]ResumableSession, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ModifiedAt.After(out[j].ModifiedAt)
	})
	if len(out) > resumableMaxCount {
		out = out[:resumableMaxCount]
	}
	return out
}

// resumableNameMap builds a sessionId→name lookup from the live-session JSON
// files under ~/.claude/sessions (reusing session.go's readSessionFile parser).
// Only user-set names are kept: a name is included when it's non-empty and its
// nameSource is present and not "derived" — derived names merely echo the cwd,
// which the DIR column already shows. These files usually vanish when a session
// ends, so hits are rare but authoritative when present.
func resumableNameMap(home string) map[string]string {
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "sessions", "*.json"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	names := make(map[string]string, len(matches))
	for _, p := range matches {
		s, ok := readSessionFile(p)
		if !ok || s.SessionID == "" || s.Name == "" {
			continue
		}
		if s.NameSource != "" && s.NameSource != "derived" {
			names[s.SessionID] = s.Name
		}
	}
	return names
}

// scratchCwd reports sessions run out of temp dirs — /tmp and /private (macOS's
// home for scratchpads and /tmp itself) — which aren't worth resuming. Narrower
// than picker.go's hiddenCwd on purpose: worktree checkouts stay resumable.
func scratchCwd(cwd string) bool {
	return cwd == "/tmp" || strings.HasPrefix(cwd, "/tmp/") ||
		cwd == "/private" || strings.HasPrefix(cwd, "/private/")
}

// resumableHead holds the fields pulled from a transcript's first lines.
type resumableHead struct {
	cwd         string
	gitBranch   string
	firstPrompt string
	// prompts are the first resumePromptsMax user prompts in the order
	// encountered, each bounded to resumePromptDetailMax runes. prompts[0] is
	// the same prompt as firstPrompt, only cut at the wider overlay budget.
	prompts    []string
	summary    string // from a {"type":"summary","summary":"..."} line, if any
	sidechain  bool   // any entry marked isSidechain — a subagent transcript
	entrypoint string // from the first entry carrying one: "cli" = interactive
}

// agentTranscript reports transcripts that belong to subagents or headless/SDK
// runs (Agent-tool sidechains, `claude -p` automation, SDK drivers) rather than
// an interactive session someone would resume. Older transcripts without an
// entrypoint field pass — absence is not evidence of automation.
func (h resumableHead) agentTranscript() bool {
	return h.sidechain || (h.entrypoint != "" && h.entrypoint != "cli")
}

// readResumableHead scans a transcript for the first cwd, first gitBranch, and
// up to resumePromptsMax genuine user prompts. cwd/gitBranch/summary/entrypoint
// are cheap and live in the first few entries, so those stop mattering past
// resumeHeadLines; prompts keep the scan going to resumePromptsScanLines
// because a real second or third prompt routinely sits much further in (each
// turn's tool_use/tool_result entries push the next user line well past the
// metadata window). It extends the head-scan approach of extractCWDFromJSONL
// (picker.go) to several fields in one pass. ok is false only when the file
// can't be opened; a readable file with no cwd yields ok=true with an empty
// cwd, and the caller drops it. Corrupt lines are skipped individually rather
// than aborting the scan.
func readResumableHead(path string) (resumableHead, bool) {
	f, err := os.Open(path)
	if err != nil {
		return resumableHead{}, false
	}
	defer f.Close()

	var head resumableHead
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for i := 0; scanner.Scan() && i < resumePromptsScanLines; i++ {
		if i >= resumeHeadLines && len(head.prompts) >= resumePromptsMax {
			break // past the metadata window and prompts are full: nothing left to gain
		}
		var line struct {
			Type        string `json:"type"`
			CWD         string `json:"cwd"`
			GitBranch   string `json:"gitBranch"`
			Summary     string `json:"summary"`
			IsMeta      bool   `json:"isMeta"`
			IsSidechain bool   `json:"isSidechain"`
			Entrypoint  string `json:"entrypoint"`
			Message     *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.IsSidechain {
			head.sidechain = true
		}
		if head.entrypoint == "" && line.Entrypoint != "" {
			head.entrypoint = line.Entrypoint
		}
		if head.cwd == "" && line.CWD != "" {
			head.cwd = line.CWD
		}
		if head.gitBranch == "" && line.GitBranch != "" {
			head.gitBranch = line.GitBranch
		}
		if head.summary == "" && line.Type == "summary" {
			// Collapse and bound the summary the same way as a prompt; it has no
			// caveat/command-wrapper concern so cleanPrompt's '<' rule is skipped.
			if s := strings.Join(strings.Fields(line.Summary), " "); s != "" {
				head.summary = truncateRunes(s, resumePromptMax)
			}
		}
		if len(head.prompts) < resumePromptsMax && line.Type == "user" && !line.IsMeta && line.Message != nil {
			if text := promptText(line.Message.Content); text != "" {
				if head.firstPrompt == "" {
					head.firstPrompt = truncateRunes(text, resumePromptMax)
				}
				head.prompts = append(head.prompts, truncateRunes(text, resumePromptDetailMax))
			}
		}
	}
	return head, true
}

// promptText extracts display text from a user message's content, which is
// either a JSON string or an array of content blocks. Only "text" blocks
// contribute (tool results / images are ignored). Command and caveat wrappers
// (leading '<', e.g. <local-command-caveat>, <command-name>) are treated as
// non-prompts and yield "" so the caller falls through to the next user entry.
// The text comes back whitespace-collapsed but untruncated: the column and the
// overlay bound it to different budgets, so the cut belongs to the caller.
func promptText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return cleanPromptText(str)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return cleanPromptText(strings.Join(parts, " "))
	}
	return ""
}

// cleanPromptText whitespace-collapses s, returning "" for empty text or a
// command/caveat wrapper (leading '<') so the caller keeps looking for a real
// prompt.
func cleanPromptText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || strings.HasPrefix(s, "<") {
		return ""
	}
	return s
}

// countFileLines returns the number of '\n' bytes in the file, the transcript's
// entry count. Streamed in 64KB chunks so a large transcript never loads whole
// into memory.
func countFileLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var count int
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		count += bytes.Count(buf[:n], []byte{'\n'})
		if err != nil {
			break
		}
	}
	return count
}

// liveSessionIDs returns the set of session ids currently live on this host, so
// CollectResumable and ResumeSession exclude/refuse a session that's already
// running. A CollectLocal error yields an empty set (fail open — listing a live
// session is better than hiding everything).
func liveSessionIDs() map[string]bool {
	set := map[string]bool{}
	sessions, err := CollectLocal()
	if err != nil {
		return set
	}
	for _, s := range sessions {
		if s.SessionID != "" {
			set[s.SessionID] = true
		}
	}
	return set
}

// resumeSessionIDRe constrains session ids to the UUID-ish charset Claude Code
// uses. sessionID reaches ResumeSession over HTTP and ends up both in a file
// lookup and a tmux send-keys line typed into a shell, so anything outside this
// set is rejected outright (no traversal, no shell metacharacters).
var resumeSessionIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ResumeSession validates the transcript for sessionID exists, refuses if the
// session is already live (errResumeSessionLive), then spawns a fresh tmux
// session running `claude --resume <id>` in cwd. Returns the tmux session name.
// Shared by the server handler and the local TUI path.
func ResumeSession(sessionID, cwd string) (string, error) {
	if sessionID == "" || cwd == "" {
		return "", fmt.Errorf("resume: session id and cwd required")
	}
	if !resumeSessionIDRe.MatchString(sessionID) {
		return "", fmt.Errorf("resume: invalid session id")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if findTranscript(home, sessionID) == "" {
		return "", fmt.Errorf("no transcript for session %s", sessionID)
	}
	if liveSessionIDs()[sessionID] {
		return "", errResumeSessionLive
	}
	tname := MakeTmuxName(cwd, sessionID, "")
	if err := exec.Command("tmux", "new-session", "-d", "-s", tname, "-c", cwd).Run(); err != nil {
		return "", fmt.Errorf("tmux new-session: %w", err)
	}
	if err := exec.Command("tmux", "send-keys", "-t", tname,
		"claude --resume "+sessionID, "Enter").Run(); err != nil {
		return "", fmt.Errorf("tmux send-keys: %w", err)
	}
	return tname, nil
}

// gatherResumable collects this host's resumable sessions plus each configured
// server's, fetched concurrently with a short per-host timeout so one slow or
// unreachable host can't stall the picker. Local rows come first (Host ""),
// then remote rows in config order; unreachable is the names of hosts whose
// fetch failed, for a picker footer note.
func gatherResumable() (sessions []ResumableSession, unreachable []string) {
	cfgs, _ := LoadServerConfigs()
	remoteResults := make([][]ResumableSession, len(cfgs))
	remoteErrs := make([]error, len(cfgs))
	var wg sync.WaitGroup
	for i, c := range cfgs {
		i, c := i, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			remoteResults[i], remoteErrs[i] = fetchRemoteResumable(c.Name)
		}()
	}
	// Compute the local list on this goroutine — it overlaps the remote fetches.
	local := CollectResumable()
	wg.Wait()

	sessions = append(sessions, local...)
	for i, c := range cfgs {
		if remoteErrs[i] != nil {
			unreachable = append(unreachable, c.Name)
			continue
		}
		for j := range remoteResults[i] {
			remoteResults[i][j].Host = c.Name
		}
		sessions = append(sessions, remoteResults[i]...)
	}
	// Each host's list arrives mtime-sorted, but the merge concatenates them;
	// re-sort so the aggregated picker is newest-first across hosts.
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})
	return sessions, unreachable
}

// resumeRows formats gathered sessions into aligned picker lines
// (AGE  HOST  NAME  DIR  BRANCH  #MSG  PROMPT) and the matching dimmed header.
// localHome collapses local-row dirs to "~"; remote-row dirs (home unknown)
// render raw. Column widths size to the data, capped so one long value can't
// blow out the layout. Metadata columns are dimmed; NAME, DIR, and PROMPT stay
// bright.
//
// The HOST column is shown only when at least one row is remote: an all-local
// list would just repeat this host's name on every row, so it's omitted outright
// (never dropped in the width ladder either — when present it's always
// meaningful).
//
// cols is the terminal width used for autocompaction. When the full layout would
// overflow cols, columns are shed/shrunk in this order until it fits: shrink
// PROMPT (down to a floor), drop #MSG, drop BRANCH, shrink DIR (down to a
// floor), shrink NAME (down to a floor). AGE, NAME, and DIR always survive; the
// header always mirrors the chosen columns and widths. cols<=0 (unknown) renders
// the full layout.
func resumeRows(sessions []ResumableSession, localHome, localName string, cols int, now time.Time) (lines []string, header string) {
	const (
		ageW      = 3
		hostCap   = 12
		nameCap   = 20
		dirCap    = 34
		branchCap = 18
		msgW      = 5
		nameMin   = 8
		dirMin    = 12
		promptMin = 10
		gap       = 2 // inter-column separator width
	)

	showHost := false
	for _, s := range sessions {
		if s.Host != "" {
			showHost = true
			break
		}
	}

	type cells struct{ age, host, name, dir, branch, msg, prompt string }
	rows := make([]cells, len(sessions))
	hostW, nameW, dirW, branchW := len("HOST"), len("NAME"), len("DIR"), len("BRANCH")
	promptW := len("PROMPT")
	for i, s := range sessions {
		host, dir := localName, collapseHome(s.CWD, localHome)
		if s.Host != "" {
			host, dir = s.Host, s.CWD // remote home unknown, show the raw path
		}
		name := s.Name
		if name == "" {
			name = "-"
		}
		branch := s.GitBranch
		if branch == "" {
			branch = "-"
		}
		prompt := s.FirstPrompt
		if prompt == "" {
			prompt = "-"
		}
		host = truncateRunes(host, hostCap)
		name = truncateRunes(name, nameCap)
		dir = truncateDirTail(dir, dirCap)
		branch = truncateRunes(branch, branchCap)
		prompt = truncateRunes(prompt, resumePromptMax)
		rows[i] = cells{
			age:    formatAge(now.Sub(s.ModifiedAt).Seconds()),
			host:   host,
			name:   name,
			dir:    dir,
			branch: branch,
			msg:    strconv.Itoa(s.MessageCount),
			prompt: prompt,
		}
		if n := utf8.RuneCountInString(host); n > hostW {
			hostW = n
		}
		if n := utf8.RuneCountInString(name); n > nameW {
			nameW = n
		}
		if n := utf8.RuneCountInString(dir); n > dirW {
			dirW = n
		}
		if n := utf8.RuneCountInString(branch); n > branchW {
			branchW = n
		}
		if n := utf8.RuneCountInString(prompt); n > promptW {
			promptW = n
		}
	}

	// Autocompact ladder. prefix is the width consumed by every column left of
	// PROMPT (including the separator that precedes PROMPT); PROMPT then takes
	// whatever remains. A stage "fits" once PROMPT can hold at least promptMin.
	showBranch, showMsg, showPrompt := true, true, true
	naturalPromptW := promptW
	prefix := func() int {
		w := ageW + gap + nameW + gap + dirW + gap
		if showHost {
			w += hostW + gap
		}
		if showBranch {
			w += branchW + gap
		}
		if showMsg {
			w += msgW + gap
		}
		return w
	}
	if cols > 0 {
		fits := func() bool { return cols-prefix() >= promptMin }
		if !fits() { // shrinking PROMPT alone isn't enough → drop #MSG
			showMsg = false
		}
		if !fits() { // still tight → drop BRANCH
			showBranch = false
		}
		if !fits() { // shrink DIR toward its floor
			dirW = shrinkToFit(dirW, dirMin, prefix()-(cols-promptMin))
		}
		if !fits() { // shrink NAME toward its floor
			nameW = shrinkToFit(nameW, nameMin, prefix()-(cols-promptMin))
		}
		// Resolve PROMPT to the remaining space. In pathologically narrow
		// terminals even the floors overflow; PROMPT then gets what's left, or is
		// dropped when nothing remains.
		avail := cols - prefix()
		switch {
		case avail < 1:
			showPrompt = false
		case avail < naturalPromptW:
			promptW = avail
		}
	}

	lines = make([]string, len(rows))
	for i, c := range rows {
		parts := make([]string, 0, 7)
		parts = append(parts, dim(padRight(c.age, ageW)))
		if showHost {
			parts = append(parts, dim(padRight(c.host, hostW)))
		}
		parts = append(parts, padRight(truncateRunes(c.name, nameW), nameW))
		parts = append(parts, padRight(truncateDirTail(c.dir, dirW), dirW))
		if showBranch {
			parts = append(parts, dim(padRight(c.branch, branchW)))
		}
		if showMsg {
			parts = append(parts, dim(padLeft(c.msg, msgW)))
		}
		if showPrompt {
			parts = append(parts, truncateRunes(c.prompt, promptW))
		}
		lines[i] = strings.Join(parts, strings.Repeat(" ", gap))
	}

	hparts := make([]string, 0, 7)
	hparts = append(hparts, padRight("AGE", ageW))
	if showHost {
		hparts = append(hparts, padRight("HOST", hostW))
	}
	hparts = append(hparts, padRight("NAME", nameW))
	hparts = append(hparts, padRight("DIR", dirW))
	if showBranch {
		hparts = append(hparts, padRight("BRANCH", branchW))
	}
	if showMsg {
		hparts = append(hparts, padLeft("#MSG", msgW))
	}
	if showPrompt {
		hparts = append(hparts, truncateRunes("PROMPT", promptW))
	}
	header = strings.Join(hparts, strings.Repeat(" ", gap))
	return lines, header
}

// shrinkToFit reduces a column width w by over runes to reclaim horizontal
// space, but never below floor (clamped to w when floor already exceeds it, so a
// naturally narrow column is left untouched). over<=0 is a no-op.
func shrinkToFit(w, floor, over int) int {
	if floor > w {
		floor = w
	}
	if over <= 0 {
		return w
	}
	if w-over < floor {
		return floor
	}
	return w - over
}

// truncateRunes shortens s to at most n runes, replacing the tail with "…" when
// it has to cut.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// truncateDirTail shortens a path to at most n runes, keeping the tail (the
// project/leaf dir, the useful part) behind a leading "…".
func truncateDirTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[len(r)-n:])
	}
	return "…" + string(r[len(r)-(n-1):])
}

// padRight pads s with spaces to n runes (no-op / truncation-free when already
// at least n wide — callers pre-truncate cells to their caps).
func padRight(s string, n int) string {
	if pad := n - utf8.RuneCountInString(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// padLeft right-aligns s within n runes.
func padLeft(s string, n int) string {
	if pad := n - utf8.RuneCountInString(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

// resumePickerState is the single-axis, filter-first state for the resume
// picker: Row selects a transcript and every printable keystroke extends the
// case-insensitive Filter (there is no command axis or quick-select, so digits
// and letters are all literal filter text). It reuses new_picker.go's filter
// engine (filterNewPickerLines) without the new-session picker's preset/prompt
// axes.
//
// ShowPrompts is a one-shot request raised by → and cleared by the loop that
// services it, the same shape newPickerState uses for its 'p' prompt overlay:
// the state stays pure and testable, the loop owns the terminal.
type resumePickerState struct {
	Row         int
	RowCount    int
	Filter      string
	ShowPrompts bool
}

// handle applies one key event, reporting whether to confirm the selection or
// cancel. Up/Down move (wrapping); Right raises ShowPrompts for the selected
// row; Enter confirms; Esc / Ctrl+C cancel; Backspace trims the filter; any
// printable ASCII byte extends it and resets the cursor to the top match.
func (s *resumePickerState) handle(key string) (confirm, cancel bool) {
	switch key {
	case KeyUp:
		if s.RowCount > 0 {
			s.Row = (s.Row + s.RowCount - 1) % s.RowCount
		}
	case KeyDown:
		if s.RowCount > 0 {
			s.Row = (s.Row + 1) % s.RowCount
		}
	case KeyRight:
		if s.RowCount > 0 {
			s.ShowPrompts = true
		}
	case "\r", "\n", KeyEnter:
		return true, false
	case KeyEsc, "\x03":
		return false, true
	case "\x7f", "\x08":
		if s.Filter != "" {
			s.Filter = s.Filter[:len(s.Filter)-1]
			s.Row = 0
		}
	default:
		if len(key) == 1 && key[0] >= 0x20 && key[0] <= 0x7e {
			s.Filter += key
			s.Row = 0
		}
	}
	return false, false
}

// resumeRowIndent is the left margin the column header and every picker row
// share. Rows used to sit 3 columns in behind a " ▶ " marker while the header
// sat 1 column in; the marker is gone (selection is a background highlight now)
// and both start at the same column. resumeRowCols hands resumeRows the width
// that is actually left for a row, so its compaction ladder budgets for the
// margin instead of letting screenRenderer clip the overflow.
const resumeRowIndent = 1

// resumeRowCols converts a terminal width into the width available to one
// picker row. cols<=0 ("unknown", the full-layout signal) passes through.
func resumeRowCols(cols int) int {
	switch {
	case cols <= 0:
		return 0
	case cols <= resumeRowIndent:
		return 1
	default:
		return cols - resumeRowIndent
	}
}

// pickResumeSession drives the resume picker in a read/handle loop until the
// user confirms a row or cancels, returning the index into sessions. Must be
// called in raw mode. Mirrors pickNewSession's single-stdin-consumer input path
// (readModalEvents on a persistent decoder).
//
// Rows are formatted per frame from the raw sessions rather than once up front.
// The loop re-reads the terminal size every iteration anyway (the viewport needs
// the height), and resumeRows' compaction ladder is the only thing that knows
// how to shed a column cleanly. Baking a width in at open time left a resize —
// or a stale initial read — to screenRenderer's blind right-edge clip, which
// chops a fully-computed column mid-value instead of dropping it.
//
// → opens the prompt overlay for the selected row (see resumePromptsOverlay).
// It runs its own renderer, so the picker's is invalidated on return to force a
// full repaint over the box.
func pickResumeSession(title string, sessions []ResumableSession, localHome, localName, note string, wakes []wakeFD) (int, bool) {
	if len(sessions) == 0 {
		return 0, false
	}
	state := resumePickerState{}
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	var lines []string
	sync := func() (filtered []string, indices []int) {
		filtered, indices = filterNewPickerLines(lines, state.Filter)
		state.RowCount = len(filtered)
		if state.Row >= state.RowCount {
			state.Row = 0
		}
		return filtered, indices
	}

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		var header string
		lines, header = resumeRows(sessions, localHome, localName, resumeRowCols(cols), time.Now())
		filtered, indices := sync()
		_ = renderer.Draw(renderResumePicker(title, header, filtered, state, note, rows), cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
			// Recompute before each key so RowCount and the index map reflect any
			// filter edit earlier in the same batch (mirrors pickNewSession).
			_, indices = sync()
			confirm, cancel := state.handle(key)
			if cancel {
				return 0, false
			}
			if state.ShowPrompts {
				state.ShowPrompts = false
				if state.RowCount > 0 {
					resumePromptsOverlay(sessions[indices[state.Row]], wakes)
					renderer.Invalidate()
				}
				continue
			}
			if confirm {
				if state.RowCount == 0 {
					continue // nothing matches the filter; ignore the confirm
				}
				return indices[state.Row], true
			}
		}
	}
}

// renderResumePicker draws the picker: title, optional filter echo, the dimmed
// column header, a viewport of rows windowed around the selection, an optional
// note, and the footer. The selected row carries the same background highlight
// the session list uses (highlightSelectedRow), so no glyph column is reserved
// and rows line up with the header.
func renderResumePicker(title, header string, lines []string, state resumePickerState, note string, rows int) string {
	indent := strings.Repeat(" ", resumeRowIndent)
	var b strings.Builder
	b.WriteString("\n " + bold(title) + "\n\n")
	if state.Filter != "" {
		fmt.Fprintf(&b, " %s %s\n\n", dim("Filter:"), state.Filter)
	}
	b.WriteString(indent + dim(header) + "\n")
	if len(lines) == 0 {
		b.WriteString(indent + dim("(no matches)") + "\n")
	}
	start, end := resumeWindow(len(lines), state.Row, rows, state.Filter != "", note != "")
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%s\n", highlightSelectedRow(indent+lines[i], i == state.Row))
	}
	if note != "" {
		b.WriteString("\n " + dim(note) + "\n")
	}
	b.WriteString("\n " + dim(resumeFooter()) + "\n")
	return b.String()
}

// resumeWindow returns the [start,end) slice of list rows to draw so the
// selected row stays on screen under a known terminal height. rows<=0 (unknown
// height) shows everything. The chrome estimate is deliberately conservative so
// the selected row is never pushed into the cropped region.
func resumeWindow(total, sel, rows int, hasFilter, hasNote bool) (start, end int) {
	if total == 0 {
		return 0, 0
	}
	if rows <= 0 {
		return 0, total
	}
	above := 3 + 1 // title block (blank/title/blank) + header
	if hasFilter {
		above += 2
	}
	below := 2 + 1 // footer block + one row of safety margin
	if hasNote {
		below += 2
	}
	capacity := rows - above - below
	if capacity < 1 {
		capacity = 1
	}
	if capacity >= total {
		return 0, total
	}
	start = sel - capacity/2
	if start < 0 {
		start = 0
	}
	if start > total-capacity {
		start = total - capacity
	}
	return start, start + capacity
}

func resumeFooter() string {
	return "↑/↓ move · → prompts · type to filter · ⌫ edit · Enter resume · Esc cancel"
}

const (
	// resumePromptsInner is the inner width the prompt overlay aims for. Prose
	// needs room to stay legible — the same reasoning as previewBoxMinInner —
	// and a narrower terminal clamps it down.
	resumePromptsInner = 68
	// resumePromptsHint is the fixed footer row inside the box.
	resumePromptsHint = "Esc close"
	// resumePromptsChrome is every box row that is not a prompt line: the two
	// borders, the title, the cwd row, the divider, the blank separator and the
	// hint.
	resumePromptsChrome = 7
	// resumePromptsMargin is how many rows of terminal the box leaves unused, so
	// it reads as an overlay rather than a full-screen takeover (mirrors
	// previewBoxMargin).
	resumePromptsMargin = 2
)

// resumePromptsOverlay shows the selected session's first few user prompts in a
// blocking bordered box, mirroring confirmOverlay's loop: it never leaves raw
// mode or the alt-screen, and returns to the picker with the selection
// untouched once a close key arrives. The data was collected with the list, so
// there is nothing to fetch — wakes is passed through only so a live resize
// re-centers the box.
func resumePromptsOverlay(s ResumableSession, wakes []wakeFD) {
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	for {
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
		_ = renderer.Draw(renderResumePromptsOverlay(s, cols, rows), cols, rows)
		keys, _ := readModalEvents(decoder, wakes)
		for _, key := range keys {
			if resumePromptsClose(key) {
				return
			}
		}
	}
}

// resumePromptsClose reports whether key dismisses the prompt overlay. Esc is
// the documented key; Enter, ←, q and Ctrl-C are accepted too, since the
// overlay has nothing else to do with them. Every other key is ignored rather
// than closing, so a burst of filter typing can't tear through the box and land
// in the picker.
func resumePromptsClose(key string) bool {
	switch key {
	case KeyEsc, "\x03", KeyEnter, "\r", "\n", KeyLeft, "q", "Q":
		return true
	}
	return false
}

// renderResumePromptsOverlay draws the prompt box centered in a cols x rows
// terminal, reusing renderConfirmOverlay's geometry: same box glyphs, same
// clamp-and-clip on a narrow terminal, same unpositioned top-left fallback when
// the size is unknown (<=0). Content that outgrows the terminal height is cut
// with a trailing "…" row rather than pushing the bottom border off-screen.
func renderResumePromptsOverlay(s ResumableSession, cols, rows int) string {
	innerWidth := resumePromptsInner
	if cols > 0 {
		max := cols - 4 // border + 1 space of padding on each side
		if max < 1 {
			max = 1
		}
		if innerWidth > max {
			innerWidth = max
		}
	}

	// body is freshly allocated per call, so the trim can rewrite its last row
	// in place to mark that content was cut.
	body := resumePromptsBody(s, innerWidth)
	if rows > 0 {
		capacity := rows - resumePromptsChrome - resumePromptsMargin
		if capacity < 1 {
			capacity = 1
		}
		if capacity < len(body) {
			body = body[:capacity]
			body[capacity-1] = dim("…")
		}
	}

	pad := func(l string) string {
		l = clipLine(l, innerWidth)
		return confirmBoxV + " " + l + strings.Repeat(" ", innerWidth-visualLen(l)) + " " + confirmBoxV
	}

	box := make([]string, 0, len(body)+resumePromptsChrome)
	box = append(box, confirmBoxTL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxTR)
	box = append(box, pad(bold(resumePromptsTitle(s))), pad(dim(s.CWD)))
	box = append(box, pad(strings.Repeat(confirmBoxH, innerWidth)))
	for _, l := range body {
		box = append(box, pad(l))
	}
	box = append(box, pad(""), pad(dim(resumePromptsHint)))
	box = append(box, confirmBoxBL+strings.Repeat(confirmBoxH, innerWidth+2)+confirmBoxBR)

	if cols <= 0 || rows <= 0 {
		return strings.Join(box, "\n")
	}

	boxWidth := innerWidth + 4
	left := (cols - boxWidth) / 2
	if left < 0 {
		left = 0
	}
	leftPad := strings.Repeat(" ", left)
	for i, l := range box {
		box[i] = leftPad + l
	}

	top := (rows - len(box)) / 2
	if top < 0 {
		top = 0
	}
	lines := make([]string, 0, top+len(box))
	for i := 0; i < top; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, box...)
	return strings.Join(lines, "\n")
}

// resumePromptsTitle identifies the session the box belongs to: its name when
// it has one, else the short id, host-qualified for a remote row.
func resumePromptsTitle(s ResumableSession) string {
	title := s.Name
	if title == "" {
		title = shortID(s.SessionID)
	}
	if s.Host != "" {
		title = s.Host + ":" + title
	}
	return title
}

// resumePromptsBody word-wraps the collected prompts to width, numbering each
// and separating them with a blank row. A session with none — an old server
// that predates the field, or a transcript whose head held no real prompt —
// gets a dimmed placeholder instead.
func resumePromptsBody(s ResumableSession, width int) []string {
	if len(s.Prompts) == 0 {
		return []string{dim("no prompts recorded for this session")}
	}
	const gutter = "   " // width of the "N. " marker, for continuation rows
	out := make([]string, 0, len(s.Prompts)*3)
	for i, p := range s.Prompts {
		if i > 0 {
			out = append(out, "")
		}
		for j, l := range wrapRunes(p, width-len(gutter)) {
			if j == 0 {
				out = append(out, dim(strconv.Itoa(i+1)+".")+" "+l)
				continue
			}
			out = append(out, gutter+l)
		}
	}
	return out
}

// wrapRunes splits s into lines of at most width runes, breaking between words
// where it can and mid-word only for a token too long to ever fit. width<1
// (a terminal too narrow to wrap into) returns s unsplit and leaves the clip to
// the caller.
func wrapRunes(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	var lines []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			lines = append(lines, string(cur))
			cur = nil
		}
	}
	for _, word := range strings.Fields(s) {
		w := []rune(word)
		for len(w) > width {
			flush()
			lines = append(lines, string(w[:width]))
			w = w[width:]
		}
		switch {
		case len(cur) == 0:
			cur = w
		case len(cur)+1+len(w) > width:
			flush()
			cur = w
		default:
			cur = append(append(cur, ' '), w...)
		}
	}
	flush()
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// writeResumeLoading paints the one-shot status line actResume shows while it
// blocks on gatherResumable. Raw mode is still on and the alt-screen still
// holds the session list, so this is a bare home + erase-display write in the
// style of writeActionOutputPosition (actions.go), not a fmt.Print into a
// cooked terminal. Nothing clears it explicitly: whatever comes next — the
// picker's first frame, or the TUI's repaint after a cancel — is a full redraw,
// since both renderers start invalidated.
func writeResumeLoading(w io.Writer) {
	_, _ = io.WriteString(w, "\x1b[H\x1b[2J\n "+dim("Loading sessions…")+"\n")
}

// shortID abbreviates a session UUID for a status line.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// actResume opens the resume picker: it gathers resumable transcripts from this
// host and every configured server (concurrently, short per-host timeout), lets
// the user filter and pick one, then resumes it in a fresh tmux session on the
// owning host and attaches. Unlike the row-targeted actions it ignores the
// current selection — it's a global entry point bound to 'r'.
func actResume(c *actCtx) {
	// gatherResumable blocks on every configured host (5s apiece before a slow
	// or unreachable one gives up), so say something first — an unchanged TUI
	// that stops answering keys reads as a hang.
	writeResumeLoading(terminalOutput)
	sessions, unreachable := gatherResumable()
	if len(sessions) == 0 {
		c.prepareLineOutput()
		if len(unreachable) > 0 {
			fmt.Printf("\nno resumable sessions (unreachable: %s)\n", strings.Join(unreachable, ", "))
		} else {
			fmt.Print("\nno resumable sessions\n")
		}
		pauseForKey(c.fd, c.oldState)
		c.enterRaw()
		return
	}

	home, _ := os.UserHomeDir()
	note := ""
	if len(unreachable) > 0 {
		note = "unreachable: " + strings.Join(unreachable, ", ")
	}
	// The rows are formatted inside the picker, per frame, so a resize relays
	// them out through resumeRows' compaction ladder instead of being clipped.
	idx, ok := pickResumeSession("Resume a session", sessions, home, shortHostname(), note, c.modalWakes)
	if !ok {
		return
	}
	sel := sessions[idx]

	c.prepareLineOutput()
	defer c.enterRaw()

	if sel.Host == "" {
		fmt.Printf("\nresuming %s in %s... ", shortID(sel.SessionID), collapseHome(sel.CWD, home))
		tname, err := ResumeSession(sel.SessionID, sel.CWD)
		if err != nil {
			fmt.Printf("failed: %v\n", err)
			pauseForKey(c.fd, c.oldState)
			return
		}
		c.spawnedTmux = tname
		fmt.Printf("ok → %s\n", tname)
		c.enterRaw()
		runTmuxAttach(c, tname)
		return
	}

	// Remote: ask the owning host to resume, then ssh-attach like the remote
	// new-session flow.
	fmt.Printf("\nresuming %s on %s... ", shortID(sel.SessionID), sel.Host)
	body, _ := json.Marshal(map[string]string{
		"session_id": sel.SessionID,
		"cwd":        sel.CWD,
	})
	resp, err := remoteRequest(sel.Host, "/sessions/resume", "POST", body)
	if err != nil {
		fmt.Printf("failed: %v\n", err)
		pauseForKey(c.fd, c.oldState)
		return
	}
	var r actionResult
	_ = json.Unmarshal(resp, &r)
	if !r.OK || r.Tmux == "" {
		fmt.Printf("failed: %s\n", r.Error)
		pauseForKey(c.fd, c.oldState)
		return
	}
	c.spawnedHost = sel.Host
	c.spawnedTmux = r.Tmux
	fmt.Printf("ok → %s\n", r.Tmux)
	srv, _ := LookupServer(sel.Host)
	c.enterRaw()
	_ = runRemoteAttach(c, srv, r.Tmux)
}
