package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// ExecuteWait pauses execution for the specified duration.
func ExecuteWait(ctx context.Context, step *scenario.Step) error {
	d, err := time.ParseDuration(step.Duration)
	if err != nil {
		return fmt.Errorf("wait parse duration %q: %w", step.Duration, err)
	}

	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
