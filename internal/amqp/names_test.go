package amqp

import "testing"

func TestSpyQueueName_Exchange_ReturnsCorrectFormat(t *testing.T) {
	tests := []struct {
		exchange string
		want     string
	}{
		{"public.orchestration-engine.Article.v1", "citadel.spy.public.orchestration-engine.Article.v1"},
		{"simple", "citadel.spy.simple"},
		{"a.b.c", "citadel.spy.a.b.c"},
	}
	for _, tt := range tests {
		t.Run(tt.exchange, func(t *testing.T) {
			got := SpyQueueName(tt.exchange)
			if got != tt.want {
				t.Errorf("SpyQueueName(%q) = %q, want %q", tt.exchange, got, tt.want)
			}
		})
	}
}

func TestLockQueueName_ReturnsExpectedName(t *testing.T) {
	got := LockQueueName()
	want := "citadel.lock"
	if got != want {
		t.Errorf("LockQueueName() = %q, want %q", got, want)
	}
}

func TestUnroutableQueueName_ReturnsExpectedName(t *testing.T) {
	got := UnroutableQueueName()
	want := "citadel.unroutable"
	if got != want {
		t.Errorf("UnroutableQueueName() = %q, want %q", got, want)
	}
}

func TestUnroutableExchangeName_ReturnsExpectedName(t *testing.T) {
	got := UnroutableExchangeName()
	want := "citadel.unroutable.ae"
	if got != want {
		t.Errorf("UnroutableExchangeName() = %q, want %q", got, want)
	}
}
