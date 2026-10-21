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
		"Use FLEET's pick"} {
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
		"function renderRich(", "renderRich(sum)", "renderRich(t.text)", "```", "](", `\]\(`,
		"safeLink(m[3], m[2])",
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
		"function cardSession(", "sessOf(c.tmux)", `"About card "`} {
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
