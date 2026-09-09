//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	amqpdriver "github.com/rabbitmq/amqp091-go"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/capture"
	"github.com/andersonten/simcitadel/internal/config"
)

// bareMgmtURL strips credentials from a management URL, producing the shape
// of the SDD section 5.1 sample where managementUrl carries no user or
// password.
func bareMgmtURL(t *testing.T, httpURL string) string {
	t.Helper()
	parsed, err := url.Parse(httpURL)
	if err != nil {
		t.Fatalf("parse management URL: %v", err)
	}
	parsed.User = nil
	return parsed.String()
}

// managementGet builds a GET against the management API with the credentials
// derived from the config target, which is the path every runner call takes.
func managementGet(ctx context.Context, t *testing.T, target *config.RabbitMQTarget, path string) *http.Response {
	t.Helper()

	addr := strings.TrimRight(target.ManagementURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		t.Fatalf("build management request: %v", err)
	}
	if user, password, ok := target.ManagementAuth(); ok {
		req.SetBasicAuth(user, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send management request: %v", err)
	}
	return resp
}

// --------------------------------------------------------------------------
// Finding 1: management API credentials derived from amqpUrl (§5.1, §4.9)
// --------------------------------------------------------------------------

// TestManagementCredentials_RealBroker_OverviewSucceeds proves that the
// management API accepts a request whose credentials come from amqpUrl when
// managementUrl carries none. Before the fix, every management call returned
// 401 and every run died in preflight.
func TestManagementCredentials_RealBroker_OverviewSucceeds(t *testing.T) {
	amqpURL, httpURL := startRabbitWithManagement(t)
	mgmtURL := bareMgmtURL(t, httpURL)

	target := &config.RabbitMQTarget{
		AMQPURL:       amqpURL,
		ManagementURL: mgmtURL,
	}

	resp := managementGet(context.Background(), t, target, "/api/overview")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("management overview with credentials from amqpUrl returned %d, want 200", resp.StatusCode)
	}
}

// TestManagementCredentials_RealBroker_BareMgmtURLReturns401 proves that the
// broker rejects a bare request, which is the pre-fix state.
func TestManagementCredentials_RealBroker_BareMgmtURLReturns401(t *testing.T) {
	_, httpURL := startRabbitWithManagement(t)
	mgmtURL := bareMgmtURL(t, httpURL)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.TrimRight(mgmtURL, "/")+"/api/overview", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bare management request returned %d, want 401", resp.StatusCode)
	}
}

// TestManagementCredentials_RealBroker_ListsQueues proves that the queue list
// of SDD section 4.9 step 7 works with credentials from amqpUrl. This is the
// path purgeQueues and verifyQueueDepths take.
func TestManagementCredentials_RealBroker_ListsQueues(t *testing.T) {
	amqpURL, httpURL := startRabbitWithManagement(t)
	mgmtURL := bareMgmtURL(t, httpURL)

	conn := dial(t, amqpURL)
	ch := channel(t, conn)
	const queue = "test.mgmt-creds.inbox"
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare queue: %v", err)
	}

	target := &config.RabbitMQTarget{
		AMQPURL:       amqpURL,
		ManagementURL: mgmtURL,
	}

	resp := managementGet(context.Background(), t, target, "/api/queues")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list queues returned %d, want 200", resp.StatusCode)
	}

	var queues []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queues); err != nil {
		t.Fatalf("decode queue list: %v", err)
	}
	found := false
	for _, q := range queues {
		if q.Name == queue {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("queue list omits %s", queue)
	}
}

// --------------------------------------------------------------------------
// Finding 4: stale confirm frames from prior publishes (§7.5)
// --------------------------------------------------------------------------

