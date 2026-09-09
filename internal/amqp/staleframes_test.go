package amqp

import (
	"context"
	"errors"
	"testing"
	"time"

	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// SDD section 7.5 makes a publish confirm an interface: the trigger answers 202
// only for a message the broker acknowledged, 422 for a message the broker
// returned, and 502 when neither arrives inside publishConfirmTimeout. "A
// timeout is a 502 ... never a silent success."
//
// The confirm channel and the return channel belong to the AMQP channel and not
// to a message. A publish that gives up on the timeout, on a cancelled request,
// or on a nack leaves its own frames in those buffered channels, and the broker
// delivers them a moment later. The next publish then reads the frames of the
// one before it, and the service stays one message behind for the life of the
// process.
//
// The direction that matters is the false pass: publish N reads the ack of
// publish N-1, answers 202 with routed true, and its own basic.return arrives
// after the handler has already replied. The message went nowhere and the run
// records a pass.

// TestPublish_AfterATimedOutPublish_DoesNotReportTheStaleConfirm proves the
// whole sequence through the same method the trigger calls.
func TestPublish_AfterATimedOutPublish_DoesNotReportTheStaleConfirm(t *testing.T) {
	confirmCh := make(chan amqpdriver.Confirmation, 1)
	returnCh := make(chan amqpdriver.Return, 1)

	p := &Publisher{
		confirmCh:      confirmCh,
		returnCh:       returnCh,
		confirmTimeout: 20 * time.Millisecond,
	}

	// The first publish is a message the broker never confirms in time.
	_, err := p.publish(context.Background(), func() error { return nil })
	var timeout *ConfirmTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("the first publish returned %v, want a confirm timeout", err)
	}

	// The broker answers the first publish a moment after the trigger gave up.
	confirmCh <- amqpdriver.Confirmation{DeliveryTag: 1, Ack: true}

	// The broker says nothing about the second publish, so the only right answer
	// is another timeout and a 502.
	result, err := p.publish(context.Background(), func() error { return nil })
	if err == nil {
		t.Fatalf("the second publish reported %+v on a message the broker never acknowledged, so the trigger answers 202 routed for a message that went nowhere", result)
	}
	var second *ConfirmTimeoutError
	if !errors.As(err, &second) {
		t.Fatalf("the second publish returned %v, want a confirm timeout", err)
	}
}

// TestPublish_AfterACancelledPublish_DoesNotReportTheStaleReturn covers the
// other direction of the same defect. A stale basic.return makes the trigger
// answer 422 for a message the broker routed, which fails a run for a reason
// that is not in the system under test.
func TestPublish_AfterACancelledPublish_DoesNotReportTheStaleReturn(t *testing.T) {
	confirmCh := make(chan amqpdriver.Confirmation, 1)
	returnCh := make(chan amqpdriver.Return, 1)

	p := &Publisher{
		confirmCh:      confirmCh,
		returnCh:       returnCh,
		confirmTimeout: time.Second,
	}

	// The client of the first publish disconnected, so the handler context ended
	// before either frame arrived.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.publish(cancelled, func() error { return nil }); err == nil {
		t.Fatal("a cancelled publish must be an error")
	}

	// The broker answers the first publish: it was unroutable.
	returnCh <- amqpdriver.Return{Exchange: "public.orchestration-engine.Task.v1", RoutingKey: "public.orchestration-engine.Task.v1"}

	// The second publish is routable and the broker acknowledges it.
	result, err := p.publish(context.Background(), func() error {
		confirmCh <- amqpdriver.Confirmation{DeliveryTag: 2, Ack: true}
		return nil
	})
	if err != nil {
		t.Fatalf("the second publish: %v", err)
	}
	if result.Returned {
		t.Fatal("the second publish inherited the basic.return of the first, so the trigger answers 422 for a message the broker routed")
	}
}

// TestPublish_AfterANack_DoesNotReportTheStaleConfirm covers the third way a
// publish abandons its frames.
func TestPublish_AfterANack_DoesNotReportTheStaleConfirm(t *testing.T) {
	confirmCh := make(chan amqpdriver.Confirmation, 1)
	returnCh := make(chan amqpdriver.Return, 1)

	p := &Publisher{
		confirmCh:      confirmCh,
		returnCh:       returnCh,
		confirmTimeout: 20 * time.Millisecond,
	}

	confirmCh <- amqpdriver.Confirmation{DeliveryTag: 1, Ack: false}
	if _, err := p.publish(context.Background(), func() error { return nil }); err == nil {
		t.Fatal("a nack must be an error")
	}

	// The basic.return of the nacked message arrives after the handler replied.
	returnCh <- amqpdriver.Return{Exchange: "public.orchestration-engine.Task.v1", RoutingKey: "public.orchestration-engine.Task.v1"}

	result, err := p.publish(context.Background(), func() error {
		confirmCh <- amqpdriver.Confirmation{DeliveryTag: 2, Ack: true}
		return nil
	})
	if err != nil {
		t.Fatalf("the publish after a nack: %v", err)
	}
	if result.Returned {
		t.Fatal("the publish after a nack inherited the earlier basic.return")
	}
}

// TestPublish_ClosedNotificationChannels_DoNotHangTheDrain holds the line on the
// fix. The driver closes both notification channels when the AMQP channel dies,
// and a read of a closed channel succeeds forever, so a drain that does not test
// the second result of the receive never returns.
func TestPublish_ClosedNotificationChannels_DoNotHangTheDrain(t *testing.T) {
	confirmCh := make(chan amqpdriver.Confirmation)
	returnCh := make(chan amqpdriver.Return)
	close(confirmCh)
	close(returnCh)

	p := &Publisher{
		confirmCh:      confirmCh,
		returnCh:       returnCh,
		confirmTimeout: 20 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.publish(context.Background(), func() error { return nil })
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a publish on a dead AMQP channel never returned")
	}
}
