package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFleetPageIsSelfContainedAndSafe(t *testing.T) {
	page := string(fleetPageHTML)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "src=\"http", "href=\"http", "@import", "eval("} {
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
		"safeLink(m[3], m[2])"} {
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
