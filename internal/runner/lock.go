package runner

import (
	"context"
	"fmt"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// Lock represents the environment lock acquired via an exclusive RabbitMQ queue.
type Lock struct {
	conn *citadelamqp.Conn
	ch   *amqpdriver.Channel
}

// acquireLock is the seam the runner calls to take the environment lock. A test
// in this package replaces it to exercise the lifecycle of SDD section 4.2
// without a broker. Production never reassigns it.
var acquireLock = AcquireLock

// AcquireLock attempts to declare the exclusive lock queue on the broker.
// AcquireLock returns a LockHeldError if another client holds the lock, and
// names the environment in the message as SDD section 4.8 requires.
func AcquireLock(ctx context.Context, amqpURL, environment string) (*Lock, error) {
	conn, err := citadelamqp.Dial(ctx, amqpURL)
	if err != nil {
		return nil, &EnvironmentError{Msg: fmt.Sprintf("connect to broker for lock: %v", err)}
	}

	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, &EnvironmentError{Msg: fmt.Sprintf("open channel for lock: %v", err)}
	}

	_, err = ch.QueueDeclare(
		citadelamqp.LockQueueName(),
		false, // non-durable
		true,  // auto-delete
		true,  // exclusive
		false, // no-wait
		nil,
	)
	if err != nil {
		_ = ch.Close()
		_ = conn.Close()
		if isResourceLocked(err) {
			return nil, &LockHeldError{Msg: fmt.Sprintf("another run holds the lock on environment %q", environment)}
		}
		return nil, &EnvironmentError{Msg: fmt.Sprintf("declare lock queue: %v", err)}
	}

	return &Lock{conn: conn, ch: ch}, nil
}

// Release closes the lock connection, which releases the exclusive queue.
func (l *Lock) Release() error {
	var firstErr error
	if l.ch != nil {
		if err := l.ch.Close(); err != nil {
			firstErr = fmt.Errorf("close lock channel: %w", err)
		}
	}
	if l.conn != nil {
		if err := l.conn.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close lock connection: %w", err)
		}
	}
	return firstErr
}

// isResourceLocked returns true if the error is a RESOURCE_LOCKED (405) channel close.
func isResourceLocked(err error) bool {
	amqpErr, ok := err.(*amqpdriver.Error)
	if !ok {
		return false
	}
	return amqpErr.Code == 405
}