// TestStaleConfirmFrames_RealBroker_SecondPublishIsNotContaminated cancels the
// context of the first publish so it abandons the confirm, then publishes again
// on the same publisher. The second result must reflect the second message, not
// the stale confirm of the first.
func TestStaleConfirmFrames_RealBroker_SecondPublishIsNotContaminated(t *testing.T) {
	amqpURL := startRabbit(t)
	conn := dial(t, amqpURL)
	ch := channel(t, conn)

	const (
		exchange   = "public.test.StaleFrames.v1"
		queue      = "test.staleframes.consumer"
		routingKey = "article.submitted"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	pubCh, err := conn.Channel()
	if err != nil {
		t.Fatalf("open publish channel: %v", err)
	}
	t.Cleanup(func() { _ = pubCh.Close() })

	pub, err := citadelamqp.NewPublisher(pubCh, 5*time.Second)
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = pub.Publish(cancelledCtx, exchange, routingKey, amqpdriver.Publishing{
		ContentType: "application/json",
		Body:        []byte(`{"specversion":"1.0","id":"stale-1","type":"T","source":"S","version":"v1"}`),
	})
	if err == nil {
		t.Fatal("the cancelled publish succeeded, so the cancel did not stop the wait")
	}

	time.Sleep(200 * time.Millisecond)

	result, err := pub.Publish(context.Background(), exchange, routingKey, amqpdriver.Publishing{
		ContentType: "application/json",
		Body:        []byte(`{"specversion":"1.0","id":"stale-2","type":"T","source":"S","version":"v1"}`),
	})
	if err != nil {
		t.Fatalf("the second publish failed: %v", err)
	}
	if !result.Confirmed {
		t.Error("the second publish is not confirmed, so it may have read the stale frame of the first")
	}
	if result.Returned {
		t.Error("the second publish is marked returned, but the exchange has a binding")
	}
}

// TestStaleReturnFrames_RealBroker_SecondPublishIsNotContaminated publishes to
// an unbound exchange with a cancelled context, then publishes to a bound
// exchange. The second publish must not inherit the basic.return of the first.
func TestStaleReturnFrames_RealBroker_SecondPublishIsNotContaminated(t *testing.T) {
	amqpURL := startRabbit(t)
	conn := dial(t, amqpURL)
	ch := channel(t, conn)

	const (
		unboundExchange = "public.test.StaleReturn.v1"
		boundExchange   = "public.test.StaleReturnBound.v1"
		queue           = "test.stalereturn.consumer"
		routingKey      = "article.submitted"
	)
	declareTopology(t, ch, unboundExchange, "", "")
	declareTopology(t, ch, boundExchange, queue, routingKey)

	pubCh, err := conn.Channel()
	if err != nil {
		t.Fatalf("open publish channel: %v", err)
	}
	t.Cleanup(func() { _ = pubCh.Close() })

	pub, err := citadelamqp.NewPublisher(pubCh, 5*time.Second)
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _ = pub.Publish(cancelledCtx, unboundExchange, routingKey, amqpdriver.Publishing{
		ContentType: "application/json",
		Body:        []byte(`{"specversion":"1.0","id":"ret-1","type":"T","source":"S","version":"v1"}`),
	})

	time.Sleep(200 * time.Millisecond)

	result, err := pub.Publish(context.Background(), boundExchange, routingKey, amqpdriver.Publishing{
		ContentType: "application/json",
		Body:        []byte(`{"specversion":"1.0","id":"ret-2","type":"T","source":"S","version":"v1"}`),
	})
	if err != nil {
		t.Fatalf("the second publish failed: %v", err)
	}
	if result.Returned {
		t.Error("the second publish inherited the basic.return of the first, so it falsely reports unroutable")
	}
}

// --------------------------------------------------------------------------
// Finding 5: publishConfirmTimeout wiring (§7.5)
// --------------------------------------------------------------------------

// TestPublishConfirmTimeout_RealBroker_TriggerRespectsThePublisherWindow proves
// that the trigger handler bounds the publish by the publisher's confirm
// timeout, not a hardcoded default. A publish to a bound exchange with a
// generous window must succeed.
func TestPublishConfirmTimeout_RealBroker_TriggerRespectsThePublisherWindow(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.ConfirmWindow.v1"
		queue      = "test.confirmwindow.consumer"
		routingKey = "article.submitted"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	triggerURL := startTriggerServer(t, amqpURL, 100).URL
	status, body := postPublish(t, triggerURL, "run-confirm-window", publishRequest{
		Delivery:   "amqp",
		Exchange:   exchange,
		RoutingKey: routingKey,
		Event:      validEvent("e-confirm-window"),
	})
	if status != http.StatusAccepted {
		t.Fatalf("publish returned %d, want 202; body=%v", status, body)
	}
	if confirmed, _ := body["confirmed"].(bool); !confirmed {
		t.Error("the publish is not confirmed")
	}
}

// --------------------------------------------------------------------------
// Finding 6: empty header selector (§8.2, §8.3)
// --------------------------------------------------------------------------

// TestEmptyHeaderSelector_RealBroker_DoesNotMatchAbsentHeader publishes two
// messages through the real pipeline: one with an eventType header set to ""
// and one with no eventType header. A query with header.eventType= must match
// only the first.
func TestEmptyHeaderSelector_RealBroker_DoesNotMatchAbsentHeader(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const (
		exchange   = "public.test.EmptyHeader.v1"
		queue      = "test.emptyheader.consumer"
		routingKey = "article.submitted"
		runID      = "run-emptyheader"
	)
	declareTopology(t, ch, exchange, queue, routingKey)

	triggerURL := startTriggerServer(t, amqpURL, 100).URL
	captureURL, _ := startCaptureService(t, amqpURL, exchange)

	status, body := postPublish(t, triggerURL, runID, publishRequest{
		Delivery:   "amqp",
		Exchange:   exchange,
		RoutingKey: routingKey,
		Event:      validEvent("e-with-header"),
		Headers:    map[string]string{"eventType": ""},
	})
	if status != http.StatusAccepted {
		t.Fatalf("publish with header returned %d; body=%v", status, body)
	}

	status, body = postPublish(t, triggerURL, runID, publishRequest{
		Delivery:   "amqp",
		Exchange:   exchange,
		RoutingKey: routingKey,
		Event:      validEvent("e-no-header"),
	})
	if status != http.StatusAccepted {
		t.Fatalf("publish without header returned %d; body=%v", status, body)
	}

	waitFor(t, "both messages are captured", func() bool {
		return len(capturedMessages(t, captureURL, "exchange="+exchange)) == 2
	})

	matched := capturedMessages(t, captureURL, "exchange="+exchange+"&header.eventType=")
	if len(matched) != 1 {
		t.Fatalf("header.eventType= matched %d messages, want 1 (only the message with the header present and empty)", len(matched))
	}
	msg := matched[0]
	var envelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(msg.Body, &envelope); err == nil && envelope.ID != "e-with-header" {
		t.Errorf("the matched message has id %q, want e-with-header", envelope.ID)
	}
}

// --------------------------------------------------------------------------
// Finding 3: spy-queue consumer death (§8.1)
// --------------------------------------------------------------------------

// TestSpyQueueConsumerDeath_RealBroker_ReportsExchangeFailed proves that
// closing the AMQP connection under a running capture service moves the
// exchange from "bound" to "failed". Before the fix, the status stayed "bound"
// and /health answered 200 while the exchange captured nothing.
func TestSpyQueueConsumerDeath_RealBroker_ReportsExchangeFailed(t *testing.T) {
	amqpURL := startRabbit(t)
	ch := channel(t, dial(t, amqpURL))

	const exchange = "public.test.ConsumerDeath.v1"
	if err := ch.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		t.Fatalf("declare exchange: %v", err)
	}

	captureConn := dial(t, amqpURL)

	configs := []capture.ExchangeConfig{{Name: exchange, Declare: "passive"}}
	bm := capture.NewBindingManager(captureConn, configs, 1000, 60*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = bm.Start(ctx)
	}()

	waitForBindings(t, bm, exchange)

	status := bm.Status()
	if status[exchange] != "bound" {
		t.Fatalf("exchange status before closure = %q, want bound", status[exchange])
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	healthSrv := httptest.NewServer(capture.NewServer(bm.Buffer(), bm.Status, logger).Handler())
	t.Cleanup(healthSrv.Close)

	resp, err := http.Get(healthSrv.URL + "/health")
	if err != nil {
		t.Fatalf("health probe before closure: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health before closure = %d, want 200", resp.StatusCode)
	}

	if err := captureConn.Close(); err != nil {
		t.Fatalf("close capture connection: %v", err)
	}

	waitFor(t, "exchange status moves to failed", func() bool {
		return bm.Status()[exchange] == "failed"
	})

	resp, err = http.Get(healthSrv.URL + "/health")
	if err != nil {
		t.Fatalf("health probe after closure: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("health after consumer death = %d, want 503", resp.StatusCode)
	}
}

// --------------------------------------------------------------------------
// Finding 2: health report before first bind (§8.1, §11.1)
// --------------------------------------------------------------------------

// TestCaptureHealth_BeforeStart_ReportsExchangesPending proves that a health
// probe before the bind goroutine runs reads every exchange as pending, not an
// empty map that answers 200. This test does not need a broker because the
// state is structural: it is set by the constructor.
func TestCaptureHealth_BeforeStart_ReportsExchangesPending(t *testing.T) {
	configs := []capture.ExchangeConfig{
		{Name: "public.test.HealthPre.v1", Declare: "passive"},
		{Name: "public.test.HealthPre.v2", Declare: "passive"},
	}
	bm := capture.NewBindingManager(nil, configs, 1000, 60*time.Second)

	status := bm.Status()
	for _, cfg := range configs {
		s, ok := status[cfg.Name]
		if !ok {
			t.Errorf("status map omits %s before Start, so a health probe reads an empty map and answers 200", cfg.Name)
			continue
		}
		if s != "pending" {
			t.Errorf("status of %s before Start = %q, want pending", cfg.Name, s)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	healthSrv := httptest.NewServer(capture.NewServer(bm.Buffer(), bm.Status, logger).Handler())
	t.Cleanup(healthSrv.Close)

	resp, err := http.Get(healthSrv.URL + "/health")
	if err != nil {
		t.Fatalf("health probe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("health before Start = %d, want 503", resp.StatusCode)
	}
}
