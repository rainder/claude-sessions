package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

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
	payload := buildAlertPayload(shortHostname(), hostID, notifyEvent{
		Kind: notifyAlert, SessionID: "notify-test", Name: "notify-test",
		WaitingFor: "test notification",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	failed := 0
	for _, d := range devices {
		err := client.Send(ctx, pushRequest{
			DeviceToken: d.Token,
			Topic:       cfg.BundleID,
			CollapseID:  hostID + ":notify-test",
			PushType:    "alert",
			Priority:    "10",
			Environment: d.Environment,
			Payload:     payload,
		})
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
