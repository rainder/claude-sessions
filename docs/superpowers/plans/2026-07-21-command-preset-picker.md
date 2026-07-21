# Command Preset Picker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add configurable command presets to the `n` modal, cycle them with left/right arrows, execute them safely for local and remote spawns, and show on-demand remote CWD history.

**Architecture:** Parse `commands:` and `servers:` from the existing restricted YAML file, persist the last confirmed preset name, and use a dedicated two-axis picker state. Local spawns receive resolved command text; remote requests send only a preset name, which the server resolves against its own allowlist. Remote CWD suggestions come from an authenticated on-demand endpoint that shares ranking logic with the local picker.

**Tech Stack:** Go standard library, `net/http`, tmux subprocesses, existing hand-written YAML parser and raw-terminal event loop.

## Global Constraints

- Preserve all unrelated uncommitted changes, including empty-host selection and server-side tilde expansion.
- Do not commit, stage, reset, restore, or clean files unless the user explicitly requests it.
- Add no dependencies.
- Keep `servers.yaml` as the configuration filename.
- Missing or invalid command config must fall back to `Claude` → `claude`.
- Remote clients send preset names only; raw command text never crosses `/sessions/new`.
- Remote servers execute only commands present in their own configured allowlist.
- Command strings are trusted administrator-authored shell text typed into tmux.
- Migration/resume keeps its existing `claude --resume` behavior.
- Remote CWD suggestions are fetched only when `n` opens a remote modal, using a 5-second timeout.
- Suggestion failure must not block manual remote creation.
- Existing single-stdin-consumer and raw-mode/OPOST invariants remain intact.

## File Structure

- Modify `yaml.go`: combined config parser, command preset validation, loaders, lookup.
- Create `yaml_test.go`: config parsing, fallback, duplicate validation, host compatibility.
- Modify `config.go`: remembered preset load/save/index resolution.
- Modify `config_test.go`: preset persistence and stale-name fallback.
- Modify `picker.go`: shared ranked CWD collection and remote merge helper.
- Modify `picker_test.go`: ranking/filtering/merge tests.
- Modify `server.go`: authenticated CWD endpoint, command preset allowlist in new-session handler, route registration.
- Modify `server_test.go`: suggestion endpoint and command allowlist handler tests.
- Create `new_picker.go`: pure two-axis picker state, rendering, blocking modal wrapper.
- Create `new_picker_test.go`: key transitions, confirmation, cancellation, rendering.
- Modify `migrate.go`: parameterize `SpawnNew` command text.
- Create `migrate_test.go`: fake-tmux argv verification.
- Modify `actions.go`: local modal integration and selected command propagation.
- Modify `remote_actions.go`: 5-second suggestion fetch, remote modal integration, preset-name payload.
- Modify `commands.go`: shell `new` uses first configured preset.
- Modify `tui.go`: help copy.
- Modify `README.md`: YAML commands, remote matching names, arrow controls, remote suggestions, trusted command warning.

---

### Task 1: Parse and Persist Command Presets

**Files:**
- Modify: `yaml.go:9-155`
- Create: `yaml_test.go`
- Modify: `config.go:1-65`
- Modify: `config_test.go`

**Interfaces:**
- Produces:
  - `type CommandPreset struct { Name string; Command string }`
  - `func LoadCommandPresets() ([]CommandPreset, error)`
  - `func findCommandPreset(presets []CommandPreset, name string) (CommandPreset, bool)`
  - `func commandPresetIndex(presets []CommandPreset, remembered string) int`
  - `func LoadCommandPresetIndex(presets []CommandPreset) int`
  - `func SaveCommandPresetName(name string)`
- Preserves: `LoadServerConfigs() ([]ServerConfig, error)`.

- [ ] **Step 1: Write failing YAML parser tests**

Create `yaml_test.go`:

```go
package main

import (
    "reflect"
    "testing"
)

func TestParseConfigYAMLCommandsAndServers(t *testing.T) {
    cfg := parseConfigYAML(`
commands:
  - name: Claude
    command: claude
  - name: Fable
    command: "claude --model fable"
servers:
  - name: beluga
    host: 100.64.0.2
    port: 9000
    token: secret
`)

    wantCommands := []CommandPreset{
        {Name: "Claude", Command: "claude"},
        {Name: "Fable", Command: "claude --model fable"},
    }
    if !reflect.DeepEqual(cfg.Commands, wantCommands) {
        t.Fatalf("commands = %#v, want %#v", cfg.Commands, wantCommands)
    }
    if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "beluga" || cfg.Servers[0].Port != 9000 {
        t.Fatalf("servers = %#v", cfg.Servers)
    }
}

func TestParseConfigYAMLCommandValidation(t *testing.T) {
    cfg := parseConfigYAML(`
commands:
  - name: "  Fable  "
    command: "  claude --model fable  "
  - name: Fable
    command: ignored-duplicate
  - name: ""
    command: blank-name
  - name: BlankCommand
    command: ""
`)
    want := []CommandPreset{{Name: "Fable", Command: "claude --model fable"}}
    if !reflect.DeepEqual(cfg.Commands, want) {
        t.Fatalf("commands = %#v, want %#v", cfg.Commands, want)
    }
}

func TestParseConfigYAMLCommandFallback(t *testing.T) {
    for _, input := range []string{"", "servers:\n", "commands:\n  - name: broken\n"} {
        cfg := parseConfigYAML(input)
        want := []CommandPreset{{Name: "Claude", Command: "claude"}}
        if !reflect.DeepEqual(cfg.Commands, want) {
            t.Fatalf("parseConfigYAML(%q).Commands = %#v, want %#v", input, cfg.Commands, want)
        }
    }
}

func TestFindCommandPreset(t *testing.T) {
    presets := []CommandPreset{{Name: "Claude", Command: "claude"}, {Name: "Fable", Command: "claude --model fable"}}
    got, ok := findCommandPreset(presets, "Fable")
    if !ok || got.Command != "claude --model fable" {
        t.Fatalf("findCommandPreset = %#v, %v", got, ok)
    }
    if _, ok := findCommandPreset(presets, "missing"); ok {
        t.Fatal("missing preset unexpectedly resolved")
    }
}
```

