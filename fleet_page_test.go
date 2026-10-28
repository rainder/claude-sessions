package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFleetPageIsSelfContainedAndSafe(t *testing.T) {
	page := string(fleetPageHTML)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "src=\"http", "href=\"http", "@import", "eval(",
		// FLEET's pick is a tag on its option, not a button of its own.
		"Use FLEET's pick",
		// Answers live in the server store: the page never writes its old
		// localStorage store, and the Chat note is never sent as an answer.
		"savePending(", `" + tm + ")", id)`,
		// The correction flag is fixed when the sheet opens: worked out at
		// send time, an answer from another device that landed while the
		// sheet was open would turn this send into a correction and slip
		// past the server's already_answered guard.
		"correction = answerOf(", "correction: answerOf(",
		// prefillChat keeps what is in the box, so Use as answer must not use it.
		`prefillChat("Andy decides on " + id + ": " + `} {
		if strings.Contains(page, bad) {
			t.Errorf("fleet.html must not contain %q", bad)
		}
	}
	for _, need := range []string{"/fleet/state", "/fleet/chat", "/fleet/log", "/fleet/usage", "/fleet/say", "/fleet/accounts", "/fleet/account/switch",
		"/fleet/push/key", "/fleet/push/subscribe", "/fleet/push/unsubscribe", "requestPermission()", "pushManager.subscribe(",
		`"open-card"`, "dataset.cardId", "Add to Home Screen to get notifications", "sameKey(", "options.applicationServerKey",
		"Europe/Vilnius", "noopener noreferrer", "visibilitychange", "Yes, I approve this write",
		"since=", ".pct", ".account", ".windows",
		// Card summaries and chat turns go through the one markdown renderer,
		// which knows fenced blocks and [label](url) links (via safeLink).
		"function renderRich(", "renderSummary(sum)", "renderRich(text, true)", "renderRich(t.text)", "```", "](", `\]\(`,
		"safeLink(m[3], m[2])",
		// A link label that names another host shows the real target.
		"function linkLabel(", "a.textContent = linkLabel(label, url, u)",
		// Autolinks: ClickUp custom ids and trecs-brain PR numbers.
		"https://app.clickup.com/t/90181100858/", "https://github.com/TRECS-SOFTWARE-LABS/trecs-brain/pull/", "function linkText(",
		// The empty Decide view shows the fleet's busy and idle counts.
		`id="zero-fleet"`, "function renderFleetState(",
		// The capacity strip under the Working heading: a tile per model
		// family (Sonnet shares Opus's tile), busy against FLEET's cap
		// (state.caps), counted with the same helper as the Decide view.
		`id="caps-working"`, "function renderCaps(", "function capTile(", "function fleetCounts(", "fleetCounts(workingRows()).all",
		"state.caps", `{Opus: "Opus + Sonnet"}`, `"Show all"`, `"1 slot left"`, `"At limit"`, `"Over limit"`,
		// The Dependencies section draws state.deps as SVG, built with
		// createElementNS (never markup), and joins sessions to the
		// working list.
		`id="deps"`, `id="n-deps"`, "function renderDeps(", "state.deps", "state.sessions", "createElementNS(", "function depLayout(",
		"#session/",
		// The graph also opens full page: a modal dialog drawn by the same
		// drawDeps, spread over the space it measures.
		`id="deps-refresh"`, "function refreshDeps(", `id="deps-expand"`, `id="deps-full"`, `id="deps-full-g"`, `id="deps-close"`, "function drawDeps(", "function openDepsFull(",
		// Below 1100px Dependencies is a view of its own in the bottom nav,
		// not a section of the Fleet view.
		`data-view="deps"`, `"fleet", "deps", "history"]`, "body.v-fleet .sec-deps", "body.v-deps .sec-fleet:not(.sec-deps)",
		// The heading shows the graph's age (deps.generatedAt), amber after
		// 2 h, and ticks with the other ages.
		`id="deps-age"`, `id="deps-age-full"`, "function renderDepsAge(", "d.generatedAt", "renderDepsAge();\n",
		// A decision card ends its options with Other (a free-text answer in
		// the FLEET chat, prefixed with the card id). A Chat button at the top
		// opens the card's live session (by its tmux field), else the FLEET
		// chat.
		`"opt other-btn"`, `"Other"`, `"Andy decides on "`, "function chatBtn(", `"btn chat-btn", "Chat"`,
		"function cardSession(", "sessOf(c.tmux)", `"About card "`,
		// Answers come from the server store (state.answered) and card
		// identity from the server's hash (state.cardHashes). A send carries
		// the option key and the correction flag; the old local store is
		// read once for the deploy migration.
		"/fleet/card/dismiss", "state.answered", "state.cardHashes", "optionKey:", "correction:",
		"already_answered:", "card_changed:", "card_gone:", "Already answered on another device",
		"function loadLegacyPending(", "function answerOf(", "function hashOf(", `" + tm + ")", null)`,
		"sheet.correction = correcting.has(id) || answerOf(id) !== null", "correction = sheet.correction;", "sayCard(text, id, str(o.key), hash, correction)", "!sheet.correction && answerOf(", `"Already answered on another device.", true)`,
		// Both composers take images: paste, drop and a file picker. The
		// picker has no capture attribute (a phone must offer its photo
		// library and screenshots), the images upload first, and the text
		// carries their stored paths after Andy's words.
		`id="say-attach"`, `id="sv-attach"`, `id="say-att"`, `id="sv-att"`,
		`accept="image/png,image/jpeg,image/webp,image/gif" multiple hidden>`,
		`addEventListener("paste"`, `addEventListener("drop"`, "URL.createObjectURL(f)", "URL.revokeObjectURL(",
		`fetch("/fleet/upload"`, "function uploadImages(", "withImages(typed, up.paths)", "withImages(text, up.paths)",
		"const ATT_MAX = 5", "sayAtt.drop(imgs)", "svAtt.drop(imgs)",
		// Clarify: a side chat about one card with a read-only fork of FLEET,
		// in its own sheet. Use as answer only prefills the chat; a grok
		// FLEET has no Clarify.
		"/fleet/clarify", "/fleet/clarify/ask", "/fleet/clarify/draft",
		`id="clarify-sheet"`, "function openClarify(", "CLARIFY_POLL",
		"fork of FLEET · read-only · FLEET does not see this",
		`"Andy decides on " + id + ": " + `, `state.fleet.tool === "grok"`,
		// Use as answer sets the chat box, never adds to what is there:
		// leftover text would ride along inside the decision.
		`"Andy decides on " + id + ": " + draft.text`, `"Replaced the text in the chat box."`,
		// A draft longer than a send can carry is cut, on a code point, to
		// the server's byte cap, as the TUI cuts it; else Send would fail.
		"const SAY_MAX_BYTES = 4096;", "function cutUTF8(s, max)",
		`ta.value = cutUTF8("Andy decides on " + id + ": " + draft.text`,
		`bad_text: "Question has control characters"`,
		// The Claude auto switch: a line under the account chips for 12 h
		// and a toast for a new one, from state.autoSwitch; its log lines
		// have their own filter.
		`id="autosw"`, "function renderAutoSwitch(", "state.autoSwitch", "a.last", "last.fromPct", "last.toPct",
		`"Auto-switched "`, "AUTO_SW_SHOW_MS = 12 * 3600e3", "toast(text)", `"auto-switch"`, ".ty-auto-switch"} {
		if !strings.Contains(page, need) {
			t.Errorf("fleet.html must contain %q", need)
		}
	}
}

