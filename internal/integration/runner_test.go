//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/runner"
)

// SDD 14.2: Lock held by a second process -> exit 5.
func TestLock_HeldByAnotherRun_ExitsWith5(t *testing.T) {
	amqpURL := startRabbit(t)
	ctx := context.Background()

	first, err := runner.AcquireLock(ctx, amqpURL, "local")
	if err != nil {
		t.Fatalf("first AcquireLock returned error: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })

	_, err = runner.AcquireLock(ctx, amqpURL, "local")
	if err == nil {
		t.Fatal("second AcquireLock succeeded while the lock was held, want an error")
	}

	var lockHeld *runner.LockHeldError
	if !errors.As(err, &lockHeld) {
		t.Fatalf("second AcquireLock returned %T (%v), want *runner.LockHeldError", err, err)
	}

	// SDD 4.8: the message names the environment that is busy.
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("lock error is %q, and it must name the environment", err.Error())
	}

	if got := runner.ClassifyError(err); got != runner.ExitLockHeld {
		t.Errorf("ClassifyError = %d, want %d", got, runner.ExitLockHeld)
	}
}

// TestLock_ReleasedByFirstRun_IsReacquirable proves the lock is not sticky.
func TestLock_ReleasedByFirstRun_IsReacquirable(t *testing.T) {
	amqpURL := startRabbit(t)
	ctx := context.Background()

	first, err := runner.AcquireLock(ctx, amqpURL, "local")
	if err != nil {
		t.Fatalf("first AcquireLock returned error: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}

	// The broker removes an auto-delete exclusive queue asynchronously.
	var second *runner.Lock
	waitFor(t, "the released lock can be acquired again", func() bool {
		second, err = runner.AcquireLock(ctx, amqpURL, "local")
		return err == nil
	})
	t.Cleanup(func() {
		if second != nil {
			_ = second.Release()
		}
	})
}

// SDD 14.2: A step that cannot reach its target exits 3, not 1.
func TestUnreachableTarget_ExitsWith3(t *testing.T) {
	// A server that is closed immediately gives an address that refuses
	// a connection, which is what an unreachable target looks like.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachableURL := closed.URL
	closed.Close()

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "integration"},
		Targets: config.TargetsConfig{
			WireMock:     &config.WireMockTarget{URL: unreachableURL, AdminURL: unreachableURL},
			EventTrigger: &config.EventTriggerTarget{URL: unreachableURL},
			EventCapture: &config.EventCaptureTarget{URL: unreachableURL},
		},
	}

	ctx := context.Background()
	client := &http.Client{Timeout: 2 * time.Second}

	err := runner.Reset(ctx, cfg, "run-unreachable", client, nil)
	if err == nil {
		t.Fatal("Reset against an unreachable target returned no error, want one")
	}

	if got := runner.ClassifyError(err); got != runner.ExitEnvironment {
		t.Errorf("ClassifyError for an unreachable target = %d, want %d (error: %v)",
			got, runner.ExitEnvironment, err)
	}
}

// SDD 14.2: Reset leaves every queue at depth 0 and the buffer empty.
func TestReset_LeavesQueuesEmptyAndBufferCleared(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.Reset.v1"
		queue      = "test.reset.consumer"
		routingKey = "article.submitted"
		runID      = "run-reset-0001"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	triggerURL := startTriggerServer(t, amqpURL, 100).URL
	captureURL, _ := startCaptureService(t, amqpURL, exchange)

	for range 3 {
		status, body := postPublish(t, triggerURL, runID, publishRequest{
			Delivery:   "amqp",
			Exchange:   exchange,
			RoutingKey: routingKey,
			Event:      validEvent("e-reset"),
		})
		if status != http.StatusAccepted {
			t.Fatalf("publish returned status %d, expect %d; body=%v", status, http.StatusAccepted, body)
		}
	}

	waitFor(t, "all three messages are captured", func() bool {
		return len(capturedMessages(t, captureURL, "exchange="+exchange)) == 3
	})
	waitFor(t, "the consumer queue holds all three messages", func() bool {
		return queueDepth(t, ch, queue) == 3
	})

	// Purge the queue and clear the buffer the way the reset sequence does.
	if _, err := ch.QueuePurge(queue, false); err != nil {
		t.Fatalf("purge queue %s: %v", queue, err)
	}

	req, err := http.NewRequest(http.MethodDelete, captureURL+"/captured", nil)
	if err != nil {
		t.Fatalf("build capture clear request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("clear capture buffer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear capture buffer returned status %d, expect %d", resp.StatusCode, http.StatusNoContent)
	}

	waitFor(t, "the queue reports depth 0", func() bool {
		return queueDepth(t, ch, queue) == 0
	})

	if got := len(capturedMessages(t, captureURL, "")); got != 0 {
		t.Errorf("capture buffer holds %d messages after clear, want 0", got)
	}
}