- [ ] **Step 2: Verify YAML tests fail for missing symbols**

Run:

```sh
go test -count=1 ./... -run 'Test(ParseConfigYAML|FindCommandPreset)'
```

Expected: compile failure naming `parseConfigYAML`, `CommandPreset`, or `findCommandPreset`.

- [ ] **Step 3: Implement combined parser and loaders**

In `yaml.go`, add:

```go
type CommandPreset struct {
    Name    string
    Command string
}

type appConfig struct {
    Servers  []ServerConfig
    Commands []CommandPreset
}

func defaultCommandPresets() []CommandPreset {
    return []CommandPreset{{Name: "Claude", Command: "claude"}}
}

func findCommandPreset(presets []CommandPreset, name string) (CommandPreset, bool) {
    for _, preset := range presets {
        if preset.Name == name {
            return preset, true
        }
    }
    return CommandPreset{}, false
}
```

Replace the ServerConfig-only parser with a block-aware parser. Use this exact state shape:

```go
func parseConfigYAML(input string) appConfig {
    var cfg appConfig
    block := ""
    var server *ServerConfig
    var command *CommandPreset

    flushServer := func() {
        if server != nil {
            cfg.Servers = append(cfg.Servers, *server)
            server = nil
        }
    }
    flushCommand := func() {
        if command == nil {
            return
        }
        command.Name = strings.TrimSpace(command.Name)
        command.Command = strings.TrimSpace(command.Command)
        if command.Name != "" && command.Command != "" {
            if _, exists := findCommandPreset(cfg.Commands, command.Name); !exists {
                cfg.Commands = append(cfg.Commands, *command)
            }
        }
        command = nil
    }
    flush := func() {
        flushServer()
        flushCommand()
    }

    for _, raw := range strings.Split(input, "\n") {
        line := strings.TrimRight(raw, " \t\r")
        stripped := strings.TrimLeft(line, " \t")
        if stripped == "" || strings.HasPrefix(stripped, "#") {
            continue
        }
        indent := len(line) - len(stripped)
        if indent == 0 && strings.Contains(line, ":") {
            flush()
            block = strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
            continue
        }
        if block != "servers" && block != "commands" {
            continue
        }
        if strings.HasPrefix(stripped, "- ") {
            flush()
            if block == "servers" {
                server = &ServerConfig{Port: 8765, Enable: true}
            } else {
                command = &CommandPreset{}
            }
            stripped = strings.TrimSpace(stripped[2:])
        }
        key, val, ok := strings.Cut(stripped, ":")
        if !ok {
            continue
        }
        key, val = strings.TrimSpace(key), trimYAMLValue(val)
        if block == "servers" && server != nil {
            setField(server, key, val)
        }
        if block == "commands" && command != nil {
            switch key {
            case "name":
                command.Name = val
            case "command":
                command.Command = val
            }
        }
    }
    flush()
    if len(cfg.Commands) == 0 {
        cfg.Commands = defaultCommandPresets()
    }
    return cfg
}

func parseServersYAML(input string) []ServerConfig {
    return parseConfigYAML(input).Servers
}
```

Factor file reading:

```go
func loadAppConfig() (appConfig, error) {
    home, err := os.UserHomeDir()
    if err != nil {
        return appConfig{}, err
    }
    data, err := os.ReadFile(filepath.Join(home, ".config", "claude-sessions", "servers.yaml"))
    if os.IsNotExist(err) {
        return appConfig{Commands: defaultCommandPresets()}, nil
    }
    if err != nil {
        return appConfig{}, err
    }
    return parseConfigYAML(string(data)), nil
}

func LoadCommandPresets() ([]CommandPreset, error) {
    cfg, err := loadAppConfig()
    if err != nil {
        return nil, err
    }
    return cfg.Commands, nil
}
```

Rewrite `LoadServerConfigs` to call `loadAppConfig`, then filter `Enable` exactly as before. Add `path/filepath` import.

- [ ] **Step 4: Write failing preset persistence tests**

Append to `config_test.go`:

```go
func TestCommandPresetIndex(t *testing.T) {
    presets := []CommandPreset{{Name: "Claude"}, {Name: "Fable"}}
    if got := commandPresetIndex(presets, "Fable"); got != 1 {
        t.Fatalf("valid remembered index = %d, want 1", got)
    }
    if got := commandPresetIndex(presets, "removed"); got != 0 {
        t.Fatalf("stale remembered index = %d, want 0", got)
    }
    if got := commandPresetIndex(nil, "Fable"); got != 0 {
        t.Fatalf("empty preset index = %d, want 0", got)
    }
}

func TestCommandPresetNameRoundTrip(t *testing.T) {
    t.Setenv("HOME", t.TempDir())
    SaveCommandPresetName("Fable")
    presets := []CommandPreset{{Name: "Claude"}, {Name: "Fable"}}
    if got := LoadCommandPresetIndex(presets); got != 1 {
        t.Fatalf("loaded preset index = %d, want 1", got)
    }
}
```

