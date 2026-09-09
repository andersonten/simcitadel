package capture

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuffer_Add_StoresMessage(t *testing.T) {
	buf := NewBuffer(10)
	msg := Message{
		ReceivedAt: time.Now(),
		Exchange:   "test.exchange",
		RoutingKey: "article.created",
		Body:       json.RawMessage(`{"id":"1"}`),
	}
	buf.Add(msg)

	result := buf.Query(Filter{})
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Seq != 1 {
		t.Errorf("expected seq 1, got %d", result[0].Seq)
	}
	if result[0].Exchange != "test.exchange" {
		t.Errorf("expected exchange %q, got %q", "test.exchange", result[0].Exchange)
	}
}

func TestBuffer_Add_FullBuffer_DropsOldest(t *testing.T) {
	buf := NewBuffer(3)
	for i := 0; i < 5; i++ {
		buf.Add(Message{
			ReceivedAt: time.Now(),
			Exchange:   "test.exchange",
			RoutingKey: "key",
			Body:       json.RawMessage(`{}`),
		})
	}

	result := buf.Query(Filter{})
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
	if result[0].Seq != 3 {
		t.Errorf("expected oldest seq 3, got %d", result[0].Seq)
	}
	if result[2].Seq != 5 {
		t.Errorf("expected newest seq 5, got %d", result[2].Seq)
	}
}

func TestBuffer_Query_EmptyFilter_ReturnsAll(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "a", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "b", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "c", Body: json.RawMessage(`{}`)})

	result := buf.Query(Filter{})
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
}

func TestBuffer_Query_ByExchange_FiltersCorrectly(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "alpha", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "beta", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "alpha", Body: json.RawMessage(`{}`)})

	result := buf.Query(Filter{Exchange: "alpha"})
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	for _, msg := range result {
		if msg.Exchange != "alpha" {
			t.Errorf("expected exchange %q, got %q", "alpha", msg.Exchange)
		}
	}
}

func TestBuffer_Count_ReturnsMatchCount(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "alpha", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "beta", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "alpha", Body: json.RawMessage(`{}`)})

	count := buf.Count(Filter{Exchange: "alpha"})
	if count != 2 {
		t.Errorf("expected count 2, got %d", count)
	}

	countAll := buf.Count(Filter{})
	if countAll != 3 {
		t.Errorf("expected total count 3, got %d", countAll)
	}
}

func TestBuffer_Add_PerExchange_IsolatesDrops(t *testing.T) {
	buf := NewBuffer(2)
	for i := 0; i < 3; i++ {
		buf.Add(Message{Exchange: "alpha", Body: json.RawMessage(`{}`)})
	}
	buf.Add(Message{Exchange: "beta", Body: json.RawMessage(`{}`)})

	alphaCount := buf.Count(Filter{Exchange: "alpha"})
	if alphaCount != 2 {
		t.Errorf("expected 2 alpha messages, got %d", alphaCount)
	}

	betaCount := buf.Count(Filter{Exchange: "beta"})
	if betaCount != 1 {
		t.Errorf("expected 1 beta message, got %d", betaCount)
	}

	all := buf.Query(Filter{})
	if len(all) != 3 {
		t.Errorf("expected 3 total messages, got %d", len(all))
	}
}

func TestBuffer_Query_PerExchange_OrdersBySeq(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "a", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "b", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "a", Body: json.RawMessage(`{}`)})

	result := buf.Query(Filter{})
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
	for i := 1; i < len(result); i++ {
		if result[i].Seq <= result[i-1].Seq {
			t.Errorf("results not sorted by seq: position %d has seq %d, position %d has seq %d",
				i-1, result[i-1].Seq, i, result[i].Seq)
		}
	}
}

func TestBuffer_Clear_EmptiesBuffer(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "a", Body: json.RawMessage(`{}`)})
	buf.Add(Message{Exchange: "b", Body: json.RawMessage(`{}`)})
	buf.Clear()

	result := buf.Query(Filter{})
	if len(result) != 0 {
		t.Fatalf("expected 0 messages after clear, got %d", len(result))
	}

	buf.Add(Message{Exchange: "c", Body: json.RawMessage(`{}`)})
	result = buf.Query(Filter{})
	if len(result) != 1 {
		t.Fatalf("expected 1 message after re-add, got %d", len(result))
	}
	if result[0].Seq != 1 {
		t.Errorf("expected seq reset to 1, got %d", result[0].Seq)
	}
}
