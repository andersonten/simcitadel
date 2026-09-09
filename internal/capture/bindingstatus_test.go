package capture

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// SDD section 8.1 gives one exchange three states: bound, pending and failed.
// Preflight of section 4.10 treats anything but bound as not ready, "so a
// scenario cannot start while a spy queue is unbound -- which would otherwise
// produce a scenario that fails an outbound assertion for reasons unrelated to
// the app".
//
// The two tests below cover the two ways the report said bound or said nothing
// while the service captured nothing.

// TestBindingManagerStatus_BeforeStart_ReportsPendingForEveryExchange holds the
// state between the constructor and the first bind. The HTTP listener opens
// before Start runs, so a health probe that arrives in that window read an empty
// map, and an empty map is the one state the health endpoint answers 200 for.
func TestBindingManagerStatus_BeforeStart_ReportsPendingForEveryExchange(t *testing.T) {
	exchanges := []ExchangeConfig{
		{Name: "public.orchestration-engine.Manuscript.v1", Declare: "own"},
		{Name: "public.orchestration-engine.Task.v1", Declare: "passive"},
	}

	bm := NewBindingManager(nil, exchanges, 10, time.Second)

	status := bm.Status()
	for _, ec := range exchanges {
		got, ok := status[ec.Name]
		if !ok {
			t.Errorf("the status report omits %s before Start, so the health endpoint reports the service ready", ec.Name)
			continue
		}
		if got != exchangeStatePending {
			t.Errorf("the status of %s is %q before Start, want %q", ec.Name, got, exchangeStatePending)
		}
	}
}

// TestHealth_BeforeStart_AnswersServiceUnavailable is the consequence of the
// test above, at the endpoint a container health check and preflight both read.
func TestHealth_BeforeStart_AnswersServiceUnavailable(t *testing.T) {
	bm := NewBindingManager(nil, []ExchangeConfig{
		{Name: "public.orchestration-engine.Manuscript.v1", Declare: "own"},
	}, 10, time.Second)

	srv := NewServer(bm.Buffer(), bm.Status, slog.Default())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("health answers %d before any exchange is bound, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestConsumeMessages_DeliveryChannelCloses_ReportsTheExchangeFailed covers the
// broker taking the consumer away. The delivery channel closes when the channel
// or the connection dies, and nothing re-establishes it, so the service captures
// nothing from that moment on. The status stayed bound, so preflight passed and
// every event/not-captured assertion of section 8.3 passed on an empty buffer.
func TestConsumeMessages_DeliveryChannelCloses_ReportsTheExchangeFailed(t *testing.T) {
	const exchange = "public.orchestration-engine.Manuscript.v1"

	bm := NewBindingManager(nil, []ExchangeConfig{{Name: exchange, Declare: "own"}}, 10, time.Second)
	bm.setStatus(exchange, exchangeStateBound)

	deliveries := make(chan amqpdriver.Delivery)
	done := make(chan struct{})
	go func() {
		defer close(done)
		bm.consumeMessages(context.Background(), exchange, deliveries)
	}()

	close(deliveries)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumeMessages did not return after the delivery channel closed")
	}

	if got := bm.Status()[exchange]; got != exchangeStateFailed {
		t.Errorf("the status of %s is %q after the consumer died, want %q", exchange, got, exchangeStateFailed)
	}

	srv := NewServer(bm.Buffer(), bm.Status, slog.Default())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("health answers %d for a dead consumer, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body struct {
		Exchanges map[string]string `json:"exchanges"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode the health response: %v", err)
	}
	if body.Exchanges[exchange] != exchangeStateFailed {
		t.Errorf("the health body reports %s as %q, want %q", exchange, body.Exchanges[exchange], exchangeStateFailed)
	}
}

// TestFilterMatch_HeaderSelectorWithAnEmptyValue_RejectsAMessageWithoutTheHeader
// covers the header filter of SDD section 8.2. A header value of an empty string
// is a value a message can carry: section 10.1 sets DEBEZIUM_FORMAT_HEADER=json,
// so the two bytes of an empty JSON string decode to an empty value.
//
// The filter compared the value alone, and a map read of an absent key gives the
// same empty string, so header.eventType= matched every message in the buffer,
// including one that carries no header at all. A selector that matches
// everything makes countAtLeast pass on any message, which is a pass a scenario
// did not earn.
func TestFilterMatch_HeaderSelectorWithAnEmptyValue_RejectsAMessageWithoutTheHeader(t *testing.T) {
	f := Filter{Headers: map[string]string{"eventType": ""}}

	noHeaders := Message{Exchange: "public.orchestration-engine.Task.v1", Body: json.RawMessage(`{}`)}
	if f.Match(noHeaders) {
		t.Error("a message that carries no eventType header matched the selector header.eventType=")
	}

	otherHeader := Message{
		Exchange: "public.orchestration-engine.Task.v1",
		Headers:  map[string]string{"aggregateId": "515150"},
		Body:     json.RawMessage(`{}`),
	}
	if f.Match(otherHeader) {
		t.Error("a message that carries another header matched the selector header.eventType=")
	}

	emptyValue := Message{
		Exchange: "public.orchestration-engine.Task.v1",
		Headers:  map[string]string{"eventType": ""},
		Body:     json.RawMessage(`{}`),
	}
	if !f.Match(emptyValue) {
		t.Error("a message that carries eventType with an empty value did not match the selector header.eventType=")
	}
}

// TestConsumeMessages_ContextCancelled_LeavesTheStatusAlone draws the line of
// the test above. A shutdown is not a binding failure.
func TestConsumeMessages_ContextCancelled_LeavesTheStatusAlone(t *testing.T) {
	const exchange = "public.orchestration-engine.Task.v1"

	bm := NewBindingManager(nil, []ExchangeConfig{{Name: exchange, Declare: "own"}}, 10, time.Second)
	bm.setStatus(exchange, exchangeStateBound)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bm.consumeMessages(ctx, exchange, make(chan amqpdriver.Delivery))
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumeMessages did not return after the context was cancelled")
	}

	if got := bm.Status()[exchange]; got != exchangeStateBound {
		t.Errorf("a shutdown changed the status of %s to %q, want %q", exchange, got, exchangeStateBound)
	}
}