- [ ] **Step 5: Verify persistence tests fail**

Run:

```sh
go test -count=1 ./... -run 'TestCommandPreset(Index|NameRoundTrip)'
```

Expected: compile failure for missing persistence functions.

- [ ] **Step 6: Implement remembered preset state**

Append to `config.go`:

```go
func commandPresetIndex(presets []CommandPreset, remembered string) int {
    for i, preset := range presets {
        if preset.Name == remembered {
            return i
        }
    }
    return 0
}

func LoadCommandPresetIndex(presets []CommandPreset) int {
    data, err := os.ReadFile(filepath.Join(ConfigDir(), "command-preset"))
    if err != nil {
        return 0
    }
    return commandPresetIndex(presets, strings.TrimSpace(string(data)))
}

func SaveCommandPresetName(name string) {
    dir := ConfigDir()
    if dir == "" {
        return
    }
    _ = os.MkdirAll(dir, 0o755)
    _ = os.WriteFile(filepath.Join(dir, "command-preset"), []byte(name+"\n"), 0o644)
}
```

- [ ] **Step 7: Run config tests and full suite**

```sh
gofmt -w yaml.go yaml_test.go config.go config_test.go
go test -count=1 ./... -run 'Test(ParseConfigYAML|FindCommandPreset|CommandPreset)'
go test -count=1 ./...
```

Expected: PASS.

- [ ] **Step 8: Review scoped diff**

```sh
git diff -- yaml.go yaml_test.go config.go config_test.go
```

Expected: command config/parser/persistence changes only. Do not commit.

---

### Task 2: Share CWD Ranking and Expose Remote Suggestions

**Files:**
- Modify: `picker.go:13-116`
- Modify: `picker_test.go`
- Modify: `server.go:28-58,287-295`
- Modify: `server_test.go`
- Modify: `remote_actions.go:19-50`

**Interfaces:**
- Produces:
  - `type cwdSuggestion struct { CWD string; Count int }` with JSON tags.
  - `func collectCwdSuggestions() []cwdSuggestion`
  - `func mergeRemoteCwdEntries(defaultCWD string, suggestions []cwdSuggestion) []cwdEntry`
  - `func fetchRemoteCwdSuggestions(host string) ([]cwdSuggestion, error)`
  - authenticated `GET /cwd-suggestions`.
- Preserves local `buildCwdPicker(selected *Session) cwdPicker` behavior except stale missing session paths are now filtered.

- [ ] **Step 1: Write failing collection and merge tests**

Append to `picker_test.go`:

```go
func TestCollectCwdSuggestionsFiltersAndRanks(t *testing.T) {
    home := t.TempDir()
    t.Setenv("HOME", home)
    high := filepath.Join(home, "high")
    low := filepath.Join(home, "low")
    missing := filepath.Join(home, "missing")
    if err := os.MkdirAll(high, 0o755); err != nil { t.Fatal(err) }
    if err := os.MkdirAll(low, 0o755); err != nil { t.Fatal(err) }
    sessions := filepath.Join(home, ".claude", "sessions")
    if err := os.MkdirAll(sessions, 0o755); err != nil { t.Fatal(err) }
    write := func(name, cwd string, pid int) {
        t.Helper()
        data := fmt.Sprintf(`{"pid":%d,"cwd":%q}`, pid, cwd)
        if err := os.WriteFile(filepath.Join(sessions, name), []byte(data), 0o644); err != nil { t.Fatal(err) }
    }
    write("1.json", high, 1)
    write("2.json", high, 2)
    write("3.json", low, 3)
    write("4.json", missing, 4)

    got := collectCwdSuggestions()
    want := []cwdSuggestion{{CWD: high, Count: 2}, {CWD: low, Count: 1}}
    if !reflect.DeepEqual(got, want) {
        t.Fatalf("suggestions = %#v, want %#v", got, want)
    }
}

func TestMergeRemoteCwdEntries(t *testing.T) {
    suggestions := []cwdSuggestion{{CWD: "/a", Count: 3}, {CWD: "/b", Count: 2}}
    got := mergeRemoteCwdEntries("/b", suggestions)
    want := []cwdEntry{{cwd: "/b", count: 2, isDefault: true}, {cwd: "/a", count: 3}}
    if !reflect.DeepEqual(got, want) {
        t.Fatalf("merged entries = %#v, want %#v", got, want)
    }
}
```

Update imports to include `fmt`, `os`, `path/filepath`, and `reflect`.

- [ ] **Step 2: Verify collection tests fail**

```sh
go test -count=1 ./... -run 'Test(CollectCwdSuggestions|MergeRemoteCwdEntries)'
```

Expected: compile failure for missing type/functions.

- [ ] **Step 3: Extract ranked collection and merge helper**

In `picker.go`, add:

```go
type cwdSuggestion struct {
    CWD   string `json:"cwd"`
    Count int    `json:"count"`
}
```

Move the session JSON + transcript count collection into `collectCwdSuggestions`. Apply `isDir` to session JSON CWDs as well. Sort count descending/path ascending and cap at 15:

