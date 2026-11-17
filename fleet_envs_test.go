package main

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// fleetEnvsCases are the `envs` values both readers must agree on.
var fleetEnvsCases = []struct {
	name string
	envs any
	want []string
}{
	{"one", []any{"prod"}, []string{"prod"}},
	{"display order", []any{"local", "dev", "prod", "predev", "sandbox"}, []string{"prod", "sandbox", "dev", "predev", "local"}},
	{"repeats dropped", []any{"dev", "dev", "sandbox"}, []string{"sandbox", "dev"}},
	{"unknown and non-strings dropped", []any{"staging", "Prod", 3, nil, "dev"}, []string{"dev"}},
	{"none alone", []any{"none"}, []string{"none"}},
	{"none next to another drops none", []any{"none", "prod"}, []string{"prod"}},
	{"empty", []any{}, nil},
	{"not an array", "prod", nil},
	{"object", map[string]any{"prod": true}, nil},
	{"missing", nil, nil},
}

func fleetEnvsCardJSON(t *testing.T, envs any) string {
	t.Helper()
	m := map[string]any{"id": "a", "kind": "needs-you"}
	if envs != nil {
		m["envs"] = envs
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFleetDecodeEnvs(t *testing.T) {
	for _, c := range fleetEnvsCases {
		if got := cardFromJSON(t, fleetEnvsCardJSON(t, c.envs)).Envs; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFleetEnvsNotInHash(t *testing.T) {
	base := fleetCardHash(cardFromJSON(t, `{"kind":"needs-you","title":"T"}`))
	for _, e := range []string{`["prod"]`, `["none"]`, `["sandbox","dev"]`} {
		if got := fleetCardHash(cardFromJSON(t, `{"kind":"needs-you","title":"T","envs":`+e+`}`)); got != base {
			t.Errorf("envs %s changed the hash", e)
		}
	}
}

// TestFleetPageEnvList runs the page's envList (fleet.html) under node on
// fleetEnvsCases, so the page and fleetDecodeEnvs agree. Skipped where node
// is not installed.
func TestFleetPageEnvList(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page := string(fleetPageHTML)
	i := strings.Index(page, "  const ENV_ORDER = ")
	j := strings.Index(page, "  // envRow:")
	if i < 0 || j < i {
		t.Fatal("fleet.html: ENV_ORDER .. envRow not found in order")
	}
	cards := make([]map[string]any, len(fleetEnvsCases))
	for k, c := range fleetEnvsCases {
		cards[k] = map[string]any{}
		if c.envs != nil {
			cards[k]["envs"] = c.envs
		}
	}
	js, _ := json.Marshal(cards)
	src := `const arr = v => Array.isArray(v) ? v : [];` + page[i:j] +
		`console.log(JSON.stringify(` + string(js) + `.map(envList)));`
	out, err := exec.Command(node, "-e", src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
	var got [][]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %q: %v", out, err)
	}
	if len(got) != len(fleetEnvsCases) {
		t.Fatalf("got %d results for %d cases", len(got), len(fleetEnvsCases))
	}
	for k, c := range fleetEnvsCases {
		want := c.want
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got[k], want) {
			t.Errorf("%s: page %q, Go %q", c.name, got[k], want)
		}
	}
}

// TestFleetPageEnvRow: every decision card (folded header, whole card) and
// every done or sent row draws the "Affects:" line, with "env: not set" when
// the card has none.
func TestFleetPageEnvRow(t *testing.T) {
	page := string(fleetPageHTML)
	for _, need := range []string{
		`el("span", "envs-l", "Affects:")`, `el("span", "env env-unset", "env: not set")`,
		`el("span", "fold-title", meta, name, envRow(c), leanEl)`,
		"title.append(meta, envRow(c));",
		`el("div", "row-main", l1, envRow(c), l2.childNodes.length ? l2 : null)`,
		`el("div", "row-head", l1, envRow(c), l2.childNodes.length ? l2 : null)`,
		`el("div", "row-main", l1, envRow(c), l2)`,
		".env-prod { color: var(--red);", ".env-sandbox { color: var(--amber);", ".env-dev { color: var(--accent);",
		".env-predev { color: var(--magenta);",
	} {
		if !strings.Contains(page, need) {
			t.Errorf("fleet.html must contain %q", need)
		}
	}
	// Chip text is a fixed label, never the card's own string.
	i := strings.Index(page, "  function envRow(")
	j := strings.Index(page[i:], "\n  }\n")
	if i < 0 || j < 0 {
		t.Fatal("envRow not found")
	}
	if body := page[i : i+j]; strings.Contains(body, "c.envs") || strings.Contains(body, "str(") {
		t.Errorf("envRow must draw only envList's fixed labels:\n%s", body)
	}
}
