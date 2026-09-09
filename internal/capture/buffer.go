package capture

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Message holds one captured AMQP message with metadata. Body holds the
// verbatim AMQP body, which is not always JSON. MarshalJSON and UnmarshalJSON
// hold the wire form of the entry, so the struct tags below describe the field
// names only.
type Message struct {
	Seq         int64             `json:"seq"`
	ReceivedAt  time.Time         `json:"receivedAt"`
	Exchange    string            `json:"exchange"`
	RoutingKey  string            `json:"routingKey"`
	Headers     map[string]string `json:"headers"`
	RunID       string            `json:"runId"`
	CloudEvents *CloudEventsInfo  `json:"cloudEvents"`
	Body        json.RawMessage   `json:"body"`
}

// Buffer is a thread-safe ring buffer that stores captured messages.
// Each exchange has its own ring buffer with maxSize capacity.
type Buffer struct {
	mu        sync.RWMutex
	exchanges map[string][]Message
	maxSize   int
	seq       int64
}

// NewBuffer creates a buffer with the given maximum size per exchange.
func NewBuffer(maxSize int) *Buffer {
	return &Buffer{
		exchanges: make(map[string][]Message),
		maxSize:   maxSize,
	}
}

// Add appends a message to the per-exchange ring buffer.
// If the exchange buffer is full, the oldest message in that exchange is dropped.
// Add supplies the CloudEvents verdict of SDD section 8.2 when the caller
// leaves it out, so every stored entry states whether the body is a valid
// event.
func (b *Buffer) Add(msg Message) {
	if msg.CloudEvents == nil {
		msg.CloudEvents = ValidateCloudEvents(msg.Body)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	msg.Seq = b.seq
	ex := msg.Exchange
	msgs := b.exchanges[ex]
	if len(msgs) >= b.maxSize {
		copy(msgs, msgs[1:])
		msgs[len(msgs)-1] = msg
	} else {
		msgs = append(msgs, msg)
	}
	b.exchanges[ex] = msgs
}

// Query returns all messages across all exchanges that match the given filter.
// Results are sorted by sequence number.
func (b *Buffer) Query(f Filter) []Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var result []Message
	for _, msgs := range b.exchanges {
		for _, msg := range msgs {
			if f.Match(msg) {
				result = append(result, msg)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Seq < result[j].Seq
	})
	return result
}

// Count returns the number of messages across all exchanges that match the given filter.
func (b *Buffer) Count(f Filter) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, msgs := range b.exchanges {
		for _, msg := range msgs {
			if f.Match(msg) {
				n++
			}
		}
	}
	return n
}

// Clear removes all messages from all exchanges and resets the sequence counter.
func (b *Buffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.exchanges = make(map[string][]Message)
	b.seq = 0
}
