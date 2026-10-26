package main

import (
	"net/url"
	"regexp"
	"strings"
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

// fleetMDLink matches a markdown link at the start of s. It returns the
// label, the url and the byte length of the whole link.
func fleetMDLink(s string) (label, href string, n int, ok bool) {
	m := fleetMDLinkRe.FindStringSubmatchIndex(s)
	if m == nil {
		return "", "", 0, false
	}
	return s[m[2]:m[3]], s[m[4]:m[5]], m[1], true
}