```go
func collectCwdSuggestions() []cwdSuggestion {
    home, _ := os.UserHomeDir()
    counts := map[string]int{}
    if home == "" {
        return nil
    }
    matches, _ := filepath.Glob(filepath.Join(home, ".claude", "sessions", "*.json"))
    for _, path := range matches {
        s, ok := readSessionFile(path)
        if ok && s.CWD != "" && !hiddenCwd(s.CWD) && isDir(s.CWD) {
            counts[s.CWD]++
        }
    }
    projects := filepath.Join(home, ".claude", "projects")
    ents, _ := os.ReadDir(projects)
    for _, entry := range ents {
        if !entry.IsDir() {
            continue
        }
        jsonls, _ := filepath.Glob(filepath.Join(projects, entry.Name(), "*.jsonl"))
        if len(jsonls) == 0 {
            continue
        }
        cwd := extractCWDFromJSONL(jsonls[0])
        if cwd != "" && !hiddenCwd(cwd) && isDir(cwd) && counts[cwd] < len(jsonls) {
            counts[cwd] = len(jsonls)
        }
    }
    out := make([]cwdSuggestion, 0, len(counts))
    for cwd, count := range counts {
        out = append(out, cwdSuggestion{CWD: cwd, Count: count})
    }
    sort.Slice(out, func(i, j int) bool {
        if out[i].Count != out[j].Count {
            return out[i].Count > out[j].Count
        }
        return out[i].CWD < out[j].CWD
    })
    if len(out) > 15 {
        out = out[:15]
    }
    return out
}
```

Rebuild `buildCwdPicker` from this list, prepending selected CWD and appending local `$PWD` as today. Add:

```go
func mergeRemoteCwdEntries(defaultCWD string, suggestions []cwdSuggestion) []cwdEntry {
    entries := make([]cwdEntry, 0, len(suggestions)+1)
    seen := map[string]bool{}
    if defaultCWD != "" {
        count := 0
        for _, suggestion := range suggestions {
            if suggestion.CWD == defaultCWD {
                count = suggestion.Count
                break
            }
        }
        entries = append(entries, cwdEntry{cwd: defaultCWD, count: count, isDefault: true})
        seen[defaultCWD] = true
    }
    for _, suggestion := range suggestions {
        if suggestion.CWD == "" || seen[suggestion.CWD] {
            continue
        }
        entries = append(entries, cwdEntry{cwd: suggestion.CWD, count: suggestion.Count})
        seen[suggestion.CWD] = true
    }
    return entries
}
```

- [ ] **Step 4: Write failing server endpoint tests**

Append to `server_test.go`:

```go
func TestCwdSuggestionsRequiresAuth(t *testing.T) {
    req := httptest.NewRequest(http.MethodGet, "/cwd-suggestions", nil)
    rec := httptest.NewRecorder()
    (&server{token: "secret"}).cwdSuggestions(rec, req)
    if rec.Code != http.StatusUnauthorized {
        t.Fatalf("status = %d, want 401", rec.Code)
    }
}

func TestCwdSuggestionsReturnsRankedHistory(t *testing.T) {
    home := t.TempDir()
    t.Setenv("HOME", home)
    cwd := filepath.Join(home, "project")
    if err := os.MkdirAll(cwd, 0o755); err != nil { t.Fatal(err) }
    sessionDir := filepath.Join(home, ".claude", "sessions")
    if err := os.MkdirAll(sessionDir, 0o755); err != nil { t.Fatal(err) }
    if err := os.WriteFile(filepath.Join(sessionDir, "1.json"), []byte(fmt.Sprintf(`{"pid":1,"cwd":%q}`, cwd)), 0o644); err != nil { t.Fatal(err) }

    req := httptest.NewRequest(http.MethodGet, "/cwd-suggestions", nil)
    req.Header.Set("Authorization", "Bearer secret")
    rec := httptest.NewRecorder()
    (&server{token: "secret"}).cwdSuggestions(rec, req)

    var got struct { Suggestions []cwdSuggestion `json:"suggestions"` }
    if err := json.NewDecoder(rec.Body).Decode(&got); err != nil { t.Fatal(err) }
    if len(got.Suggestions) != 1 || got.Suggestions[0].CWD != cwd {
        t.Fatalf("suggestions = %#v", got.Suggestions)
    }
}
```

- [ ] **Step 5: Implement endpoint and route**

Add to `server.go`:

```go
func (s *server) cwdSuggestions(w http.ResponseWriter, r *http.Request) {
    if !s.authed(r) {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }
    writeJSON(w, http.StatusOK, struct {
        Suggestions []cwdSuggestion `json:"suggestions"`
    }{Suggestions: collectCwdSuggestions()})
}
```

Register:

```go
mux.HandleFunc("GET /cwd-suggestions", s.cwdSuggestions)
```

- [ ] **Step 6: Add timeout-aware remote request and suggestion fetch**

Refactor `remoteRequest` in `remote_actions.go`:

```go
func remoteRequestWithTimeout(name, path, method string, body []byte, timeout time.Duration) ([]byte, error) {
    // existing remoteRequest body, with client Timeout set from timeout
}

func remoteRequest(name, path, method string, body []byte) ([]byte, error) {
    return remoteRequestWithTimeout(name, path, method, body, 30*time.Second)
}

func fetchRemoteCwdSuggestions(host string) ([]cwdSuggestion, error) {
    data, err := remoteRequestWithTimeout(host, "/cwd-suggestions", http.MethodGet, nil, 5*time.Second)
    if err != nil {
        return nil, err
    }
    var response struct {
        Suggestions []cwdSuggestion `json:"suggestions"`
    }
    if err := json.Unmarshal(data, &response); err != nil {
        return nil, err
    }
    return response.Suggestions, nil
}
```

- [ ] **Step 7: Run focused and full tests**

```sh
gofmt -w picker.go picker_test.go server.go server_test.go remote_actions.go
go test -count=1 ./... -run 'Test(CollectCwdSuggestions|MergeRemoteCwdEntries|CwdSuggestions)'
go test -count=1 ./...
go vet ./...
```

