package kimi

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestStderrTail pins the truncation of a failing process's stderr before it is
// turned into an error event: only the tail survives, so kimi's full tool
// context dump cannot reach the chat.
func TestStderrTail(t *testing.T) {
	if got := stderrTail("boom"); got != "boom" {
		t.Fatalf("single short line must pass through, got %q", got)
	}

	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("noise line\n")
	}
	b.WriteString("error: failed to run prompt: provider.api_error: 504 Gateway Time-out")
	got := stderrTail(b.String())
	if strings.Contains(got, "noise line") {
		t.Fatalf("tail must drop leading noise, got %q", got)
	}
	if !strings.Contains(got, "504 Gateway Time-out") {
		t.Fatalf("tail must keep the cause, got %q", got)
	}

	// a single enormous line is rune-capped, keeping its end
	got2 := stderrTail(strings.Repeat("x", 5000) + "TAIL")
	if n := len([]rune(got2)); n > 601 {
		t.Fatalf("huge line not capped: %d runes", n)
	}
	if !strings.HasSuffix(got2, "TAIL") {
		t.Fatalf("cap must keep the end of the line, got %q", got2)
	}
}

// TestTurnAbortMessage pins what the user sees when a turn is aborted through
// the context. A deadline (agent timeout_mins) must explain itself; an explicit
// cancel must stay silent — reporting it produced the bare
// "read stdout: read |0: file already closed" that reached a group on 2026-09-22.
func TestTurnAbortMessage(t *testing.T) {
	got := turnAbortMessage(context.DeadlineExceeded, 20*time.Minute)
	if !strings.Contains(got, "20m0s") {
		t.Fatalf("deadline message should name the limit, got %q", got)
	}
	if !strings.Contains(got, "未完成") {
		t.Fatalf("deadline message should say it was aborted, got %q", got)
	}

	if msg := turnAbortMessage(context.Canceled, 20*time.Minute); msg != "" {
		t.Fatalf("explicit cancel must stay silent, got %q", msg)
	}
	if msg := turnAbortMessage(nil, time.Minute); msg != "" {
		t.Fatalf("nil context error must stay silent, got %q", msg)
	}

	// a wrapped deadline is still a deadline
	wrapped := fmt.Errorf("read stdout: %w", context.DeadlineExceeded)
	if msg := turnAbortMessage(wrapped, 0); msg == "" || !strings.Contains(msg, "超时") {
		t.Fatalf("wrapped deadline must still be reported, got %q", msg)
	}
}
