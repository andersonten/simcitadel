package amqp

import (
	"context"
	"fmt"
	"sync"
	"time"

	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// Publisher sends messages to an AMQP exchange with publisher confirms and mandatory routing.
type Publisher struct {
	ch             *amqpdriver.Channel
	confirmCh      chan amqpdriver.Confirmation
	returnCh       chan amqpdriver.Return
	confirmTimeout time.Duration

	// mu makes one publish and its two acknowledgement frames one operation.
	// The confirm channel and the return channel belong to the channel, not to
	// a message, so two concurrent publishers read each other's frames.
	mu sync.Mutex
}

// ConfirmTimeoutError reports that the broker did not confirm a publish inside
// the timeout. SDD section 7.5 makes the wait an interface, so the duration
// travels on the error and a caller does not read it out of a message.
type ConfirmTimeoutError struct {
	Timeout time.Duration
}

// Error names the timeout that expired.
func (e *ConfirmTimeoutError) Error() string {
	return fmt.Sprintf("broker did not confirm within %s", e.Timeout)
}

// NewPublisher creates a publisher on the given channel with publisher confirms enabled.
// NewPublisher registers the return handler before any publish.
func NewPublisher(ch *amqpdriver.Channel, confirmTimeout time.Duration) (*Publisher, error) {
	if err := ch.Confirm(false); err != nil {
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}

	confirmCh := ch.NotifyPublish(make(chan amqpdriver.Confirmation, 1))
	returnCh := ch.NotifyReturn(make(chan amqpdriver.Return, 1))

	return &Publisher{
		ch:             ch,
		confirmCh:      confirmCh,
		returnCh:       returnCh,
		confirmTimeout: confirmTimeout,
	}, nil
}

// PublishResult holds the outcome of a publish operation.
type PublishResult struct {
	Confirmed bool
	Returned  bool
	Return    *amqpdriver.Return
}

// ConfirmTimeout returns the window this publisher waits for a broker confirm.
// SDD section 7.5 gives that window one name, publishConfirmTimeout, so a caller
// that bounds a request reads the value here instead of holding a second copy.
func (p *Publisher) ConfirmTimeout() time.Duration {
	return p.confirmTimeout
}

// Publish sends a message to the exchange with the given routing key.
// Publish sets mandatory=true, waits for the broker confirm, and returns
// both the confirm result and any basic.return.
// Publish returns an error if the broker does not confirm within the timeout.
func (p *Publisher) Publish(ctx context.Context, exchange, routingKey string, msg amqpdriver.Publishing) (*PublishResult, error) {
	if msg.DeliveryMode == 0 {
		msg.DeliveryMode = amqpdriver.Persistent
	}

	return p.publish(ctx, func() error {
		if err := p.ch.PublishWithContext(ctx, exchange, routingKey, true, false, msg); err != nil {
			return fmt.Errorf("publish to %s/%s: %w", exchange, routingKey, err)
		}
		return nil
	})
}

// publish makes one publish and its two acknowledgement frames one operation.
//
// The discard is the reason this is one method and not two statements at the
// call site. A publish that gives up on the timeout, on a cancelled request or
// on a nack leaves its own frames in the two buffered channels, and the broker
// delivers them a moment later. Without the discard the next publish reads them
// as its own, so the service stays one message behind for the life of the
// process, and an unroutable message reports as published.
func (p *Publisher) publish(ctx context.Context, send func() error) (*PublishResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.discardStaleFrames()

	if err := send(); err != nil {
		return nil, err
	}

	return p.awaitResult(ctx)
}

// discardStaleFrames empties both notification channels of any frame that
// belongs to an earlier publish. It must run before the publish, because the
// broker sends the basic.return of the new message before its ack. A discard
// after the publish throws that return away.
//
// The receive tests its second result, because the driver closes both channels
// when the AMQP channel dies and a read of a closed channel succeeds forever.
func (p *Publisher) discardStaleFrames() {
	for {
		select {
		case _, ok := <-p.returnCh:
			if !ok {
				return
			}
			continue
		default:
		}
		select {
		case _, ok := <-p.confirmCh:
			if !ok {
				return
			}
			continue
		default:
			return
		}
	}
}

// awaitResult waits for the publisher confirm and any basic.return within the
// timeout. SDD section 7.5 requires both signals, because the two race: a
// select that returns on the confirm alone loses the return of an unroutable
// message and reports it as published.
func (p *Publisher) awaitResult(ctx context.Context) (*PublishResult, error) {
	timer := time.NewTimer(p.confirmTimeout)
	defer timer.Stop()

	result := &PublishResult{}

	for {
		// The broker sends the return frame before the ack, and both channels
		// are buffered, so a return that exists is already readable. Taking it
		// first removes the race instead of depending on which case the select
		// below happens to choose.
		select {
		case ret := <-p.returnCh:
			result.Returned = true
			result.Return = &ret
		default:
		}

		if result.Confirmed {
			return result, nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("await publish confirm: %w", ctx.Err())
		case <-timer.C:
			return nil, &ConfirmTimeoutError{Timeout: p.confirmTimeout}
		case ret := <-p.returnCh:
			result.Returned = true
			result.Return = &ret
		case confirm := <-p.confirmCh:
			if !confirm.Ack {
				return nil, fmt.Errorf("broker nack: message not confirmed")
			}
			result.Confirmed = true
		}
	}
}
