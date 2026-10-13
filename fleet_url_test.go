package main

import "testing"

func TestFleetLoginURL(t *testing.T) {
	got := fleetLoginURL("ws.example.ts.net", 8765, "a+b/c")
	if got != "http://ws.example.ts.net:8765/fleet?t=a%2Bb%2Fc" {
		t.Fatalf("got %q", got)
	}
	if got := fleetLoginURL("100.1.2.3", 9000, "tok"); got != "http://100.1.2.3:9000/fleet?t=tok" {
		t.Fatalf("got %q", got)
	}
}

func TestCmdFleetURLBadFlag(t *testing.T) {
	if code := cmdFleetURL([]string{"--port", "x"}); code != 2 {
		t.Fatalf("code %d", code)
	}
}
