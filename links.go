package main

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Terminal hyperlinks (OSC 8): ESC ]8;params;URI ST text ESC ]8;; ST. The
// terminal shows only text and opens URI on Cmd+click (iTerm2). The escapes
// take no columns: visualLen and clipLine (render.go) skip them through
// escapeLen, and clipLine closes a link the cut left open.
const (
	osc8Prefix = "\x1b]8;"
	osc8Close  = "\x1b]8;;\x1b\\"
)

// osc8Open is the sequence that starts a hyperlink to uri. uri must be one
// safeLinkURL accepted.
func osc8Open(uri string) string { return osc8Prefix + ";" + uri + "\x1b\\" }

// osc8Opens reports whether the escape sequence seq is an OSC 8 (ok) and, if
// so, whether it opens a link (a non-empty URI) or closes one.
func osc8Opens(seq string) (open, ok bool) {
	if !strings.HasPrefix(seq, osc8Prefix) {
		return false, false
	}
	body := strings.TrimSuffix(strings.TrimSuffix(seq[len(osc8Prefix):], "\x1b\\"), "\a")
	j := strings.IndexByte(body, ';')
	if j < 0 {
		return false, true
	}
	return body[j+1:] != "", true
}

// safeLinkURL reports whether u may be a terminal hyperlink: an http or https
// URL with a host, all printable ASCII without a space. Anything else (a
// javascript: or file: URL, a control byte that would end the OSC early) is
// no link; the text still shows.
func safeLinkURL(u string) bool {
	if u == "" {
		return false
	}
	for i := 0; i < len(u); i++ {
		if u[i] <= ' ' || u[i] > '~' {
			return false
		}
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return false
	}
	return p.Scheme == "http" || p.Scheme == "https"
}

// fleetMDLinkRe is a markdown link [label](url) at the start of the text, the
// FLEET page's rule (fleet.html RICH_RE): the label has no "]" or newline,
// the url no space and at most one level of balanced parentheses.
var fleetMDLinkRe = regexp.MustCompile(`^\[([^\]\n]+)\]\(((?:[^()\s]|\([^()\s]*\))+)\)`)

// fleetLabelTokenSep splits a link label into the runs a host could hide in.
var fleetLabelTokenSep = regexp.MustCompile(`[^A-Za-z0-9.-]+`)

// fleetLabelDomainRe marks a token as domain-like: a dot followed by a letter.
var fleetLabelDomainRe = regexp.MustCompile(`\.[A-Za-z]`)

// fleetLinkLabel is what a markdown link shows. A label must not show one
// place and open another ([docs.example.com](https://evil.io)), so the rule
// fails closed: the link shows href itself when the label has "://", any
// non-ASCII letter (a confusable such as Cyrillic а), or any domain-like
// token (a dot followed by a letter, anywhere: "click a.b.evil.io here",
// "e.g.") that is not href's own host. A label equal to href shows as it is.
// A short id (4388c878), words without dots and "PR 3301" show as written.
// fleet.html's linkLabel is the same rule (TestFleetPageLinkLabel).
func fleetLinkLabel(label, href string) string {
	if label == href {
		return label
	}
	if strings.Contains(label, "://") {
		return href
	}
	for _, r := range label {
		if r > unicode.MaxASCII && unicode.IsLetter(r) {
			return href
		}
	}
	p, err := url.Parse(href)
	if err != nil {
		return href
	}
	for _, tok := range fleetLabelTokenSep.Split(label, -1) {
		tok = strings.TrimRight(tok, ".")
		if fleetLabelDomainRe.MatchString(tok) && !strings.EqualFold(tok, p.Hostname()) {
			return href
		}
	}
	return label
}

// fleetMDLink matches a markdown link at the start of s. It returns the
// label, the url and the byte length of the whole link.
func fleetMDLink(s string) (label, href string, n int, ok bool) {
	m := fleetMDLinkRe.FindStringSubmatchIndex(s)
	if m == nil {
		return "", "", 0, false
	}
	return s[m[2]:m[3]], s[m[4]:m[5]], m[1], true
}
