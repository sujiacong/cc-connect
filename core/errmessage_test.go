package core

import (
	"strings"
	"testing"
)

// TestErrMsgForUser pins the condensation of an agent error before it reaches
// the chat: short errors pass through, long stderr dumps collapse to the cause
// line. Regression for the 2026-09-18 event where a 52,495-byte stderr blob
// was sent to a lanxin group verbatim.
func TestErrMsgForUser(t *testing.T) {
	short := "Session not found"
	if got := errMsgForUser(short); got != short {
		t.Fatalf("short error must pass through unchanged, got %q", got)
	}

	blob := strings.Repeat("/icg/data/root/icg-ai-bot/hw_knowledge_base/:\nfulltable\nindex.md\n", 200) +
		"error: failed to run prompt: provider.api_error: 504 Gateway Time-out"
	got := errMsgForUser(blob)
	if n := len([]rune(got)); n > maxUserErrMsgRunes+120 {
		t.Fatalf("condensed error still too long: %d runes", n)
	}
	if !strings.Contains(got, "504 Gateway Time-out") {
		t.Fatalf("condensed error lost the cause line: %q", got)
	}
	if strings.Contains(got, "hw_knowledge_base") {
		t.Fatalf("condensed error still leaks internal context: %q", got)
	}

	// no line mentions "error" -> fall back to the last non-empty line
	noErr := strings.Repeat("noise noise noise\n", 100) + "final line says nothing"
	if got2 := errMsgForUser(noErr); !strings.Contains(got2, "final line says nothing") {
		t.Fatalf("fallback should keep the last non-empty line, got %q", got2)
	}
}
