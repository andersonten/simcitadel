package exampleapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	amqpdriver "github.com/rabbitmq/amqp091-go"

	"github.com/andersonten/simcitadel/internal/amqp"
)

// Consume declares the inbound topology and processes deliveries until the
// context ends.
//
// The app owns its inbound queue and declares it, which is what the real
// application does. The exchange is declared too, because the app must be able to
// start before anything publishes to it, and a topic exchange declared twice with
// the same arguments is not an error.
func (a *App) Consume(ctx context.Context, conn *amqp.Conn) error {
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open a channel: %w", err)
	}
	defer ch.Close()

	if err := ch.ExchangeDeclare(a.cfg.InboundExchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare the inbound exchange %s: %w", a.cfg.InboundExchange, err)
	}

	if _, err := ch.QueueDeclare(a.cfg.InboundQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare the inbound queue %s: %w", a.cfg.InboundQueue, err)
	}

	if err := ch.QueueBind(a.cfg.InboundQueue, a.cfg.InboundRoutingKey, a.cfg.InboundExchange, false, nil); err != nil {
		return fmt.Errorf("bind %s to %s: %w", a.cfg.InboundQueue, a.cfg.InboundExchange, err)
	}

	for _, rk := range a.cfg.InboundAdditionalRoutingKeys {
		if err := ch.QueueBind(a.cfg.InboundQueue, rk, a.cfg.InboundExchange, false, nil); err != nil {
			return fmt.Errorf("bind %s to %s with key %s: %w", a.cfg.InboundQueue, a.cfg.InboundExchange, rk, err)
		}
	}

	// One delivery at a time. The app is a demonstration and a single consumer
	// makes the log readable, which matters more here than throughput.
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("set the prefetch: %w", err)
	}

	deliveries, err := ch.Consume(a.cfg.InboundQueue, "example-app", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume from %s: %w", a.cfg.InboundQueue, err)
	}

	a.setAMQPReady(true)
	defer a.setAMQPReady(false)

	a.logger.Info("consuming",
		"exchange", a.cfg.InboundExchange,
		"queue", a.cfg.InboundQueue,
		"routingKey", a.cfg.InboundRoutingKey)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case delivery, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("the broker closed the delivery channel")
			}
			a.handle(ctx, delivery)
		}
	}
}

// handle processes one delivery and settles it.
//
// A failed event is rejected without requeue. Requeueing would spin the same
// failure at the speed of the broker, fill the log, and hide the cause. A
// scenario that depends on the event then fails on its await, which is the
// correct outcome: the app could not do its job, and the run says so.
func (a *App) handle(ctx context.Context, delivery amqpdriver.Delivery) {
	err := a.Process(ctx, delivery.Body)

	switch {
	case err == nil:
		if ackErr := delivery.Ack(false); ackErr != nil {
			a.logger.Error("acknowledge the delivery", "error", ackErr)
		}
	case errors.Is(err, ErrDuplicate):
		// A redelivery of a processed event is a success. The inbox exists for
		// exactly this, so the app acknowledges and writes no second outbox row.
		a.logger.Info("duplicate event acknowledged", "routingKey", delivery.RoutingKey)
		if ackErr := delivery.Ack(false); ackErr != nil {
			a.logger.Error("acknowledge the duplicate", "error", ackErr)
		}
	default:
		a.logger.Error("process the event",
			"error", err,
			"routingKey", delivery.RoutingKey,
			"exchange", delivery.Exchange)
		a.RecordFailure(ctx, delivery.Body, err)
		if nackErr := delivery.Nack(false, false); nackErr != nil {
			a.logger.Error("reject the delivery", "error", nackErr)
		}
	}
}

// RecordFailure writes the error onto the inbox row of an event that failed. It
// reports nothing, because a failure to record a failure must not replace the
// original error in the log.
//
// The retry count and the last error are two of the four inbox columns that
// inbox/assert reads, so a scenario can assert on a failure the app recorded.
func (a *App) RecordFailure(ctx context.Context, body []byte, cause error) {
	id := envelopeID(body)
	if id == "" {
		return
	}
	// processed_at stays null, so a redelivery of this event is processed again
	// rather than dropped as a duplicate.
	_, err := a.db.ExecContext(ctx, `
		INSERT INTO public.inbox (message_id, retry_count, last_error)
		VALUES ($1, 1, $2)
		ON CONFLICT (message_id) DO UPDATE
		SET retry_count = public.inbox.retry_count + 1,
		    last_error  = EXCLUDED.last_error`,
		id, cause.Error())
	if err != nil {
		a.logger.Warn("record the failure on the inbox row", "error", err)
	}
}

// envelopeID reads the id attribute alone. A body that fails to parse has no id,
// and the caller then records nothing.
func envelopeID(body []byte) string {
	var envelope struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.ID
}
