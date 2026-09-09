package trigger_test

import (
	"testing"

	"github.com/andersonten/simcitadel/internal/trigger"
)

func TestBudget_Acquire_WithinLimit_ReturnsTrue(t *testing.T) {
	b := trigger.NewBudget(3)
	for i := range 3 {
		if !b.Acquire("run-1") {
			t.Fatalf("acquire %d: want true, got false", i+1)
		}
	}
}

func TestBudget_Acquire_ExceedsLimit_ReturnsFalse(t *testing.T) {
	b := trigger.NewBudget(2)
	b.Acquire("run-1")
	b.Acquire("run-1")
	if b.Acquire("run-1") {
		t.Fatal("acquire beyond limit: want false, got true")
	}
}

func TestBudget_Reset_RunID_ClearsCounter(t *testing.T) {
	b := trigger.NewBudget(2)
	b.Acquire("run-1")
	b.Acquire("run-1")
	b.Reset("run-1")
	if !b.Acquire("run-1") {
		t.Fatal("acquire after reset: want true, got false")
	}
}

func TestBudget_Reset_Empty_ClearsAll(t *testing.T) {
	b := trigger.NewBudget(1)
	b.Acquire("run-1")
	b.Acquire("run-2")
	b.Reset("")
	if !b.Acquire("run-1") {
		t.Fatal("acquire run-1 after reset all: want true, got false")
	}
	if !b.Acquire("run-2") {
		t.Fatal("acquire run-2 after reset all: want true, got false")
	}
}

func TestBudget_Count_ReturnsCurrentCount(t *testing.T) {
	b := trigger.NewBudget(10)
	if got := b.Count("run-1"); got != 0 {
		t.Fatalf("count before acquire: want 0, got %d", got)
	}
	b.Acquire("run-1")
	b.Acquire("run-1")
	b.Acquire("run-1")
	if got := b.Count("run-1"); got != 3 {
		t.Fatalf("count after 3 acquires: want 3, got %d", got)
	}
}
