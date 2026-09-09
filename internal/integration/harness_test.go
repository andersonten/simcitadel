//go:build integration

// Package integration holds the container-backed tests of SDD section 14.2.
//
// Run them with:
//
//	go test -tags=integration ./internal/integration/...
//
// Each test starts the containers it needs, so the suite does not depend on a
// shared environment. A test polls for a condition and never sleeps for a
// fixed period.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	amqpdriver "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"github.com/testcontainers/testcontainers-go/wait"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/capture"
	"github.com/andersonten/simcitadel/internal/trigger"
)

const (
	// rabbitImage matches the digest pinned in deploy/compose/docker-compose.yml.
	rabbitImage = "rabbitmq:4.3.4-management@sha256:eb5295d083325da5929a5ade766684d4019ffd2bce8bc7e43d6f9a05cafc8646"

	// pollInterval is how often a condition is re-checked.
	pollInterval = 50 * time.Millisecond

	// pollTimeout bounds how long a condition may take to become true.
	pollTimeout = 20 * time.Second
)

// startRabbit starts a RabbitMQ container and returns its AMQP URL.
func startRabbit(t *testing.T) string {
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

	amqpURL, err := container.AmqpURL(ctx)
	if err != nil {
		t.Fatalf("read AMQP URL: %v", err)
	}
	return amqpURL
}

// dial opens a connection that closes when the test ends.
func dial(t *testing.T, amqpURL string) *citadelamqp.Conn {
	t.Helper()
	conn, err := citadelamqp.Dial(context.Background(), amqpURL)
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// channel opens a channel that closes when the test ends.
func channel(t *testing.T, conn *citadelamqp.Conn) *amqpdriver.Channel {
	t.Helper()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	return ch
}

// declareTopology declares a topic exchange, and binds a queue to it when
// routingKey is not empty. An exchange with no binding is what an unroutable
// publish needs.
func declareTopology(t *testing.T, ch *amqpdriver.Channel, exchange, queue, routingKey string) {
	t.Helper()

	if err := ch.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		t.Fatalf("declare exchange %s: %v", exchange, err)
	}
	if queue == "" {
		return
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare queue %s: %v", queue, err)
	}
	if err := ch.QueueBind(queue, routingKey, exchange, false, nil); err != nil {
		t.Fatalf("bind queue %s to %s: %v", queue, exchange, err)
	}
}

// startTriggerServer starts an in-process event trigger against the broker.
func startTriggerServer(t *testing.T, amqpURL string, budget int) *httptest.Server {
	t.Helper()

	conn := dial(t, amqpURL)
	ch := channel(t, conn)

	pub, err := citadelamqp.NewPublisher(ch, 5*time.Second)
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}

	srv := trigger.NewServer(pub, trigger.NewBudget(budget), func() bool { return !conn.IsClosed() })
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return httpSrv
}

// startCaptureService starts an in-process event capture bound to the given
// exchanges, and waits until every binding is established.
// startCaptureService returns the base URL of the query API.
func startCaptureService(t *testing.T, amqpURL string, exchanges ...string) (string, *capture.BindingManager) {
	t.Helper()

	conn := dial(t, amqpURL)

	configs := make([]capture.ExchangeConfig, 0, len(exchanges))
	for _, name := range exchanges {
		configs = append(configs, capture.ExchangeConfig{Name: name, Declare: "passive"})
	}

	bm := capture.NewBindingManager(conn, configs, 1000, 60*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := bm.Start(ctx); err != nil && ctx.Err() == nil {
			t.Logf("binding manager stopped: %v", err)
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpSrv := httptest.NewServer(capture.NewServer(bm.Buffer(), bm.Status, logger).Handler())
	t.Cleanup(httpSrv.Close)

	waitForBindings(t, bm, exchanges...)
	return httpSrv.URL, bm
}

// waitForBindings blocks until every exchange reports a bound state.
func waitForBindings(t *testing.T, bm *capture.BindingManager, exchanges ...string) {
	t.Helper()

	waitFor(t, "every capture binding is established", func() bool {
		status := bm.Status()
		for _, name := range exchanges {
			if status[name] != "bound" {
				return false
			}
		}
		return true
	})
}

// waitFor polls condition until it returns true, and fails the test on timeout.
// A test never sleeps for a fixed period, per CONTRIBUTING.md.
func waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timeout after %s waiting until %s", pollTimeout, description)
}

// publishRequest is the body the event trigger accepts on POST /publish.
type publishRequest struct {
	Delivery   string            `json:"delivery"`
	Exchange   string            `json:"exchange"`
	RoutingKey string            `json:"routingKey"`
	Event      map[string]any    `json:"event"`
	Headers    map[string]string `json:"headers"`
}

// validEvent returns a CloudEvents envelope that passes trigger validation.
func validEvent(id string) map[string]any {
	return map[string]any{
		"specversion": "1.0",
		"id":          id,
		"type":        "ArticleSubmitted",
		"source":      "SRV-TEST",
		"version":     "v1",
		"subject":     "/articles/" + id,
		"data":        map[string]any{"articleId": id},
	}
}

// postPublish sends one publish request and returns the status and body.
func postPublish(t *testing.T, triggerURL, runID string, body publishRequest) (int, map[string]any) {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal publish request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, triggerURL+"/publish", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("build publish request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if runID != "" {
		req.Header.Set("X-Citadel-Run", runID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send publish request: %v", err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

// capturedMessages queries the capture API with the given query string.
func capturedMessages(t *testing.T, captureURL, query string) []capture.Message {
	t.Helper()

	url := captureURL + "/captured"
	if query != "" {
		url += "?" + query
	}

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("query captured messages: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("captured query returned status %d, expect 200", resp.StatusCode)
	}

	var messages []capture.Message
	if err := json.NewDecoder(resp.Body).Decode(&messages); err != nil {
		t.Fatalf("decode captured messages: %v", err)
	}
	return messages
}

// queueDepth returns the current message count of a queue.
func queueDepth(t *testing.T, ch *amqpdriver.Channel, queue string) int {
	t.Helper()

	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		t.Fatalf("inspect queue %s: %v", queue, err)
	}
	return q.Messages
}