// TestFleetPageReadsRealUsageKeys pins the page to the json tags the server
// actually writes: a renamed tag would leave the header chips silently empty.
func TestFleetPageReadsRealUsageKeys(t *testing.T) {
	resp := usageNumbersResponse{
		Claude: []claudeUsageNumbers{{Account: "a@example.com", Active: true,
			FiveHour: &usageWindowNumbers{Label: "5h", Pct: 12}, SevenDay: &usageWindowNumbers{Label: "7d", Pct: 34}}},
		Grok: []providerUsageNumbers{{Account: "g@example.com", Windows: []usageWindowNumbers{{Label: "weekly", Pct: 56}}}},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	page := string(fleetPageHTML)
	for _, key := range []string{"claude", "grok", "active", "fiveHour", "sevenDay", "account", "pct", "windows", "label"} {
		if !strings.Contains(body, `"`+key+`"`) {
			t.Fatalf("usageNumbersResponse JSON has no key %q: %s", key, body)
		}
		if !strings.Contains(page, "."+key) {
			t.Errorf("fleet.html does not read .%s", key)
		}
	}
}

// TestFleetPageReadsRealAnswerKeys pins the page to the json tags the answer
// store actually writes: a renamed tag would leave every card looking open.
func TestFleetPageReadsRealAnswerKeys(t *testing.T) {
	a := fleetAnswer{CardHash: "h", CardChanged: true, Attempts: []fleetAttempt{{OptionKey: "k", Label: "l", Comment: true,
		At: "2026-10-22T10:00:00Z", Status: "sent"}}}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	page := string(fleetPageHTML)
	for _, key := range []string{"cardHash", "cardChanged", "attempts", "optionKey", "label", "status"} {
		if !strings.Contains(body, `"`+key+`"`) {
			t.Fatalf("fleetAnswer JSON has no key %q: %s", key, body)
		}
		if !strings.Contains(page, "."+key) {
			t.Errorf("fleet.html does not read .%s", key)
		}
	}
}

// TestFleetPageCSPAllowsThumbnails: the pending-image thumbnails are blob:
// URLs, which an img-src without blob: blocks with no error on the page.
func TestFleetPageCSPAllowsThumbnails(t *testing.T) {
	if !strings.Contains(fleetPageCSP, "img-src 'self' data: blob:;") {
		t.Fatalf("CSP %q must allow blob: images", fleetPageCSP)
	}
	if strings.Contains(fleetPageCSP, "connect-src 'self' blob:") || strings.Contains(fleetPageCSP, "script-src 'unsafe-inline' blob:") {
		t.Fatal("blob: is for images only")
	}
}

// TestFleetPageCardsFold: decision cards fold behind a real button that says
// whether it is open, the open set survives a reload through try-wrapped
// storage, a deep link opens its card, and a shut card draws no options.
func TestFleetPageCardsFold(t *testing.T) {
	page := string(fleetPageHTML)
	for _, need := range []string{
		`const FOLD_KEY = "fleet.expanded.v1";`,
		"try {\n    const m = obj(JSON.parse(localStorage.getItem(FOLD_KEY)",
		"try {\n      if (!unfolded.size) localStorage.removeItem(FOLD_KEY);",
		`el("button", "card-tog"`, `b.type = "button";`,
		`b.setAttribute("aria-expanded", open ? "true" : "false");`, `b.setAttribute("aria-controls", bodyId);`,
		"unfolded.get(str(c.id)) === str(c.kind)",
		"if (!open) { body.hidden = true; return card; }",
		"if (openCardID) unfoldCard(",
		"decisionCard(c, true, true)", "decisionCard(c, false, true)", "decisionCard(c, explicit))",
		"pruneUnfolded(st);",
	} {
		if !strings.Contains(page, need) {
			t.Errorf("fleet.html must contain %q", need)
		}
	}
	// The header button holds plain text only: no linkText inside foldHead.
	i := strings.Index(page, "function foldHead(")
	j := strings.Index(page, "function titleLinks(")
	if i < 0 || j < i {
		t.Fatal("foldHead and titleLinks not found in order")
	}
	if strings.Contains(page[i:j], "linkText(") || strings.Contains(page[i:j], "safeLink(") {
		t.Error("foldHead must not put links inside the toggle button")
	}
}
