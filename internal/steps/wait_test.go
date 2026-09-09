package steps

import (
	"context"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/scenario"
)

func TestExecuteWait_Duration_Pauses(t *testing.T) {
	step := &scenario.Step{Duration: "50ms"}
	start := time.Now()
	err := ExecuteWait(context.Background(), step)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("expected at least 50ms pause, got %s", elapsed)
	}
}

func TestExecuteWait_CancelledContext_ReturnsError(t *testing.T) {
	step := &scenario.Step{Duration: "10s"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ExecuteWait(ctx, step)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}
