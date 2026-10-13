package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// fleetTailscaleHost names the host a browser on the tailnet should use. A
// package var so a test never shells out to a real tailscale CLI.
var fleetTailscaleHost = defaultFleetTailscaleHost

// defaultFleetTailscaleHost prefers the MagicDNS name (stable, readable on a
// phone) and falls back to the Tailscale IPv4 address; "" when neither is
// known.
func defaultFleetTailscaleHost() string {
	if bin := tailscaleBinary(); bin != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "status", "--json")
		cmd.Env = tailscaleCLIEnv(os.Environ())
		if out, err := cmd.Output(); err == nil {
			var st struct {
				Self struct {
					DNSName string `json:"DNSName"`
				} `json:"Self"`
			}
			if json.Unmarshal(out, &st) == nil {
				if name := strings.TrimSuffix(st.Self.DNSName, "."); name != "" {
					return name
				}
			}
		}
	}
	return tailscaleIPv4()
}

// fleetLoginURL builds the one-time GET /fleet?t=<token> login link. The
// token is query-escaped: a base64 token may hold '+' or '/'.
func fleetLoginURL(host string, port int, token string) string {
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/fleet",
		RawQuery: url.Values{"t": {token}}.Encode()}
	return u.String()
}

// cmdFleetURL prints the FLEET page login URL. It reads the existing server
// token and never creates one: a token the running server never loaded would
// make a URL that cannot log in. The token goes to stdout only.
func cmdFleetURL(args []string) int {
	fs := flag.NewFlagSet("fleet-url", flag.ContinueOnError)
	host := fs.String("host", "", "host name or address the browser uses")
	port := fs.Int("port", defaultServerPort, "server port")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h := *host
	if h == "" {
		h = fleetTailscaleHost()
	}
	if h == "" {
		fmt.Fprintln(os.Stderr, "fleet-url: no Tailscale address; pass --host")
		return 1
	}
	tok, err := readServerToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet-url: no server token; start the server once")
		return 1
	}
	fmt.Println(fleetLoginURL(h, *port, tok))
	fmt.Fprintln(os.Stderr, "Open this URL once per device. It contains the server token; do not share it.")
	return 0
}
