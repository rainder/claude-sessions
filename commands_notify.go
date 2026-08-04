package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

// testPushRequest builds the one push a "is my phone actually receiving these"
// check sends. It lives here, called by both `notify-test` and the server's
// POST /devices/{token}/test, so the CLI and the phone exercise the same push
// rather than two copies that drift apart.
//
// The device's own environment rides along: a sandbox token registered against
// the production gateway reads as a dead token, and reproducing the registered
// value is the only way the test reveals it.
func testPushRequest(hostName, hostID, bundleID string, d Device) pushRequest {
	return pushRequest{
		DeviceToken: d.Token,
		Topic:       bundleID,
		CollapseID:  hostID + ":notify-test",
		PushType:    "alert",
		Priority:    "10",
		Environment: d.Environment,
		Payload: buildAlertPayload(hostName, hostID, notifyEvent{
			Kind: notifyAlert, SessionID: "notify-test", Name: "notify-test",
			WaitingFor: "test notification",
		}),
	}
}

// cmdNotifyTest pushes a test notification to every registered device.
//
// The worst failure mode of a watchdog is dying silently — a dead server and a
// quiet week look identical from the phone. This is the command that answers
// "is it actually working", printing Apple's own reason string per device
// rather than a generic failure.
func cmdNotifyTest(args []string) int {
	cfg, err := LoadAPNsConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	client, err := newAPNsClient(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	store := LoadDeviceStore()
	devices := store.List()
	if len(devices) == 0 {
		fmt.Fprintln(os.Stderr, "no devices registered — run `claude-sessions pair` and scan the QR")
		return 1
	}
	hostID := LoadHostID()
	hostName := shortHostname()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	failed := 0
	for _, d := range devices {
		err := client.Send(ctx, testPushRequest(hostName, hostID, cfg.BundleID, d))
		if err != nil {
			failed++
			fmt.Printf("%s  FAILED  %v\n", shortToken(d.Token), err)
			continue
		}
		fmt.Printf("%s  sent\n", shortToken(d.Token))
	}
	if failed > 0 {
		return 1
	}
	return 0
}
