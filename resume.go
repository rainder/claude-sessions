package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	// first prompt. The metadata lives in the first few entries.
	resumeHeadLines = 30
	// resumePromptMax is the rune budget for the first-prompt column.
	resumePromptMax = 60
)

// ResumableSession is one past transcript the picker can resume. Host is
// serialized out (json:"-"); the client tags it after fetching a remote list.
type ResumableSession struct {
	SessionID    string    `json:"session_id"`
	CWD          string    `json:"cwd"`
	GitBranch    string    `json:"git_branch,omitempty"`
	Name         string    `json:"name,omitempty"` // best-effort session name (user-set name or summary)
	FirstPrompt  string    `json:"first_prompt,omitempty"`
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
	summary     string // from a {"type":"summary","summary":"..."} line, if any
	sidechain   bool   // any entry marked isSidechain — a subagent transcript
	entrypoint  string // from the first entry carrying one: "cli" = interactive
}

// agentTranscript reports transcripts that belong to subagents or headless/SDK
// runs (Agent-tool sidechains, `claude -p` automation, SDK drivers) rather than
// an interactive session someone would resume. Older transcripts without an
// entrypoint field pass — absence is not evidence of automation.
func (h resumableHead) agentTranscript() bool {
	return h.sidechain || (h.entrypoint != "" && h.entrypoint != "cli")
}

// readResumableHead scans up to resumeHeadLines lines of a transcript for the
// first cwd, first gitBranch, and first genuine user prompt. It extends the
// head-scan approach of extractCWDFromJSONL (picker.go) to three fields in one
// pass. ok is false only when the file can't be opened; a readable file with no
// cwd yields ok=true with an empty cwd, and the caller drops it. Corrupt lines
// are skipped individually rather than aborting the scan.
func readResumableHead(path string) (resumableHead, bool) {
	f, err := os.Open(path)
	if err != nil {
		return resumableHead{}, false
	}
	defer f.Close()

	var head resumableHead
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for i := 0; scanner.Scan() && i < resumeHeadLines; i++ {
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
		if head.firstPrompt == "" && line.Type == "user" && !line.IsMeta && line.Message != nil {
			if text := firstPromptText(line.Message.Content); text != "" {
				head.firstPrompt = text
			}
		}
	}
	return head, true
}

// firstPromptText extracts display text from a user message's content, which is
// either a JSON string or an array of content blocks. Only "text" blocks
// contribute (tool results / images are ignored). Command and caveat wrappers
// (leading '<', e.g. <local-command-caveat>, <command-name>) are treated as
// non-prompts and yield "" so the caller falls through to the next user entry.
func firstPromptText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return cleanPrompt(str)
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
		return cleanPrompt(strings.Join(parts, " "))
	}
	return ""
}

// cleanPrompt whitespace-collapses s and truncates it to resumePromptMax runes.
// It returns "" for empty text or a command/caveat wrapper (leading '<'), so the
// caller keeps looking for a real prompt.
func cleanPrompt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || strings.HasPrefix(s, "<") {
		return ""
	}
	return truncateRunes(s, resumePromptMax)
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
type resumePickerState struct {
	Row      int
	RowCount int
	Filter   string
}

// handle applies one key event, reporting whether to confirm the selection or
// cancel. Up/Down move (wrapping); Enter confirms; Esc / Ctrl+C cancel;
// Backspace trims the filter; any printable ASCII byte extends it and resets the
// cursor to the top match.
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

// pickResumeSession drives the resume picker in a read/handle loop until the
// user confirms a row or cancels, returning the index into the original lines.
// Must be called in raw mode. Mirrors pickNewSession's single-stdin-consumer
// input path (readModalEvents on a persistent decoder).
func pickResumeSession(title, header string, lines []string, note string, wakes []wakeFD) (int, bool) {
	if len(lines) == 0 {
		return 0, false
	}
	state := resumePickerState{}
	renderer := newScreenRenderer(os.Stdout)
	decoder := newInputDecoder()
	fd := int(os.Stdin.Fd())

	sync := func() (filtered []string, indices []int) {
		filtered, indices = filterNewPickerLines(lines, state.Filter)
		state.RowCount = len(filtered)
		if state.Row >= state.RowCount {
			state.Row = 0
		}
		return filtered, indices
	}

	for {
		filtered, indices := sync()
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			cols, rows = 0, 0
		}
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
// note, and the footer.
func renderResumePicker(title, header string, lines []string, state resumePickerState, note string, rows int) string {
	var b strings.Builder
	b.WriteString("\n " + bold(title) + "\n\n")
	if state.Filter != "" {
		fmt.Fprintf(&b, " %s %s\n\n", dim("Filter:"), state.Filter)
	}
	b.WriteString(" " + dim(header) + "\n")
	if len(lines) == 0 {
		b.WriteString("   " + dim("(no matches)") + "\n")
	}
	start, end := resumeWindow(len(lines), state.Row, rows, state.Filter != "", note != "")
	for i := start; i < end; i++ {
		marker := "   "
		if i == state.Row {
			marker = " ▶ "
		}
		fmt.Fprintf(&b, "%s%s\n", marker, lines[i])
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
	return "↑/↓ move · type to filter · ⌫ edit · Enter resume · Esc cancel"
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
	// Size the picker rows to the terminal width once, up front. pickResumeSession
	// re-measures height per frame for its viewport, but the row layout (which
	// columns, how wide) is fixed here — a resize while the picker is open keeps
	// the width it opened with, which is acceptable.
	cols, _, err := term.GetSize(c.fd)
	if err != nil {
		cols = 0
	}
	lines, header := resumeRows(sessions, home, shortHostname(), cols, time.Now())
	note := ""
	if len(unreachable) > 0 {
		note = "unreachable: " + strings.Join(unreachable, ", ")
	}
	idx, ok := pickResumeSession("Resume a session", header, lines, note, c.modalWakes)
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
