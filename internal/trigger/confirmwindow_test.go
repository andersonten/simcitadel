package trigger_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/trigger"
	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// SDD section 7.5 gives the confirm wait one name and one value:
// publishConfirmTimeout, default 5 s. The handler bounds the publish and the
// publisher waits for the frames, so the two hold the same value or the shorter
// one wins in silence.
//
// cmd/event-trigger reads EVENT_TRIGGER_PUBLISH_CONFIRM_TIMEOUT and passes it to
// the publisher alone. SetConfirmTimeout has no caller outside a test, so the
// handler kept the 5 s default. A stack that configures 30 s cut every publish
// at 5 s and reported "broker did not confirm within 5s", which names a value
// the operator never set.

// windowPublisher reports its own confirm window and records the deadline the
// handler put on the publish.
type windowPublisher struct {
	window   time.Duration
	deadline chan time.Duration
}

func (p *windowPublisher) ConfirmTimeout() time.Duration { return p.window }

func (p *windowPublisher) Publish(ctx context.Context, _, _ string, _ amqpdriver.Publishing) (*citadelamqp.PublishResult, error) {
	remaining := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		remaining = time.Until(dl)
	}
	p.deadline <- remaining
	return &citadelamqp.PublishResult{Confirmed: true}, nil
}

// plainPublisher reports no window of its own.
type plainPublisher struct {
	deadline chan time.Duration
}

func (p *plainPublisher) Publish(ctx context.Context, _, _ string, _ amqpdriver.Publishing) (*citadelamqp.PublishResult, error) {
	remaining := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		remaining = time.Until(dl)
	}
	p.deadline <- remaining
	return &citadelamqp.PublishResult{Confirmed: true}, nil
}

func publishOnce(t *testing.T, srv *trigger.Server) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(validEvent()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Citadel-Run", "0191c3aa-0000-7000-8000-0000000000ff")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
}

// TestNewServer_PublisherWithItsOwnConfirmWindow_BoundsThePublishByIt proves the
// handler takes the window from the publisher, so one configured value governs
// both halves of the publish.
func TestNewServer_PublisherWithItsOwnConfirmWindow_BoundsThePublishByIt(t *testing.T) {
	pub := &windowPublisher{window: 37 * time.Second, deadline: make(chan time.Duration, 1)}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	publishOnce(t, srv)

	remaining := <-pub.deadline
	if remaining < 0 {
		t.Fatal("the handler put no deadline on the publish")
	}
	if remaining < 30*time.Second {
		t.Errorf("the handler bounded the publish at about %s, and the publisher waits 37s, so the configured window is ignored", remaining.Round(time.Second))
	}
}

// TestNewServer_PublisherWithNoConfirmWindow_UsesTheDefault holds the default of
// SDD section 7.5 for a publisher that reports no window.
func TestNewServer_PublisherWithNoConfirmWindow_UsesTheDefault(t *testing.T) {
	pub := &plainPublisher{deadline: make(chan time.Duration, 1)}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })

	publishOnce(t, srv)

	remaining := <-pub.deadline
	if remaining < 0 {
		t.Fatal("the handler put no deadline on the publish")
	}
	if remaining > trigger.DefaultConfirmTimeout {
		t.Errorf("the handler bounded the publish at %s, want at most the default %s", remaining, trigger.DefaultConfirmTimeout)
	}
	if remaining < trigger.DefaultConfirmTimeout/2 {
		t.Errorf("the handler bounded the publish at %s, want about the default %s", remaining, trigger.DefaultConfirmTimeout)
	}
}

// TestSetConfirmTimeout_AfterTheConstructor_StillWins keeps the explicit setter
// working, because a caller that wants another bound must still be able to say
// so.
func TestSetConfirmTimeout_AfterTheConstructor_StillWins(t *testing.T) {
	pub := &windowPublisher{window: 37 * time.Second, deadline: make(chan time.Duration, 1)}
	srv := trigger.NewServer(pub, trigger.NewBudget(100), func() bool { return true })
	srv.SetConfirmTimeout(time.Second)

	publishOnce(t, srv)

	remaining := <-pub.deadline
	if remaining > time.Second {
		t.Errorf("the handler bounded the publish at %s, want at most 1s", remaining)
	}
}
