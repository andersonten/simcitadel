package steps

import (
	"slices"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

func TestInboxColumns_ConfiguredNames_AreUsedInOrder(t *testing.T) {
	inbox := &config.InboxConfig{
		Table:             "svc_inbox",
		MessageIDColumn:   "msg_id",
		ProcessedAtColumn: "handled_at",
		RetryCountColumn:  "attempts",
		LastErrorColumn:   "failure",
	}

	want := []string{"msg_id", "handled_at", "attempts", "failure"}
	if got := inboxColumns(inbox); !slices.Equal(got, want) {
		t.Errorf("inboxColumns() = %v, want %v", got, want)
	}
}

// TestInboxColumns_EmptyNames guards a configuration that did not pass through
// config.Load, where no default has been applied yet.
func TestInboxColumns_EmptyNames_FallBackToReferenceDefaults(t *testing.T) {
	want := []string{
		config.DefaultInboxMessageIDColumn,
		config.DefaultInboxProcessedAtColumn,
		config.DefaultInboxRetryCountColumn,
		config.DefaultInboxLastErrorColumn,
	}

	if got := inboxColumns(&config.InboxConfig{}); !slices.Equal(got, want) {
		t.Errorf("inboxColumns() = %v, want %v", got, want)
	}
}

func TestInboxColumns_PartialNames_FillOnlyTheGaps(t *testing.T) {
	inbox := &config.InboxConfig{MessageIDColumn: "msg_id"}

	want := []string{
		"msg_id",
		config.DefaultInboxProcessedAtColumn,
		config.DefaultInboxRetryCountColumn,
		config.DefaultInboxLastErrorColumn,
	}

	if got := inboxColumns(inbox); !slices.Equal(got, want) {
		t.Errorf("inboxColumns() = %v, want %v", got, want)
	}
}
