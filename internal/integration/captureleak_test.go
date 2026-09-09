//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"github.com/testcontainers/testcontainers-go/wait"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/capture"
)

// startRabbitWithManagement starts a broker and returns its AMQP URL and the
// root of its management API. The management API reports the open channels, so
// a test can hold the capture service to the channels it actually needs.
func startRabbitWithManagement(t *testing.T) (amqpURL, httpURL string) {
	t.Helper()
	ctx := context.Background()

	container, err := tcrabbitmq.Run(ctx, rabbitImage,
		testcontainers.WithWaitStrategy(
			wait.ForLog("Server startup complete").WithStartupTimeout(3*time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start RabbitMQ container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate RabbitMQ container: %v", err)
		}
	})

	amqpURL, err = container.AmqpURL(ctx)
	if err != nil {
		t.Fatalf("read AMQP URL: %v", err)
	}
	httpURL, err = container.HttpURL(ctx)
	if err != nil {
		t.Fatalf("read management URL: %v", err)
	}
	return amqpURL, httpURL
}

// brokerChannelCount returns how many AMQP channels the broker holds open.
func brokerChannelCount(t *testing.T, httpURL string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, httpURL+"/api/channels", nil)
	if err != nil {
		t.Fatalf("build channel query: %v", err)
	}
	req.SetBasicAuth("guest", "guest")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("query channels: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("channel query returned status %d, expect 200", resp.StatusCode)
	}

	var channels []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&channels); err != nil {
		t.Fatalf("decode channels: %v", err)
	}
	return len(channels)
}

// requireChannelCount polls until the broker reports want channels. The
// management API refreshes its statistics on an interval, so a count is read
// until it settles rather than once. The failure names the observed count.
func requireChannelCount(t *testing.T, httpURL string, want int, context string) {
	t.Helper()

	var got int
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		got = brokerChannelCount(t, httpURL)
		if got == want {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("%s: the broker holds %d channels, want %d (difference of %d)",
		context, got, want, got-want)
}

// startCaptureOn starts a binding manager on its own connection and waits until
// every exchange is bound.
func startCaptureOn(t *testing.T, amqpURL string, configs []capture.ExchangeConfig, bindTimeout time.Duration) *capture.BindingManager {
	t.Helper()

	conn, err := citadelamqp.Dial(context.Background(), amqpURL)
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	bm := capture.NewBindingManager(conn, configs, 100, bindTimeout)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := bm.Start(ctx); err != nil && ctx.Err() == nil {
			t.Logf("binding manager stopped: %v", err)
		}
	}()
	return bm
}

// TestCaptureBinding_PassiveExchanges_HoldOneChannelEach proves the capture
// service does not leak a channel per passive exchange. A spy queue needs one
// channel to consume from. A channel opened only to declare an exchange must
// close, because the broker enforces a channel_max and a leak per exchange
// exhausts a long-lived capture service.
func TestCaptureBinding_PassiveExchanges_HoldOneChannelEach(t *testing.T) {
	amqpURL, httpURL := startRabbitWithManagement(t)

	exchanges := []string{"public.leak.a.v1", "public.leak.b.v1", "public.leak.c.v1"}

	// A passive declare succeeds only after the exchange exists. The setup
	// connection closes again, so the measurement starts from zero channels.
	setupConn, err := citadelamqp.Dial(context.Background(), amqpURL)
	if err != nil {
		t.Fatalf("dial broker for setup: %v", err)
	}
	setupCh, err := setupConn.Channel()
	if err != nil {
		t.Fatalf("open setup channel: %v", err)
	}
	for _, name := range exchanges {
		if err := setupCh.ExchangeDeclare(name, "topic", true, false, false, false, nil); err != nil {
			t.Fatalf("declare exchange %s: %v", name, err)
		}
	}
	if err := setupConn.Close(); err != nil {
		t.Fatalf("close setup connection: %v", err)
	}
	requireChannelCount(t, httpURL, 0, "after the setup connection closes")

	configs := make([]capture.ExchangeConfig, 0, len(exchanges))
	for _, name := range exchanges {
		configs = append(configs, capture.ExchangeConfig{Name: name, Declare: "passive"})
	}

	bm := startCaptureOn(t, amqpURL, configs, 30*time.Second)
	waitForBindings(t, bm, exchanges...)

	requireChannelCount(t, httpURL, len(exchanges),
		"with one consumer per passive exchange and no declare channel left open")
}

// TestCaptureBinding_OwnExchanges_HoldOneChannelEach covers the declare mode
// that SDD section 8.1 makes the default.
func TestCaptureBinding_OwnExchanges_HoldOneChannelEach(t *testing.T) {
	amqpURL, httpURL := startRabbitWithManagement(t)

	exchanges := []string{"public.own.a.v1", "public.own.b.v1"}
	requireChannelCount(t, httpURL, 0, "on a fresh broker")

	configs := make([]capture.ExchangeConfig, 0, len(exchanges))
	for _, name := range exchanges {
		configs = append(configs, capture.ExchangeConfig{Name: name})
	}

	bm := startCaptureOn(t, amqpURL, configs, 30*time.Second)
	waitForBindings(t, bm, exchanges...)

	requireChannelCount(t, httpURL, len(exchanges), "with one consumer per own exchange")
}

// TestCaptureBinding_ExchangeNeverAppears_LeavesNoChannelBehind proves the
// retry loop of SDD section 8.1 accumulates no channel. The exchange never
// appears, so the loop runs to its bind timeout, and every channel it opened
// must be gone.
func TestCaptureBinding_ExchangeNeverAppears_LeavesNoChannelBehind(t *testing.T) {
	amqpURL, httpURL := startRabbitWithManagement(t)
	requireChannelCount(t, httpURL, 0, "on a fresh broker")

	conn, err := citadelamqp.Dial(context.Background(), amqpURL)
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// The bind timeout spans several attempts at the fixed 2s retry interval.
	bm := capture.NewBindingManager(conn,
		[]capture.ExchangeConfig{{Name: "public.absent.v1", Declare: "passive"}},
		100, 7*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bm.Start(ctx)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the binding manager did not give up within 30s")
	}

	if status := bm.Status()["public.absent.v1"]; status != "failed" {
		t.Fatalf("exchange status is %q, want failed", status)
	}

	requireChannelCount(t, httpURL, 0, "after the retry loop gives up")
}
