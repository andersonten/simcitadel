package runner

import (
	"regexp"
	"testing"
	"time"
)

func TestNewRunID_Format_IsValidUUID(t *testing.T) {
	id := NewRunID()

	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidPattern.MatchString(id) {
		t.Fatalf("run ID %q does not match UUIDv7 pattern", id)
	}
}

func TestNewRunID_Sequential_SortsChronologically(t *testing.T) {
	first := NewRunID()
	time.Sleep(2 * time.Millisecond)
	second := NewRunID()

	if first >= second {
		t.Fatalf("expected %q < %q for chronological sort", first, second)
	}
}
