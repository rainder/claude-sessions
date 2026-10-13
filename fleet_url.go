package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
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

// fleetLoginURL builds the one-time GET /fleet?c=<code> login link. The code
// is query-escaped even though base64url never needs it.
func fleetLoginURL(host string, port int, code string) string {
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/fleet",
		RawQuery: url.Values{"c": {code}}.Encode()}
	return u.String()
}

// fleetFindServer returns the base URL of this host's running server, or "".
// Tailscale first, then loopback, like cmdPair: a --bind tailscale server is
// not on loopback at all. A package var so a test can point the CLI at an
// httptest server.
var fleetFindServer = func(port int, token string) string {
	if ts := tailscaleIPv4(); ts != "" {
		if base := serverBaseURL(ts, port, token); base != "" {
			return base
		}
	}
	return serverBaseURL("127.0.0.1", port, token)
}

// fleetLoginPost posts to a /fleet/login route with the bearer token and
// returns the body when the status is want.
func fleetLoginPost(base, token, path string, want int) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != want {
		return nil, fmt.Errorf("%s: %s %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// cmdFleetURL asks the running server for a one-time login code and prints
// the FLEET page link that redeems it. The server token is used only as the
// bearer header of that request; it is never printed and never in the link.
// It reads the existing token and never creates one: a token the running
// server never loaded could not arm anything. --revoke logs every device out.
func cmdFleetURL(args []string) int {
	fs := flag.NewFlagSet("fleet-url", flag.ContinueOnError)
	host := fs.String("host", "", "host name or address the browser uses")
	port := fs.Int("port", defaultServerPort, "server port")
	revoke := fs.Bool("revoke", false, "log out every device that has opened the FLEET page")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h := *host
	if !*revoke {
		// Before arming: a code with no usable link would only sit there.
		if h == "" {
			h = fleetTailscaleHost()
		}
		if h == "" {
			fmt.Fprintln(os.Stderr, "fleet-url: no Tailscale address; pass --host")
			return 1
		}
	}
	tok, err := readServerToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet-url: no server token; start the server once")
		return 1
	}
	base := fleetFindServer(*port, tok)
	if base == "" {
		fmt.Fprintf(os.Stderr, "fleet-url: no server answering on port %d; is `claude-sessions -s` running on this host?\n", *port)
		return 1
	}
	if *revoke {
		if _, err := fleetLoginPost(base, tok, "/fleet/login/revoke", http.StatusNoContent); err != nil {
			fmt.Fprintf(os.Stderr, "fleet-url: revoke failed (%v)\n", err)
			return 1
		}
		fmt.Println("All FLEET page logins are revoked.")
		return 0
	}
	body, err := fleetLoginPost(base, tok, "/fleet/login/arm", http.StatusOK)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet-url: could not get a login code (%v)\n", err)
		return 1
	}
	var out struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Code == "" {
		fmt.Fprintln(os.Stderr, "fleet-url: the server answered without a login code")
		return 1
	}
	fmt.Println(fleetLoginURL(h, *port, out.Code))
	fmt.Fprintln(os.Stderr, "Open this link once, on one device, within 10 minutes. It works one time.")
	return 0
}
