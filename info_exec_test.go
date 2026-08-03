package main

import (
	"context"
	"errors"
	"testing"
)

func TestCuFetchFuncSeamIsSwappable(t *testing.T) {
	prev := cuFetchFunc
	t.Cleanup(func() { cuFetchFunc = prev })
	called := false
	cuFetchFunc = func(ctx context.Context, ticketID string) ([]byte, error) {
		called = true
		if ticketID != "DR-1" {
			t.Errorf("ticketID = %q, want DR-1", ticketID)
		}
		return []byte("ticket body"), nil
	}
	out, err := cuFetchFunc(context.Background(), "DR-1")
	if err != nil || string(out) != "ticket body" || !called {
		t.Errorf("got (%q, %v), called=%v", out, err, called)
	}
}

func TestClaudeSummarizeFuncSeamIsSwappable(t *testing.T) {
	prev := claudeSummarizeFunc
	t.Cleanup(func() { claudeSummarizeFunc = prev })
	claudeSummarizeFunc = func(ctx context.Context, instruction string, input []byte) ([]byte, error) {
		if instruction != "instr" || string(input) != "data" {
			return nil, errors.New("wrong args")
		}
		return []byte("summary"), nil
	}
	out, err := claudeSummarizeFunc(context.Background(), "instr", []byte("data"))
	if err != nil || string(out) != "summary" {
		t.Errorf("got (%q, %v)", out, err)
	}
}
