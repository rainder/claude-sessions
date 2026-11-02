package main

import (
	"encoding/json"
	"os/exec"
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
		`prefillChat("Andy decides on " + id + ": " + `,
		// A desk's words come from its name, never a fixed one.
		`"CHAT is`, `"Write to CHAT"`, "desk=1"} {
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
		`"Auto-switched "`, "AUTO_SW_SHOW_MS = 12 * 3600e3", "toast(text)", `"auto-switch"`, ".ty-auto-switch",
		// The chat panel's desks come from state.desks, each built from one
		// template, and every desk request is pinned to that desk by name
		// (TestFleetPageDeskTargets drives the module).
		"state.desks", `<template id="desk-tpl">`, `"/fleet/session?desk=" + encodeURIComponent(D.name)`, "desk: D.name}"} {
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

// deskPageWorld is a stand-in for the rest of fleet.html around the chat
// target module: a few-field fake element, stubbed fetches that record what
// the page asked and sent, and timers that never fire. world(pref) builds one
// fresh page with pref remembered; the test code after it drives the module.
const deskPageWorld = `
const str = v => typeof v === "string" ? v : "";
const arr = v => Array.isArray(v) ? v : [];
const obj = v => (v && typeof v === "object" && !Array.isArray(v)) ? v : null;
class E {
  constructor(tag) {
    Object.assign(this, {tag, hidden: false, disabled: false, value: "", textContent: "", title: "", placeholder: "",
      className: "", style: {}, children: [], attrs: {}, on: {}, clientHeight: 0, scrollTop: 0, scrollHeight: 0});
    this.classList = {toggle() {}, add() {}, remove() {}};
  }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(type, f) { (this.on[type] = this.on[type] || []).push(f); }
  fire(type, ev) { for (const f of this.on[type] || []) f(ev || {}); }
  append(...kids) { for (const k of kids) this.children.push(k); }
  getClientRects() { return []; }
  scrollTo() {}
}
function world(pref) {
  let state = null;
  const expired = false;
  const document = {hidden: false, activeElement: null};
  const window = {};
  const ids = {};
  const $ = id => ids[id] || (ids[id] = new E(id));
  function el(tag, cls, ...kids) { const e = new E(tag); e.className = cls || ""; for (const k of kids) if (k) e.textContent += k; return e; }
  const store = {};
  if (pref !== null) store["fleet.chatTarget"] = pref;
  const localStorage = {getItem: k => k in store ? store[k] : null, setItem: (k, v) => { store[k] = String(v); }};
  const SV_NAME = /^[A-Za-z0-9._-]{1,128}$/;
  const svOpenErrors = {gone: "This session has ended."}, svSendErrors = {busy: "busy"};
  const chat = {stale: false, follow: true, savedTop: null};
  const renderChat = () => {}, measure = () => {}, guard = f => f();
  const toasts = [], fetched = [], said = [];
  const toast = t => toasts.push(t);
  const setTimeout = () => 0, clearTimeout = () => {};
  async function svFetch(url) {
    fetched.push(url);
    const m = /[?&]desk=([A-Z]+)/.exec(url);
    return {body: {target: {canSend: true, sessionId: "sid-" + (m ? m[1] : "")}, source: "transcript", turns: [], pos: 1}};
  }
  async function postSay(url, payload) { said.push(payload); return {ok: true}; }
  async function uploadImages() { return {ok: true, paths: []}; }
  const withImages = t => t, tooLong = () => false, TOO_LONG = "too long";
  const mergeSession = (v, d) => { v.target = d.target; v.source = d.source; return false; }, renderSessionLog = () => {};
  const attachBox = () => ({items: [], busy: false, render() {}, drop() {}});
  function deskNodes() {
    const n = {};
    for (const k of ["wrap", "log", "newmsg", "comp", "note", "noteT", "warn", "att", "attach", "file", "say", "send"]) n[k] = new E(k);
    n.wrap.hidden = n.comp.hidden = true;
    return n;
  }
  /*MODULE*/
  return {applyDesk, deskPref, desks, ids, store, said, fetched, toasts,
    get shown() { return deskShown; }, setState(s) { state = s; }};
}
const flush = () => new Promise(r => setImmediate(r));
const fails = [];
const check = (ok, what) => { if (!ok) fails.push(what); };
const both = {desks: [{name: "CHAT", alive: true, tmux: "desk"}, {name: "SLACK", alive: true, tmux: "slack-triage-c4a100"}]};
`

// deskPageDrive is the test itself, run after deskPageWorld; it prints the
// failed checks as a JSON list.
const deskPageDrive = `
(async () => {
  const w = world("SLACK");
  w.setState(both);
  w.applyDesk();
  await flush();
  const [chatD, slackD] = w.desks;
  check(w.desks.length === 2 && chatD.name === "CHAT" && slackD.name === "SLACK", "two desks in state order");
  check(w.ids.ctgt.children.map(b => b.textContent).join() === "CHAT,SLACK", "switch buttons: " + w.ids.ctgt.children.map(b => b.textContent));
  check(w.shown === slackD && !slackD.els.wrap.hidden && chatD.els.wrap.hidden && w.ids["chat-wrap"].hidden, "remembered SLACK shows on load");
  check(w.fetched.join() === "/fleet/session?desk=SLACK&t=slack-triage-c4a100", "only SLACK polls, pinned: " + w.fetched);

  // A draft for SLACK stays in SLACK's box when CHAT shows.
  slackD.els.say.value = "for slack";
  slackD.els.say.fire("input");
  chatD.btn.fire("click");
  await flush();
  check(w.shown === chatD && w.store["fleet.chatTarget"] === "CHAT", "CHAT shown and remembered");
  check(chatD.els.say.value === "" && slackD.els.say.value === "for slack", "drafts stay in their own boxes");
  check(w.fetched[w.fetched.length - 1] === "/fleet/session?desk=CHAT&t=desk", "CHAT polls once shown: " + w.fetched);
  check(chatD.els.say.placeholder === "Write to CHAT" && slackD.els.say.placeholder === "Write to SLACK", "placeholders name their desk");

  // SLACK's send does nothing while CHAT shows; CHAT's sends CHAT's text to
  // CHAT.
  slackD.els.send.fire("click");
  await flush();
  check(w.said.length === 0, "a hidden desk sent: " + JSON.stringify(w.said));
  chatD.els.say.value = "for chat";
  chatD.els.send.fire("click");
  await flush();
  check(JSON.stringify(w.said) === JSON.stringify([{target: "desk", text: "for chat", desk: "CHAT"}]), "CHAT send: " + JSON.stringify(w.said));
  check(chatD.els.say.value === "" && slackD.els.say.value === "for slack", "CHAT's send cleared CHAT's box only");

  slackD.btn.fire("click");
  await flush();
  slackD.els.send.fire("click");
  await flush();
  check(JSON.stringify(w.said[1]) === JSON.stringify({target: "slack-triage-c4a100", text: "for slack", desk: "SLACK"}), "SLACK send: " + JSON.stringify(w.said));

  // SLACK goes away while shown: the view stays, send is off, the reason
  // names SLACK. CHAT stays usable.
  w.setState({desks: [both.desks[0], {name: "SLACK", alive: false, reason: "not running"}]});
  w.applyDesk();
  slackD.els.say.value = "late";
  slackD.els.send.fire("click");
  await flush();
  check(w.shown === slackD && !slackD.els.wrap.hidden, "a desk going away moved the view");
  check(slackD.btn.disabled && !chatD.btn.disabled, "SLACK's button off, CHAT's on");
  check(slackD.els.send.disabled && w.said.length === 2, "send to a gone desk");
  check(slackD.els.noteT.textContent === "SLACK is not running." && !slackD.els.note.hidden, "note: " + slackD.els.noteT.textContent);
  check(w.ids["ctgt-why"].textContent === "SLACK is not running", "why: " + w.ids["ctgt-why"].textContent);
  check(slackD.btn.title === "SLACK is not running" && chatD.btn.title === "Chat with your CHAT session", "titles: " + slackD.btn.title + " / " + chatD.btn.title);

  // FLEET shows: a disabled desk cannot be picked; the line gives every
  // unusable desk's reason.
  w.ids["ctgt-fleet"].fire("click");
  slackD.btn.fire("click");
  check(w.shown === null && w.store["fleet.chatTarget"] === "fleet", "FLEET shown and remembered");
  w.setState({desks: [{name: "CHAT", alive: false, reason: "ambiguous"}, {name: "SLACK", alive: false, reason: "shares tmux"}]});
  w.applyDesk();
  check(w.ids["ctgt-why"].textContent === "Two sessions are named CHAT · SLACK shares its tmux session", "why on FLEET: " + w.ids["ctgt-why"].textContent);

  // The remembered desk opens only when usable; names that are not desk
  // names never get a button; "chat" is CHAT, as an older page saved it.
  const w2 = world("SLACK");
  w2.setState({desks: [both.desks[0], {name: "SLACK", alive: false, reason: "not running"}, {name: "bad name"}, {name: "FLEET"}]});
  w2.applyDesk();
  await flush();
  check(w2.shown === null && w2.fetched.length === 0, "an unusable remembered desk opened");
  check(w2.desks.map(d => d.name).join() === "CHAT,SLACK", "desks: " + w2.desks.map(d => d.name));
  const w3 = world("chat");
  check(w3.deskPref() === "CHAT", "legacy chat pref");
  w3.setState(both);
  w3.applyDesk();
  check(!!w3.shown && w3.shown.name === "CHAT", "legacy chat pref opens CHAT");
  console.log(JSON.stringify(fails));
})().catch(e => console.log(JSON.stringify(["threw: " + e.stack])));
`

// TestFleetPageDeskTargets runs the page's chat target module (fleet.html,
// "chat target" up to "refresh") under node: two desks, each with its own
// box; a draft for one never goes to the other; a desk that goes away keeps
// the view and blocks send with its own reason. Skipped where node is not
// installed.
func TestFleetPageDeskTargets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page := string(fleetPageHTML)
	i := strings.Index(page, "  // ---- chat target: FLEET or a desk")
	j := strings.Index(page, "  // ---- refresh ---")
	if i < 0 || j < i {
		t.Fatal("fleet.html: chat target .. refresh not found in order")
	}
	src := strings.Replace(deskPageWorld, "/*MODULE*/", page[i:j], 1) + deskPageDrive
	out, err := exec.Command(node, "-e", src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
	var fails []string
	if err := json.Unmarshal(out, &fails); err != nil {
		t.Fatalf("node output %q: %v", out, err)
	}
	for _, f := range fails {
		t.Error(f)
	}
}
