package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/andersonten/simcitadel/internal/amqp"

	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// The states the Status map reports for one exchange. The health endpoint reads
// them to decide the status code, so they are constants and not bare strings in
// two files.
const (
	exchangeStatePending = "pending"
	exchangeStateBound   = "bound"
	exchangeStateFailed  = "failed"
)

// ExchangeConfig describes one exchange to capture from.
type ExchangeConfig struct {
	Name    string
	Declare string // "own" or "passive"
}

// BindingManager handles spy queue binding and message consumption.
type BindingManager struct {
	conn        *amqp.Conn
	exchanges   []ExchangeConfig
	buf         *Buffer
	bindTimeout time.Duration
	logger      *slog.Logger

	mu       sync.RWMutex
	statuses map[string]string
}

// NewBindingManager creates a new binding manager. Every configured exchange
// starts as pending, because the HTTP listener opens before Start binds
// anything. A status report that holds no exchange is the report of a service
// with nothing to bind, and that is the one state the health endpoint of SDD
// section 8.1 answers 200 for.
func NewBindingManager(conn *amqp.Conn, exchanges []ExchangeConfig, bufferSize int, bindTimeout time.Duration) *BindingManager {
	statuses := make(map[string]string, len(exchanges))
	for _, ec := range exchanges {
		statuses[ec.Name] = exchangeStatePending
	}
	return &BindingManager{
		conn:        conn,
		exchanges:   exchanges,
		buf:         NewBuffer(bufferSize),
		bindTimeout: bindTimeout,
		logger:      slog.Default(),
		statuses:    statuses,
	}
}

// Status returns the binding status of each exchange.
func (bm *BindingManager) Status() map[string]string {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	result := make(map[string]string, len(bm.statuses))
	for k, v := range bm.statuses {
		result[k] = v
	}
	return result
}

// Buffer returns the capture buffer for query access.
func (bm *BindingManager) Buffer() *Buffer {
	return bm.buf
}

func (bm *BindingManager) setStatus(exchange, status string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.statuses[exchange] = status
}

// Start begins binding spy queues and consuming messages.
// For exchanges with declare="own", it declares the exchange as type=topic, durable=true.
// For exchanges with declare="passive", it retries the passive declare every 2s
// until success or the bind timeout, logging once per 10 attempts.
// Start reports exchange status through the Status method.
func (bm *BindingManager) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := range bm.exchanges {
		if bm.exchanges[i].Declare == "" {
			bm.exchanges[i].Declare = "own"
		}
		bm.setStatus(bm.exchanges[i].Name, exchangeStatePending)
		wg.Add(1)
		go func(ec ExchangeConfig) {
			defer wg.Done()
			if err := bm.bindExchange(ctx, ec); err != nil {
				bm.setStatus(ec.Name, exchangeStateFailed)
				bm.logger.Error("bind exchange failed", "exchange", ec.Name, "error", err)
			}
		}(bm.exchanges[i])
	}
	wg.Wait()
	return nil
}

// bindExchange declares the exchange, binds its spy queue, and starts a
// consumer. The consumer keeps the channel, so the channel closes only when
// bindExchange fails. The broker enforces a channel_max, so an abandoned
// channel is a resource the service never gets back.
func (bm *BindingManager) bindExchange(ctx context.Context, ec ExchangeConfig) error {
	ch, err := bm.conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel for %s: %w", ec.Name, err)
	}

	// bound stays false until the consumer owns the channel.
	bound := false
	defer func() {
		if !bound {
			_ = ch.Close()
		}
	}()

	if ec.Declare == "own" {
		err = ch.ExchangeDeclare(ec.Name, "topic", true, false, false, false, nil)
		if err != nil {
			return fmt.Errorf("declare exchange %s: %w", ec.Name, err)
		}
	} else {
		if err := bm.passiveDeclareWithRetry(ctx, ec.Name); err != nil {
			return err
		}
	}

	queueName := amqp.SpyQueueName(ec.Name)
	args := amqpdriver.Table{
		"x-max-length": int64(1000),
		"x-overflow":   "drop-head",
	}

	_, err = ch.QueueDeclare(queueName, true, false, false, false, args)
	if err != nil {
		return fmt.Errorf("declare spy queue %s: %w", queueName, err)
	}

	err = ch.QueueBind(queueName, "#", ec.Name, false, nil)
	if err != nil {
		return fmt.Errorf("bind spy queue %s to %s: %w", queueName, ec.Name, err)
	}

	deliveries, err := ch.Consume(queueName, "", true, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("start consumer on %s: %w", queueName, err)
	}

	bound = true
	bm.setStatus(ec.Name, exchangeStateBound)
	bm.logger.Info("exchange bound", "exchange", ec.Name, "queue", queueName)

	go bm.consumeMessages(ctx, ec.Name, deliveries)
	return nil
}

