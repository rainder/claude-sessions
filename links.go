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

// fleetDomainLabelRe is a label that reads as a web address: an optional
// scheme, a host with a dot and a letter TLD, an optional port and path. Its
// group 1 is the host. A short id such as 4388c878 has no dot and no match.
// fleet.html's LABEL_HOST is the same rule.
var fleetDomainLabelRe = regexp.MustCompile(`(?i)^(?:[a-z][a-z0-9+.-]*://)?([a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,})(?::\d+)?(?:[/?#]\S*)?$`)

// fleetLinkLabel is what a markdown link shows. A label that reads as a URL
// or a domain must not point somewhere else: [docs.example.com](https://evil)
// would show one host and open another. So when the label names a host other
// than href's, or holds a URL whose host cannot be read, the link shows href
// itself. Any other label (a short id, a word, "PR 3301") shows as written.
func fleetLinkLabel(label, href string) string {
	t := strings.TrimSpace(label)
	m := fleetDomainLabelRe.FindStringSubmatch(t)
	if m == nil {
		if strings.Contains(t, "://") || fleetURLRe.MatchString(t) {
			return href
		}
		return label
	}
	p, err := url.Parse(href)
	if err != nil || !strings.EqualFold(m[1], p.Hostname()) {
		return href
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
