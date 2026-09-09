package trigger

import "sync"

// Budget tracks the number of messages published per run.
type Budget struct {
	mu       sync.Mutex
	counters map[string]int
	limit    int
}

// NewBudget creates a budget with the given per-run limit.
func NewBudget(limit int) *Budget {
	return &Budget{
		counters: make(map[string]int),
		limit:    limit,
	}
}

// Acquire attempts to consume one unit from the budget for the given run ID.
// Acquire returns false if the budget is exhausted.
func (b *Budget) Acquire(runID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.counters[runID] >= b.limit {
		return false
	}
	b.counters[runID]++
	return true
}

// Reset clears the budget counter for the given run ID.
// Reset clears all counters if runID is empty.
func (b *Budget) Reset(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if runID == "" {
		b.counters = make(map[string]int)
		return
	}
	delete(b.counters, runID)
}

// Count returns the current count for the given run ID.
func (b *Budget) Count(runID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counters[runID]
}