Expected: PASS.

- [ ] **Step 8: Review scoped diff**

```sh
git diff -- picker.go picker_test.go server.go server_test.go remote_actions.go
```

Expected: shared ranking, endpoint, and timeout plumbing only. Do not commit.

---

### Task 3: Build the Two-Axis New-Session Modal

**Files:**
- Create: `new_picker.go`
- Create: `new_picker_test.go`
- Modify: `helpers.go:61-102`

**Interfaces:**
- Consumes: `CommandPreset`, `KeyUp`, `KeyDown`, `KeyLeft`, `KeyRight`, `KeyEsc`, `readEventBlocking`, terminal color helpers.
- Produces:
  - `type newPickerState struct { Row, Preset, RowCount, PresetCount int }`
  - `func (s *newPickerState) handle(key string) (confirm, cancel bool)`
  - `func renderNewPicker(title string, lines []string, presets []CommandPreset, state newPickerState, note string) string`
  - `func pickNewSession(title string, lines []string, rowStart int, presets []CommandPreset, presetStart int, note string) (row, preset int, ok bool)`

- [ ] **Step 1: Write failing picker-state tests**

Create `new_picker_test.go`:

```go
package main

import (
    "strings"
    "testing"
)

func TestNewPickerStateAxesAndWrap(t *testing.T) {
    state := newPickerState{RowCount: 3, PresetCount: 2}
    state.handle(KeyLeft)
    if state.Row != 0 || state.Preset != 1 { t.Fatalf("left = %#v", state) }
    state.handle(KeyRight)
    if state.Preset != 0 { t.Fatalf("right = %#v", state) }
    state.handle(KeyUp)
    if state.Row != 2 || state.Preset != 0 { t.Fatalf("up = %#v", state) }
    state.handle(KeyDown)
    if state.Row != 0 { t.Fatalf("down = %#v", state) }
}

func TestNewPickerStateConfirmAndCancel(t *testing.T) {
    state := newPickerState{RowCount: 3, PresetCount: 2}
    if confirm, cancel := state.handle("2"); !confirm || cancel || state.Row != 1 {
        t.Fatalf("digit = confirm %v cancel %v state %#v", confirm, cancel, state)
    }
    if confirm, cancel := state.handle("q"); confirm || !cancel {
        t.Fatalf("q = confirm %v cancel %v", confirm, cancel)
    }
}

func TestRenderNewPicker(t *testing.T) {
    presets := []CommandPreset{{Name: "Claude", Command: "claude"}, {Name: "Fable", Command: "claude --model fable"}}
    out := renderNewPicker("New session on beluga", []string{"/repo", "enter path manually…"}, presets,
        newPickerState{Row: 1, Preset: 1, RowCount: 2, PresetCount: 2}, "remote suggestions unavailable")
    for _, want := range []string{"New session on beluga", "Fable", "claude --model fable", "←/→ command", "remote suggestions unavailable"} {
        if !strings.Contains(out, want) { t.Fatalf("output missing %q:\n%s", want, out) }
    }
}
```

- [ ] **Step 2: Verify picker tests fail**

```sh
go test -count=1 ./... -run 'TestNewPicker'
```

Expected: compile failure for missing picker types/functions.

- [ ] **Step 3: Implement pure state and renderer**

Create `new_picker.go` with:

```go
package main

import (
    "fmt"
    "strings"
)

type newPickerState struct {
    Row, Preset           int
    RowCount, PresetCount int
}

func (s *newPickerState) handle(key string) (confirm, cancel bool) {
    switch key {
    case KeyUp:
        s.Row = (s.Row + s.RowCount - 1) % s.RowCount
    case KeyDown:
        s.Row = (s.Row + 1) % s.RowCount
    case KeyLeft:
        s.Preset = (s.Preset + s.PresetCount - 1) % s.PresetCount
    case KeyRight:
        s.Preset = (s.Preset + 1) % s.PresetCount
    case "\r", "\n":
        return true, false
    case "q", "Q", KeyEsc, "\x03":
        return false, true
    default:
        if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
            row := int(key[0] - '1')
            if row < s.RowCount {
                s.Row = row
                return true, false
            }
        }
    }
    return false, false
}

func renderNewPicker(title string, lines []string, presets []CommandPreset, state newPickerState, note string) string {
    preset := presets[state.Preset]
    var b strings.Builder
    b.WriteString("\033[H\033[J\n " + bold(title) + "\n\n")
    fmt.Fprintf(&b, " Command:  ◀ %s ▶\n           %s\n\n", bold(preset.Name), dim(preset.Command))
    for i, line := range lines {
        marker := "   "
        if i == state.Row { marker = " ▶ " }
        fmt.Fprintf(&b, "%s%s%2d)%s  %s\n", marker, ansiBold, i+1, ansiReset, line)
    }
    if note != "" { b.WriteString("\n " + dim(note) + "\n") }
    b.WriteString("\n " + dim("↑/↓ cwd · ←/→ command · Enter select · q cancel") + "\n")
    return b.String()
}

func pickNewSession(title string, lines []string, rowStart int, presets []CommandPreset, presetStart int, note string) (row, preset int, ok bool) {
    if len(lines) == 0 || len(presets) == 0 { return 0, 0, false }
    state := newPickerState{Row: rowStart, Preset: presetStart, RowCount: len(lines), PresetCount: len(presets)}
    if state.Row < 0 || state.Row >= state.RowCount { state.Row = 0 }
    if state.Preset < 0 || state.Preset >= state.PresetCount { state.Preset = 0 }
    for {
        fmt.Print(renderNewPicker(title, lines, presets, state, note))
        for _, key := range readEventBlocking() {
            confirm, cancel := state.handle(key)
            if cancel { return 0, 0, false }
            if confirm { return state.Row, state.Preset, true }
        }
    }
}
```