// passiveDeclareWithRetry waits for an exchange another team owns, as SDD
// section 8.1 requires for declare mode passive. Each attempt takes a
// throwaway channel, because the broker closes the channel when the exchange
// is absent, and a poisoned channel cannot carry the queue declare that
// follows. Every attempt closes its channel, so a long wait costs nothing.
func (bm *BindingManager) passiveDeclareWithRetry(ctx context.Context, exchange string) error {
	deadline := time.After(bm.bindTimeout)
	attempt := 0
	for {
		err := bm.passiveDeclare(exchange)
		if err == nil {
			return nil
		}
		attempt++
		if attempt%10 == 1 {
			bm.logger.Info("waiting for exchange", "exchange", exchange, "attempt", attempt)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("passive declare %s: %w", exchange, ctx.Err())
		case <-deadline:
			return fmt.Errorf("passive declare %s: timeout after %s", exchange, bm.bindTimeout)
		case <-time.After(2 * time.Second):
		}
	}
}

// passiveDeclare makes one passive declare attempt on its own channel.
func (bm *BindingManager) passiveDeclare(exchange string) error {
	ch, err := bm.conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel for passive declare %s: %w", exchange, err)
	}
	defer func() { _ = ch.Close() }()

	if err := ch.ExchangeDeclarePassive(exchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("passive declare %s: %w", exchange, err)
	}
	return nil
}

// runIDAttribute is the CloudEvents extension attribute and AMQP header that
// carries the run ID, per SDD section 4.3.
const runIDAttribute = "citadelrun"

// flattenHeaders converts an AMQP header table to string values.
//
// A value that is a JSON string is decoded to the string it holds. SDD section
// 10.1 sets DEBEZIUM_FORMAT_HEADER=json, and the same section maps three outbox
// columns onto headers, so eventType arrives on the wire as a quote, the name and
// a quote. A scenario selector writes the name, so comparing the encoded form
// against it never matches, and the failure reads as a missing event rather than
// as a mismatched encoding.
//
// The decode is the encoding read in reverse and not a strip of quote characters.
// An escape inside the JSON string is unescaped, and a value that is not a JSON
// string keeps every byte.
func flattenHeaders(table amqpdriver.Table) map[string]string {
	headers := make(map[string]string, len(table))
	for k, v := range table {
		headers[k] = decodeHeaderValue(fmt.Sprintf("%v", v))
	}
	return headers
}

// decodeHeaderValue decodes a JSON string and returns anything else unchanged.
func decodeHeaderValue(raw string) string {
	// A JSON string is the only form this decodes. A JSON object, a JSON array and
	// a bare number all keep their own text, because a scenario that matches on one
	// needs the punctuation.
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return raw
	}
	var decoded string
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return raw
	}
	return decoded
}

// extractRunID returns the run ID that the event trigger stamped on a message.
// The AMQP header comes first, because it survives a body that is not JSON.
// The CloudEvents extension attribute in the body is the fallback.
func extractRunID(headers map[string]string, body json.RawMessage) string {
	if runID := headers[runIDAttribute]; runID != "" {
		return runID
	}

	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	runID, _ := envelope[runIDAttribute].(string)
	return runID
}

// consumeMessages stores every delivery until the context ends or the broker
// takes the consumer away.
//
// A closed delivery channel means the AMQP channel or the connection died, and
// nothing re-establishes the consumer, so the exchange goes back to failed. SDD
// section 8.1 makes the health report the evidence that a spy queue is bound,
// and preflight of section 4.10 refuses to run against anything but bound. A
// status that stays bound for a consumer that is gone lets every
// event/not-captured assertion of section 8.3 pass on a buffer nothing can
// reach.
func (bm *BindingManager) consumeMessages(ctx context.Context, exchange string, deliveries <-chan amqpdriver.Delivery) {
	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-deliveries:
			if !ok {
				bm.setStatus(exchange, exchangeStateFailed)
				bm.logger.Error("delivery channel closed, the exchange captures nothing", "exchange", exchange)
				return
			}
			headers := flattenHeaders(d.Headers)

			msg := Message{
				ReceivedAt:  time.Now(),
				Exchange:    exchange,
				RoutingKey:  d.RoutingKey,
				Headers:     headers,
				RunID:       extractRunID(headers, json.RawMessage(d.Body)),
				CloudEvents: ValidateCloudEvents(json.RawMessage(d.Body)),
				Body:        json.RawMessage(d.Body),
			}
			bm.buf.Add(msg)
		}
	}
}
