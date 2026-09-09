package trigger_test

import (
	"testing"

	"github.com/andersonten/simcitadel/internal/trigger"
)

func TestValidateCloudEvents_ValidEvent_NoError(t *testing.T) {
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-001",
		"type":        "ArticleSubmitted",
		"source":      "SRV-SUBMISSIONS",
		"version":     "1.0.0",
		"subject":     "/articles/12345",
	}
	if err := trigger.ValidateCloudEvents(event); err != nil {
		t.Fatalf("valid event: want no error, got %v", err)
	}
}

func TestValidateCloudEvents_MissingSpecversion_ReturnsError(t *testing.T) {
	event := map[string]any{
		"id":      "evt-001",
		"type":    "ArticleSubmitted",
		"source":  "SRV-SUBMISSIONS",
		"version": "1.0.0",
	}
	err := trigger.ValidateCloudEvents(event)
	if err == nil {
		t.Fatal("missing specversion: want error, got nil")
	}
}

func TestValidateCloudEvents_MissingId_ReturnsError(t *testing.T) {
	event := map[string]any{
		"specversion": "1.0",
		"type":        "ArticleSubmitted",
		"source":      "SRV-SUBMISSIONS",
		"version":     "1.0.0",
	}
	err := trigger.ValidateCloudEvents(event)
	if err == nil {
		t.Fatal("missing id: want error, got nil")
	}
}

func TestValidateCloudEvents_MissingType_ReturnsError(t *testing.T) {
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-001",
		"source":      "SRV-SUBMISSIONS",
		"version":     "1.0.0",
	}
	err := trigger.ValidateCloudEvents(event)
	if err == nil {
		t.Fatal("missing type: want error, got nil")
	}
}

func TestValidateCloudEvents_MissingSource_ReturnsError(t *testing.T) {
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-001",
		"type":        "ArticleSubmitted",
		"version":     "1.0.0",
	}
	err := trigger.ValidateCloudEvents(event)
	if err == nil {
		t.Fatal("missing source: want error, got nil")
	}
}

func TestValidateCloudEvents_MissingVersion_ReturnsError(t *testing.T) {
	event := map[string]any{
		"specversion": "1.0",
		"id":          "evt-001",
		"type":        "ArticleSubmitted",
		"source":      "SRV-SUBMISSIONS",
	}
	err := trigger.ValidateCloudEvents(event)
	if err == nil {
		t.Fatal("missing version: want error, got nil")
	}
}

func TestValidateCloudEvents_InvalidSpecVersion_ReturnsError(t *testing.T) {
	event := map[string]any{
		"specversion": "0.3",
		"id":          "evt-001",
		"type":        "ArticleSubmitted",
		"source":      "SRV-SUBMISSIONS",
		"version":     "1.0.0",
	}
	err := trigger.ValidateCloudEvents(event)
	if err == nil {
		t.Fatal("invalid specversion: want error, got nil")
	}
}