Remove obsolete `pickMenu` from `helpers.go`; retain imports still used elsewhere.

- [ ] **Step 4: Run picker tests and full suite**

```sh
gofmt -w new_picker.go new_picker_test.go helpers.go
go test -count=1 ./... -run 'TestNewPicker'
go test -count=1 ./...
```

Expected: PASS.

- [ ] **Step 5: Review scoped diff**

```sh
git diff -- new_picker.go new_picker_test.go helpers.go
```

Expected: dedicated modal only. Do not commit.

---

### Task 4: Parameterize SpawnNew and Integrate Local Creation

**Files:**
- Modify: `migrate.go:90-101`
- Create: `migrate_test.go`
- Modify: `actions.go:192-248`
- Modify: `commands.go:75-114`
- Modify: `tui.go:391-421`

**Interfaces:**
- Consumes: Tasks 1 and 3 preset/picker APIs.
- Produces: `func SpawnNew(cwd, displayName, command string) (string, error)`.

- [ ] **Step 1: Write failing fake-tmux test**

Create `migrate_test.go`:

```go
package main

import (
    "os"
    "path/filepath"
    "strings"
    "testing"
)

func TestSpawnNewSendsConfiguredCommand(t *testing.T) {
    dir := t.TempDir()
    logPath := filepath.Join(dir, "tmux.log")
    script := filepath.Join(dir, "tmux")
    body := "#!/bin/sh\nfor arg in \"$@\"; do printf '<%s>' \"$arg\"; done >> \"$TMUX_LOG\"\nprintf '\\n' >> \"$TMUX_LOG\"\n"
    if err := os.WriteFile(script, []byte(body), 0o755); err != nil { t.Fatal(err) }
    t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
    t.Setenv("TMUX_LOG", logPath)

    if _, err := SpawnNew(dir, "test", "claude --model fable"); err != nil { t.Fatal(err) }
    data, err := os.ReadFile(logPath)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(string(data), "<send-keys>") || !strings.Contains(string(data), "<claude --model fable><Enter>") {
        t.Fatalf("tmux argv:\n%s", data)
    }
}
```

- [ ] **Step 2: Verify compile failure from new signature**

```sh
go test -count=1 ./... -run '^TestSpawnNewSendsConfiguredCommand$'
```

Expected: compile failure because `SpawnNew` accepts two arguments.

- [ ] **Step 3: Parameterize SpawnNew and update deterministic CLI caller**

Change `SpawnNew`:

```go
func SpawnNew(cwd, displayName, command string) (string, error) {
    tname := MakeTmuxName(cwd, randomSlug(), displayName)
    if err := exec.Command("tmux", "new-session", "-d", "-s", tname, "-c", cwd).Run(); err != nil {
        return "", fmt.Errorf("tmux new-session: %w", err)
    }
    if err := exec.Command("tmux", "send-keys", "-t", tname, command, "Enter").Run(); err != nil {
        return "", fmt.Errorf("tmux send-keys: %w", err)
    }
    return tname, nil
}
```

In `cmdNew`, load presets and use first:

```go
presets, err := LoadCommandPresets()
if err != nil {
    fmt.Fprintln(os.Stderr, err)
    return 1
}
tname, err := SpawnNew(cwd, name, presets[0].Command)
```

Update the server handler's existing call so the package remains green before
Task 5 adds request-time selection:

```go
presets, err := LoadCommandPresets()
if err != nil {
    writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
    return
}
tname, err := SpawnNew(body.CWD, body.Name, presets[0].Command)
```

This is the backward-compatible missing-command behavior Task 5 retains.

- [ ] **Step 4: Integrate local two-axis modal**

In `actNew`, load presets and remembered index before building rows:

```go
presets, err := LoadCommandPresets()
if err != nil {
    fmt.Printf("\nload commands: %v\n", err)
    pauseForKey(c.fd, c.oldState)
    return
}
presetStart := LoadCommandPresetIndex(presets)
```

Replace `pickMenu` with:

```go
row, presetIndex, ok := pickNewSession("New tmux session", lines, start, presets, presetStart, "")
if !ok { return }
preset := presets[presetIndex]
SaveCommandPresetName(preset.Name)
```

Use `row` instead of `idx`, and call:

```go
tname, err := SpawnNew(cwd, "", preset.Command)
```

Update visible copy from `tmux+claude` to `tmux session` where command is configurable. Keep path prompting, local tilde expansion, validation, attachment, and terminal-mode transitions unchanged.

Update help line in `tui.go`:

```go
fmt.Println("    n            new tmux session (↑/↓ cwd · ←/→ command)")
```

- [ ] **Step 5: Run focused and full tests**

```sh
gofmt -w migrate.go migrate_test.go actions.go commands.go tui.go
go test -count=1 ./... -run 'Test(SpawnNew|NewPicker|CommandPreset)'
go test -count=1 ./...
go vet ./...
```

Expected: PASS. All three `SpawnNew` callers now provide explicit command text; the server uses the first configured preset until Task 5 adds allowlist selection.

- [ ] **Step 6: Review scoped diff**

```sh
git diff -- migrate.go migrate_test.go actions.go commands.go tui.go server.go
```

Expected: command parameter and local UI propagation only. Do not commit.

---

### Task 5: Integrate Remote Modal and Server Allowlist

