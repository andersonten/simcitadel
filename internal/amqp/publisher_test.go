package amqp

import (
	"context"
	"errors"
	"testing"
	"time"

	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// TestAwaitResult_ReturnAndConfirmBothReady_ReportsTheReturn proves that a
// basic.return is never lost when the confirm is ready at the same moment. SDD
// section 7.5 requires the implementation to wait for both signals instead of
// assuming an order.
func TestAwaitResult_ReturnAndConfirmBothReady_ReportsTheReturn(t *testing.T) {
	for i := 0; i < 200; i++ {
		confirmCh := make(chan amqpdriver.Confirmation, 1)
		returnCh := make(chan amqpdriver.Return, 1)

		returnCh <- amqpdriver.Return{Exchange: "public.x.v1", RoutingKey: "k"}
		confirmCh <- amqpdriver.Confirmation{DeliveryTag: 1, Ack: true}

		p := &Publisher{
			confirmCh:      confirmCh,
			returnCh:       returnCh,
			confirmTimeout: time.Second,
		}

		result, err := p.awaitResult(context.Background())
		if err != nil {
			t.Fatalf("iteration %d: awaitResult: %v", i, err)
		}
		if !result.Returned {
			t.Fatalf("iteration %d: the basic.return was lost, so an unroutable message reports as published", i)
		}
		if result.Return == nil || result.Return.Exchange != "public.x.v1" || result.Return.RoutingKey != "k" {
			t.Fatalf("iteration %d: the return carries no exchange and routing key", i)
		}
	}
}

// TestAwaitResult_ReturnAfterTheConfirm_IsStillReported covers the order in
// which the confirm is read first and the return is delivered a moment later.
func TestAwaitResult_ReturnBeforeTheConfirm_ReportsTheReturn(t *testing.T) {
	confirmCh := make(chan amqpdriver.Confirmation, 1)
	returnCh := make(chan amqpdriver.Return, 1)

	returnCh <- amqpdriver.Return{Exchange: "public.y.v1", RoutingKey: "k2"}

	p := &Publisher{confirmCh: confirmCh, returnCh: returnCh, confirmTimeout: time.Second}

	go func() { confirmCh <- amqpdriver.Confirmation{DeliveryTag: 1, Ack: true} }()

	result, err := p.awaitResult(context.Background())
	if err != nil {
		t.Fatalf("awaitResult: %v", err)
	}
	if !result.Returned {
		t.Fatal("the basic.return was lost")
	}
}

// TestAwaitResult_NoConfirm_ReturnsATypedTimeoutCarryingTheDuration holds SDD
// section 7.5: the 502 message names the timeout, so the duration travels on a
// typed error rather than inside a string the caller must match.
func TestAwaitResult_NoConfirm_ReturnsATypedTimeoutCarryingTheDuration(t *testing.T) {
	p := &Publisher{
		confirmCh:      make(chan amqpdriver.Confirmation),
		returnCh:       make(chan amqpdriver.Return),
		confirmTimeout: 10 * time.Millisecond,
	}

	_, err := p.awaitResult(context.Background())
	if err == nil {
		t.Fatal("a publish the broker never confirmed must be an error")
	}

	var timeoutErr *ConfirmTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error is %T, want *ConfirmTimeoutError so a caller does not match on the message", err)
	}
	if timeoutErr.Timeout != 10*time.Millisecond {
		t.Errorf("Timeout = %s, want 10ms", timeoutErr.Timeout)
	}
	if got := err.Error(); got != "broker did not confirm within 10ms" {
		t.Errorf("Error() = %q, want the SDD section 7.5 wording", got)
	}
}

// TestAwaitResult_Nack_IsAnError holds SDD section 7.5: a nack is never a
// silent success.
func TestAwaitResult_Nack_IsAnError(t *testing.T) {
	confirmCh := make(chan amqpdriver.Confirmation, 1)
	confirmCh <- amqpdriver.Confirmation{DeliveryTag: 1, Ack: false}

	p := &Publisher{
		confirmCh:      confirmCh,
		returnCh:       make(chan amqpdriver.Return, 1),
		confirmTimeout: time.Second,
	}

	if _, err := p.awaitResult(context.Background()); err == nil {
		t.Fatal("a broker nack must be an error")
	}
}
