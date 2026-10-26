package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSafeLinkURL(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"https://brain.dev.trecs.dev/document-pages/4388c878-1111-2222-3333-444455556666", true},
		{"http://a.b/c?x=1;y=2", true},
		{"https://h", true},
		{"", false},
		{"javascript:alert(1)", false},
		{"ftp://h/f", false},
		{"file:///etc/passwd", false},
		{"https://", false},
		{"https:/x", false},
		{"https://a b", false},
		{"https://a.b/\a", false},
		{"https://a.b/\x1b\\", false},
		{"https://ä.de", false},
	} {
		if got := safeLinkURL(c.in); got != c.want {
			t.Errorf("safeLinkURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestOSC8Opens(t *testing.T) {
	for _, c := range []struct {
		seq      string
		open, ok bool
	}{
		{osc8Open("https://x.y"), true, true},
		{"\x1b]8;id=1;https://x.y\a", true, true},
		{osc8Close, false, true},
		{"\x1b]8;;\a", false, true},
		{"\x1b]0;title\a", false, false},
	} {
		if open, ok := osc8Opens(c.seq); open != c.open || ok != c.ok {
			t.Errorf("osc8Opens(%q) = %v,%v want %v,%v", c.seq, open, ok, c.open, c.ok)
		}
	}
}

func TestFleetSummaryMarkdownLink(t *testing.T) {
	const u = "https://brain.dev.trecs.dev/document-pages/4388c878-1111-2222-3333-444455556666"
	l := fleetSummaryStyledLines("dev page ["+"4388c878]("+u+") is wrong", 120)
	if len(l) != 1 {
		t.Fatalf("lines = %q", l)
	}
	if got := stripANSI(l[0]); got != "dev page 4388c878 is wrong" {
		t.Errorf("visible = %q", got)
	}
	want := osc8Open(u) + "\033[4;34m4388c878" + ansiReset + osc8Close
	if !strings.Contains(l[0], want) {
		t.Errorf("line %q lacks the link %q", l[0], want)
	}
	if visualLen(l[0]) != len("dev page 4388c878 is wrong") {
		t.Errorf("visualLen = %d", visualLen(l[0]))
	}
	// The plain render shows the label only.
	if got := fleetSummaryLines("see [PR 3301]("+u+") now", 80); len(got) != 1 || got[0] != "see PR 3301 now" {
		t.Errorf("plain = %q", got)
	}
}

func TestFleetSummaryLinkRejected(t *testing.T) {
	for _, in := range []string{
		"x [label](javascript:alert(1)) y",
		"x [label](ftp://h/f) y",
		"x [label](file:///etc/passwd) y",
	} {
		l := fleetSummaryStyledLines(in, 80)
		if strings.Contains(l[0], osc8Prefix) {
			t.Errorf("%q: link emitted: %q", in, l[0])
		}
		if got := stripANSI(l[0]); got != "x label y" {
			t.Errorf("%q: visible %q, want the label only", in, got)
		}
	}
	// Inside code a [x](url) stays literal text.
	l := fleetSummaryStyledLines("run `[x](https://a.b)` now", 80)
	if got := stripANSI(l[0]); got != "run [x](https://a.b) now" {
		t.Errorf("code: visible %q", got)
	}
}

func TestFleetSummaryBareURLLink(t *testing.T) {
	l := fleetSummaryStyledLines("see https://h/x. and ftp://h/y", 80)
	if !strings.Contains(l[0], osc8Open("https://h/x")+"\033[4;34mhttps://h/x"+ansiReset+osc8Close+".") {
		t.Errorf("bare URL: %q", l[0])
	}
	if strings.Count(l[0], osc8Prefix) != 2 { // one open, one close
		t.Errorf("ftp URL got a link: %q", l[0])
	}
	// A URL label equal to its target shows as written, linked once.
	l = fleetSummaryStyledLines("[https://docs.trecs.dev/f](https://docs.trecs.dev/f)", 80)
	if l[0] != osc8Open("https://docs.trecs.dev/f")+"\033[4;34mhttps://docs.trecs.dev/f"+ansiReset+osc8Close {
		t.Errorf("URL label: %q", l[0])
	}
	// A URL label on another host shows the real target instead.
	l = fleetSummaryStyledLines("[https://a.b/c](https://d.e/f)", 80)
	if l[0] != osc8Open("https://d.e/f")+"\033[4;34mhttps://d.e/f"+ansiReset+osc8Close {
		t.Errorf("mismatched URL label: %q", l[0])
	}
}

// fleetLinksBalanced reports whether every OSC 8 open in line is closed
// before the line ends, with no nesting.
func fleetLinksBalanced(line string) bool {
	open := false
	for i := 0; i < len(line); {
		if n := escapeLen(line, i); n > 0 {
			if o, ok := osc8Opens(line[i : i+n]); ok {
				if o == open {
					return false
				}
				open = o
			}
			i += n
			continue
		}
		i++
	}
	return !open
}

func TestFleetSummaryWrappedLinkReopens(t *testing.T) {
	const u = "https://d.e/f"
	lines := fleetSummaryStyledLines("see [alpha beta gamma delta]("+u+") end", 11)
	words := 0
	for _, l := range lines {
		if !fleetLinksBalanced(l) {
			t.Errorf("unbalanced link in %q", l)
		}
		for _, w := range []string{"alpha", "beta", "gamma", "delta"} {
			if !strings.Contains(l, w) {
				continue
			}
			words++
			i := strings.Index(l, w)
			if strings.LastIndex(l[:i], osc8Open(u)) < 0 {
				t.Errorf("%q on line %q is not inside the link", w, l)
			}
		}
		if strings.Contains(l, "end") && strings.Contains(l[strings.LastIndex(l, osc8Close)+1:], osc8Prefix) {
			t.Errorf("link runs past its label: %q", l)
		}
	}
	if words != 4 || len(lines) < 3 {
		t.Fatalf("wrap = %q", lines)
	}
}

func TestVisualLenOSC8(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{osc8Open("https://x.y") + "abc" + osc8Close, 3},
		{"\x1b]8;;https://x\aabc\x1b]8;;\a", 3},
		{"a" + osc8Open("https://x.y") + "\033[1m⛵\033[0m" + osc8Close + "b", 4},
		{"\x1b]8;;https://unterminated", 0},
	} {
		if got := visualLen(c.in); got != c.want {
			t.Errorf("visualLen(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestClipLineClosesLink(t *testing.T) {
	const u = "https://x.y/z"
	// Cut inside the label: the close past the cut is still written.
	if got := clipLine("ab"+osc8Open(u)+"cdef"+osc8Close+"gh", 4); got != "ab"+osc8Open(u)+"cd"+osc8Close {
		t.Errorf("cut in link = %q", got)
	}
	// A link the input never closes is closed at the end.
	if got := clipLine("ab"+osc8Open(u)+"cdef", 3); got != "ab"+osc8Open(u)+"c"+osc8Close {
		t.Errorf("unclosed link = %q", got)
	}
	// The open is never cut, even at width 0, and the URI never counts.
	got := clipLine(osc8Open(u)+"x"+osc8Close, 0)
	if got != osc8Open(u)+osc8Close {
		t.Errorf("width 0 = %q", got)
	}
	if got := clipLine(osc8Open(u)+"xyz"+osc8Close, 3); got != osc8Open(u)+"xyz"+osc8Close {
		t.Errorf("exact fit = %q", got)
	}
}

// TestFleetSummaryFrameLinks renders a card whose summary has a markdown link
// longer than the panel: no frame line is wider than the terminal, and every
// line closes the links it opens.
func TestFleetSummaryFrameLinks(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := fleetTestCard("a", "needs-you")
	c.Summary = "dev page [4388c878 and its very long label text](https://brain.dev.trecs.dev/document-pages/4388c878-1111) needs a look https://brain.dev.trecs.dev/projects/0123456789abcdef"
	for _, cols := range []int{12, 20, 33, 60, 120} {
		frame := renderFleetView(fleetTestState(fleetTestView(c), now), cols, 40, now)
		for _, l := range strings.Split(frame, "\n") {
			if visibleWidth(l) > cols {
				t.Errorf("cols=%d: line %q is %d wide", cols, l, visibleWidth(l))
			}
			if !fleetLinksBalanced(l) {
				t.Errorf("cols=%d: unbalanced link in %q", cols, l)
			}
		}
	}
}

// fleetLinkLabelCase is one label for a link to fleetLinkLabelHref, and
// whether it must show the href instead.
type fleetLinkLabelCase struct {
	label  string
	toHref bool
}

const fleetLinkLabelHref = "https://brain.dev.trecs.dev/document-pages/4388c878-1111"

var fleetLinkLabelCases = []fleetLinkLabelCase{
	// Kept: short ids, words, the href's own host, the href itself.
	{"4388c878", false},
	{"PR 3301", false},
	{"dev page", false},
	{"v1.2 notes", false},
	{"brain.dev.trecs.dev", false},
	{"BRAIN.dev.trecs.dev/document-pages/x", false},
	{"see brain.dev.trecs.dev.", false},
	{fleetLinkLabelHref, false},
	// Replaced: any scheme, another host anywhere, a host that only starts
	// with the real one, a mail address, a confusable letter, a non-ASCII
	// letter, any other dotted word.
	{"https://brain.dev.trecs.dev", true},
	{"javascript://x", true},
	{"docs.example.com", true},
	{"click brain.dev.trecs.dev.evil.io here", true},
	{"brain.dev.trecs.dev-x.io", true},
	{"brain.dev.trecs.dev evil.io", true},
	{"user@evil.io", true},
	{"br\u0430in.dev.trecs.dev", true}, // Cyrillic а
	{"caf\u00e9", true},
	{"e.g. this", true},
	{"render.go", true},
}

func TestFleetLinkLabel(t *testing.T) {
	for _, c := range fleetLinkLabelCases {
		want := c.label
		if c.toHref {
			want = fleetLinkLabelHref
		}
		if got := fleetLinkLabel(c.label, fleetLinkLabelHref); got != want {
			t.Errorf("fleetLinkLabel(%q) = %q, want %q", c.label, got, want)
		}
	}
	// In a summary the deceptive label is gone and the target shows.
	l := fleetSummaryStyledLines("open [github.com/x](https://evil.io/y)", 80)
	if got := stripANSI(l[0]); got != "open https://evil.io/y" {
		t.Errorf("summary = %q", got)
	}
	l = fleetSummaryStyledLines("open [click brain.dev.trecs.dev.evil.io here](https://evil.io/y)", 80)
	if got := stripANSI(l[0]); got != "open https://evil.io/y" {
		t.Errorf("summary = %q", got)
	}
}

// TestFleetPageLinkLabel runs the page's linkLabel (fleet.html) under node on
// the same cases as fleetLinkLabel, so the two rules cannot drift apart.
// Skipped where node is not installed.
func TestFleetPageLinkLabel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page := string(fleetPageHTML)
	fn := strings.Index(page, "  function linkLabel(")
	if fn < 0 {
		t.Fatal("fleet.html has no linkLabel")
	}
	end := fn + strings.Index(page[fn:], "\n  }\n") + len("\n  }\n")
	var labels []string
	for _, c := range fleetLinkLabelCases {
		labels = append(labels, c.label)
	}
	js, _ := json.Marshal(labels)
	src := `const str = v => typeof v === "string" ? v : "";` + page[fn:end] +
		`const url = "` + fleetLinkLabelHref + `", u = new URL(url);` +
		`console.log(JSON.stringify(` + string(js) + `.map(l => linkLabel(l, url, u))));`
	out, err := exec.Command(node, "-e", src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %q: %v", out, err)
	}
	for i, l := range labels {
		if want := fleetLinkLabel(l, fleetLinkLabelHref); got[i] != want {
			t.Errorf("page linkLabel(%q) = %q, TUI shows %q", l, got[i], want)
		}
	}
}