**Files:**
- Modify: `remote_actions.go:190-234`
- Modify: `server.go:129-157`
- Modify: `server_test.go`
- Modify: `actions_test.go`

**Interfaces:**
- Consumes: command loaders, two-axis picker, remote CWD fetch/merge, parameterized `SpawnNew`.
- Produces: remote preset-name request and server-side allowlist resolution.

- [ ] **Step 1: Write failing allowlist handler tests**

Append to `server_test.go`:

```go
func writeCommandConfig(t *testing.T, home string) {
    t.Helper()
    dir := filepath.Join(home, ".config", "claude-sessions")
    if err := os.MkdirAll(dir, 0o755); err != nil { t.Fatal(err) }
    data := "commands:\n  - name: Claude\n    command: claude\n  - name: Fable\n    command: claude --model fable\n"
    if err := os.WriteFile(filepath.Join(dir, "servers.yaml"), []byte(data), 0o644); err != nil { t.Fatal(err) }
}

func TestNewSessionRejectsUnknownCommandPreset(t *testing.T) {
    home := t.TempDir()
    t.Setenv("HOME", home)
    writeCommandConfig(t, home)
    req := httptest.NewRequest(http.MethodPost, "/sessions/new", strings.NewReader(fmt.Sprintf(`{"cwd":%q,"command":"Unknown"}`, home)))
    req.Header.Set("Authorization", "Bearer test-token")
    rec := httptest.NewRecorder()
    (&server{token: "test-token"}).newSession(rec, req)
    var got actionResult
    if err := json.NewDecoder(rec.Body).Decode(&got); err != nil { t.Fatal(err) }
    if got.Error != "command preset not configured: Unknown" {
        t.Fatalf("error = %q", got.Error)
    }
}

func installFakeTmux(t *testing.T) string {
    t.Helper()
    dir := t.TempDir()
    logPath := filepath.Join(dir, "tmux.log")
    script := filepath.Join(dir, "tmux")
    body := "#!/bin/sh\nfor arg in \"$@\"; do printf '<%s>' \"$arg\"; done >> \"$TMUX_LOG\"\nprintf '\\n' >> \"$TMUX_LOG\"\n"
    if err := os.WriteFile(script, []byte(body), 0o755); err != nil { t.Fatal(err) }
    t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
    t.Setenv("TMUX_LOG", logPath)
    return logPath
}

func TestNewSessionMissingCommandUsesFirstPreset(t *testing.T) {
    home := t.TempDir()
    t.Setenv("HOME", home)
    writeCommandConfig(t, home)
    logPath := installFakeTmux(t)

    req := httptest.NewRequest(http.MethodPost, "/sessions/new", strings.NewReader(fmt.Sprintf(`{"cwd":%q}`, home)))
    req.Header.Set("Authorization", "Bearer test-token")
    rec := httptest.NewRecorder()
    (&server{token: "test-token"}).newSession(rec, req)

    var got actionResult
    if err := json.NewDecoder(rec.Body).Decode(&got); err != nil { t.Fatal(err) }
    if !got.OK || got.Tmux == "" {
        t.Fatalf("result = %#v", got)
    }
    data, err := os.ReadFile(logPath)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(string(data), "<claude><Enter>") {
        t.Fatalf("tmux argv:\n%s", data)
    }
}

func TestNewSessionKnownPresetUsesItsCommand(t *testing.T) {
    home := t.TempDir()
    t.Setenv("HOME", home)
    writeCommandConfig(t, home)
    logPath := installFakeTmux(t)

    req := httptest.NewRequest(http.MethodPost, "/sessions/new", strings.NewReader(fmt.Sprintf(`{"cwd":%q,"command":"Fable"}`, home)))
    req.Header.Set("Authorization", "Bearer test-token")
    rec := httptest.NewRecorder()
    (&server{token: "test-token"}).newSession(rec, req)

    var got actionResult
    if err := json.NewDecoder(rec.Body).Decode(&got); err != nil { t.Fatal(err) }
    if !got.OK || got.Tmux == "" {
        t.Fatalf("result = %#v", got)
    }
    data, err := os.ReadFile(logPath)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(string(data), "<claude --model fable><Enter>") {
        t.Fatalf("tmux argv:\n%s", data)
    }
}
```

These tests exercise the real HTTP handler end-to-end through the fake tmux; do not replace them with helper-level assertions that bypass `newSession`.

- [ ] **Step 2: Verify unknown-preset test fails correctly**

```sh
go test -count=1 ./... -run 'TestNewSession(RejectsUnknownCommandPreset|MissingCommandUsesFirstPreset|KnownPresetUsesItsCommand)'
```

Expected: `RejectsUnknownCommandPreset` and `KnownPresetUsesItsCommand` FAIL because the handler ignores the command field; `MissingCommandUsesFirstPreset` already passes via Task 4's first-preset call and acts as a regression guard.

- [ ] **Step 3: Resolve preset name in server handler**

Extend request body:

```go
var body struct {
    CWD     string `json:"cwd"`
    Name    string `json:"name"`
    Command string `json:"command"` // preset name, never raw command text
}
```

After CWD validation:

```go
presets, err := LoadCommandPresets()
if err != nil {
    writeJSON(w, http.StatusOK, actionResult{Error: err.Error()})
    return
}
preset := presets[0]
if body.Command != "" {
    var ok bool
    preset, ok = findCommandPreset(presets, body.Command)
    if !ok {
        writeJSON(w, http.StatusOK, actionResult{Error: "command preset not configured: " + body.Command})
        return
    }
}
tname, err := SpawnNew(body.CWD, body.Name, preset.Command)
```

This preserves old clients by mapping missing command to index zero.

