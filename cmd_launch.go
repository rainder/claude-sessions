package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

var allowedCmdBinaries = []string{"claude", "grok", "claudex", "claudexx"}

func validateCmdBinary(cmd string) error {
	if cmd == "" {
		return fmt.Errorf("command binary must not be empty")
	}
	if filepath.Base(cmd) != cmd || strings.ContainsAny(cmd, `/\`) {
		return fmt.Errorf("command binary must be a bare name, not a path: %s", cmd)
	}
	for _, allowed := range allowedCmdBinaries {
		if cmd == allowed {
			return nil
		}
	}
	return fmt.Errorf("command binary not allowed: %s", cmd)
}

func launchFromCmd(cmd string, args []string, prompt string) (string, error) {
	if err := validateCmdBinary(cmd); err != nil {
		return "", err
	}
	parts := []string{cmd}
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	if prompt != "" {
		parts = append(parts, shellQuote(prompt))
	}
	return strings.Join(parts, " "), nil
}
