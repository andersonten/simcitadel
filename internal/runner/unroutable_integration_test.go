//go:build integration

package runner

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	amqpdriver "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"github.com/testcontainers/testcontainers-go/wait"
)

// This file proves the unroutable check of SDD section 4.11 against a real
// broker. The shape of the management answer decides where the exchange and
// the routing key of a message live, and only a broker can settle that.
//
// Run it with:
//
//	go test -tags=integration ./internal/runner/

const (
	// rabbitImage matches the digest pinned in deploy/compose/docker-compose.yml.
	rabbitImage = "rabbitmq:4.3.4-management@sha256:eb5295d083325da5929a5ade766684d4019ffd2bce8bc7e43d6f9a05cafc8646"

	// unroutablePollInterval is how often the check runs again.
	unroutablePollInterval = 100 * time.Millisecond

	// unroutablePollTimeout bounds how long the message takes to arrive.
	unroutablePollTimeout = 30 * time.Second
)

// TestCheckUnroutableDepth_RealBroker_NamesTheExchangeAndRoutingKey publishes a
// message that no queue can accept, and holds the failure to SDD section 4.11:
// it names the exchange and the routing key of that message.
func TestCheckUnroutableDepth_RealBroker_NamesTheExchangeAndRoutingKey(t *testing.T) {
	ctx := context.Background()
	amqpURL, mgmtURL := startBroker(t)

	const (
		exchange   = "public.orchestration-engine.Article.v1"
		routingKey = "article.submitted"
	)

	client := &http.Client{Timeout: 10 * time.Second}
	applyAlternateExchangePolicy(ctx, t, client, mgmtURL)

	conn, err := amqpdriver.Dial(amqpURL)
	if err != nil {
		t.Fatalf("dial broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	// The unroutable exchange and its queue belong to Citadel, as SDD section
	// 7.3 declares them in definitions.json.
	if err := ch.ExchangeDeclare("citadel.unroutable.ae", "fanout", true, false, false, false, nil); err != nil {
		t.Fatalf("declare the alternate exchange: %v", err)
	}
	if _, err := ch.QueueDeclare("citadel.unroutable", true, false, false, false, nil); err != nil {
		t.Fatalf("declare the unroutable queue: %v", err)
	}
	if err := ch.QueueBind("citadel.unroutable", "", "citadel.unroutable.ae", false, nil); err != nil {
		t.Fatalf("bind the unroutable queue: %v", err)
	}

	// The application owns this exchange, and no queue binds to the routing key.
	if err := ch.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		t.Fatalf("declare the application exchange: %v", err)
	}

	// The policy applies asynchronously, so a message published before the
	// exchange carries it is dropped instead of routed to the alternate.
	waitForPolicy(ctx, t, client, mgmtURL, exchange)

	err = ch.PublishWithContext(ctx, exchange, routingKey, false, false, amqpdriver.Publishing{
		ContentType: "application/json",
		Body:        []byte(`{"specversion":"1.0"}`),
	})
	if err != nil {
		t.Fatalf("publish an unroutable message: %v", err)
	}

	var failure error
	deadline := time.Now().Add(unroutablePollTimeout)
	for time.Now().Before(deadline) {
		failure = checkUnroutableDepth(ctx, mgmtTarget(mgmtURL), client)
		if failure != nil {
			break
		}
		time.Sleep(unroutablePollInterval)
	}

	if failure == nil {
		t.Fatalf("the unroutable queue stayed empty after %s", unroutablePollTimeout)
	}
	t.Logf("failure text: %s", failure.Error())

	for _, want := range []string{exchange, routingKey} {
		if !strings.Contains(failure.Error(), want) {
			t.Errorf("the failure text does not name %q: %q", want, failure.Error())
		}
	}
	if got := ClassifyError(failure); got != ExitEnvironment {
		t.Errorf("exit = %d, want %d", got, ExitEnvironment)
	}

	// The read of SDD section 4.11 must leave the queue as it found it, so a
	// second check reports the same message.
	if second := checkUnroutableDepth(ctx, mgmtTarget(mgmtURL), client); second == nil {
		t.Error("the message read emptied the queue, so the read is destructive")
	}
}

// startBroker starts a RabbitMQ container and returns its AMQP URL and its
// management URL. The management URL carries the credentials, because every
// management call reads them from the URL.
func startBroker(t *testing.T) (string, string) {
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
		t.Fatalf("read the AMQP URL: %v", err)
	}
	httpURL, err := container.HttpURL(ctx)
	if err != nil {
		t.Fatalf("read the management URL: %v", err)
	}
	parsed, err := url.Parse(httpURL)
	if err != nil {
		t.Fatalf("parse the management URL: %v", err)
	}
	parsed.User = url.UserPassword(container.AdminUsername, container.AdminPassword)
	return amqpURL, parsed.String()
}

// applyAlternateExchangePolicy applies the policy of SDD section 7.4, which
// attaches the alternate exchange to every application exchange.
func applyAlternateExchangePolicy(ctx context.Context, t *testing.T, client *http.Client, mgmtURL string) {
	t.Helper()

	body := []byte(`{"pattern":"^(public|private)\\.","apply-to":"exchanges","priority":0,` +
		`"definition":{"alternate-exchange":"citadel.unroutable.ae"}}`)
	target := strings.TrimRight(mgmtURL, "/") + "/api/policies/%2F/citadel-alternate-exchange"

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build the policy request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("apply the alternate exchange policy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("apply the alternate exchange policy: status %d", resp.StatusCode)
	}
}

// waitForPolicy blocks until the management API reports that the exchange
// carries the alternate exchange policy.
func waitForPolicy(ctx context.Context, t *testing.T, client *http.Client, mgmtURL, exchange string) {
	t.Helper()

	target := fmt.Sprintf("%s/api/exchanges/%%2F/%s", strings.TrimRight(mgmtURL, "/"), url.PathEscape(exchange))
	deadline := time.Now().Add(unroutablePollTimeout)

	for time.Now().Before(deadline) {
		var state struct {
			Policy string `json:"policy"`
		}
		if _, err := readCheckJSON(ctx, client, "policy", http.MethodGet, target, nil, &state); err == nil {
			if state.Policy == "citadel-alternate-exchange" {
				return
			}
		}
		time.Sleep(unroutablePollInterval)
	}
	t.Fatalf("exchange %s does not carry the alternate exchange policy after %s", exchange, unroutablePollTimeout)
}