- [ ] **Step 4: Write pure remote modal preparation test**

Add a helper in `remote_actions.go`:

```go
func remoteNewRows(defaultCWD string, suggestions []cwdSuggestion) (lines []string, start int) {
    entries := mergeRemoteCwdEntries(defaultCWD, suggestions)
    lines = make([]string, 0, len(entries)+1)
    for i, entry := range entries {
        if entry.isDefault { start = i }
        freq := ""
        if entry.count > 0 { freq = "  " + dim(fmt.Sprintf("(%d)", entry.count)) }
        lines = append(lines, fmt.Sprintf("%-50s%s", entry.cwd, freq))
    }
    lines = append(lines, "enter path manually…")
    return lines, start
}
```

Add to `actions_test.go`:

```go
func TestRemoteNewRowsSuggestionsAndFallback(t *testing.T) {
    lines, start := remoteNewRows("/selected", []cwdSuggestion{{CWD: "/history", Count: 4}, {CWD: "/selected", Count: 2}})
    if start != 0 || len(lines) != 3 || !strings.Contains(lines[0], "/selected") || !strings.Contains(lines[1], "/history") {
        t.Fatalf("rows = %#v start=%d", lines, start)
    }
    fallback, _ := remoteNewRows("", nil)
    if len(fallback) != 1 || fallback[0] != "enter path manually…" {
        t.Fatalf("fallback rows = %#v", fallback)
    }
}
```

- [ ] **Step 5: Replace remote cooked-only flow with modal**

In `actNewRemote`:

1. Load presets and remembered index.
2. Call `fetchRemoteCwdSuggestions(host)` before opening modal.
3. On fetch error, use nil suggestions and note `remote suggestions unavailable`.
4. Build entries/lines from `mergeRemoteCwdEntries(defaultCWD, suggestions)`.
5. Call `pickNewSession("New session on "+host, lines, start, presets, presetStart, note)`.
6. Save confirmed preset name.
7. If selected row is a listed entry, use its CWD; otherwise switch cooked and call `readLine("\ncwd path (q=cancel) > ")`.
8. POST preset name:

```go
body, _ := json.Marshal(map[string]string{
    "cwd":     cwd,
    "command": preset.Name,
})
```

Keep response handling and SSH/tmux attachment unchanged. Do not locally expand or validate remote manual paths.

- [ ] **Step 6: Run focused and full tests**

```sh
gofmt -w remote_actions.go server.go server_test.go actions_test.go
go test -count=1 ./... -run 'Test(NewSession|RemoteNewRows|CwdSuggestions|SelectedRemoteNewTarget|SpawnNew)'
go test -count=1 ./...
go vet ./...
go build .
```

Expected: PASS.

- [ ] **Step 7: Review scoped diff**

```sh
git diff -- remote_actions.go server.go server_test.go actions_test.go
```

Expected: preset-name payload, server allowlist, modal integration, and tests only. Do not commit.

---

### Task 6: Documentation and Final Verification

**Files:**
- Modify: `README.md:92-170`
- Verify all feature files.

**Interfaces:**
- Consumes completed Tasks 1-5.
- Produces documented config/controls and final evidence.

- [ ] **Step 1: Update README configuration example**

Before `servers:` add:

```yaml
commands:
  - name: Claude
    command: claude
  - name: ClaudeX
    command: claudex
  - name: Fable
    command: claude --model fable
```

Document:

- absent `commands:` defaults to `Claude` / `claude`;
- left/right cycles commands in `n` modal while up/down cycles CWD;
- last confirmed command is remembered;
- remote hosts need matching preset names in their own `servers.yaml`;
- remote command text may differ by host;
- remote path history loads on demand and falls back to manual entry;
- command strings are trusted shell input.

Update live-view key row for `n` and Files section for `command-preset` state.

- [ ] **Step 2: Run fresh full verification**

```sh
go test -count=1 ./...
go vet ./...
go build .
git diff --check
```

Expected: all commands exit zero.

- [ ] **Step 3: Run focused behavior suite**

```sh
go test -count=1 ./... -run 'Test(ParseConfigYAML|CommandPreset|CollectCwdSuggestions|MergeRemoteCwdEntries|CwdSuggestions|NewPicker|SpawnNew|NewSession|RemoteNewRows)'
```

Expected: PASS.

- [ ] **Step 4: Inspect final working tree without altering unrelated changes**

```sh
git status --short
git diff -- yaml.go yaml_test.go config.go config_test.go picker.go picker_test.go server.go server_test.go new_picker.go new_picker_test.go helpers.go migrate.go migrate_test.go actions.go actions_test.go remote_actions.go commands.go tui.go README.md docs/superpowers/specs/2026-07-21-command-preset-picker-design.md docs/superpowers/plans/2026-07-21-command-preset-picker.md
```

Expected: feature changes visible; unrelated existing changes remain untouched; nothing staged or committed.

- [ ] **Step 5: Manual TUI check without remote mutation**

Run local TUI and verify:

1. `n` opens modal showing current command label and exact command.
2. Left/right wraps command presets without moving CWD.
3. Up/down moves CWD without changing command.
4. Cancel does not change remembered preset.
5. Reopen after confirmation starts on remembered preset.
6. On beluga, remote suggestions appear or fallback note/manual row appears within 5 seconds.

Stop before confirming a spawn if testing would create an unwanted session. Report any unperformed mutating check explicitly.

- [ ] **Step 6: Report completion without committing**

Report files changed, automated command evidence, manual non-mutating check results, remote deployment requirement, and skipped mutation checks. Do not commit or push.
