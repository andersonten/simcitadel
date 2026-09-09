package expect

import "testing"

func TestEvalJSONPath_SimpleField_ReturnsValue(t *testing.T) {
	doc := map[string]any{"name": "alice"}
	vals, err := EvalJSONPath(doc, "$.name")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vals) != 1 || vals[0] != "alice" {
		t.Fatalf("expected [alice], got %v", vals)
	}
}

func TestEvalJSONPath_NestedField_ReturnsValue(t *testing.T) {
	doc := map[string]any{
		"user": map[string]any{
			"email": "a@b.com",
		},
	}
	vals, err := EvalJSONPath(doc, "$.user.email")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vals) != 1 || vals[0] != "a@b.com" {
		t.Fatalf("expected [a@b.com], got %v", vals)
	}
}

func TestEvalJSONPath_ArrayIndex_ReturnsElement(t *testing.T) {
	doc := map[string]any{
		"items": []any{"first", "second", "third"},
	}
	vals, err := EvalJSONPath(doc, "$.items[1]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vals) != 1 || vals[0] != "second" {
		t.Fatalf("expected [second], got %v", vals)
	}
}

func TestEvalJSONPath_ArrayWildcard_ReturnsAll(t *testing.T) {
	doc := map[string]any{
		"items": []any{
			map[string]any{"id": float64(1)},
			map[string]any{"id": float64(2)},
			map[string]any{"id": float64(3)},
		},
	}
	vals, err := EvalJSONPath(doc, "$.items[*].id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vals) != 3 {
		t.Fatalf("expected 3 results, got %d: %v", len(vals), vals)
	}
	for i, expected := range []float64{1, 2, 3} {
		if vals[i] != expected {
			t.Errorf("index %d: expected %v, got %v", i, expected, vals[i])
		}
	}
}

func TestEvalJSONPath_FilterExpression_FiltersCorrectly(t *testing.T) {
	doc := map[string]any{
		"items": []any{
			map[string]any{"status": "draft", "title": "one"},
			map[string]any{"status": "published", "title": "two"},
			map[string]any{"status": "draft", "title": "three"},
		},
	}
	vals, err := EvalJSONPath(doc, "$.items[?(@.status=='published')].title")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vals) != 1 || vals[0] != "two" {
		t.Fatalf("expected [two], got %v", vals)
	}
}

func TestEvalJSONPath_InvalidPath_ReturnsError(t *testing.T) {
	doc := map[string]any{"name": "test"}
	_, err := EvalJSONPath(doc, "name")
	if err == nil {
		t.Fatal("expected error for path without $ prefix")
	}
}

func TestCompareValues_EqualIntegers_ReturnsTrue(t *testing.T) {
	if !CompareValues(42, 42) {
		t.Fatal("expected equal integers to match")
	}
}

func TestCompareValues_FloatAndInt_ReturnsTrue(t *testing.T) {
	if !CompareValues(1, 1.0) {
		t.Fatal("expected int 1 to match float64 1.0")
	}
	if !CompareValues(1.0, 1) {
		t.Fatal("expected float64 1.0 to match int 1")
	}
}

func TestCompareValues_StringAndNumber_ReturnsFalse(t *testing.T) {
	if CompareValues("1", 1) {
		t.Fatal("expected string '1' to not match number 1")
	}
	if CompareValues(1, "1") {
		t.Fatal("expected number 1 to not match string '1'")
	}
}

func TestCompareValues_EqualStrings_ReturnsTrue(t *testing.T) {
	if !CompareValues("hello", "hello") {
		t.Fatal("expected equal strings to match")
	}
}

func TestCompareValues_NullValues_ReturnsTrue(t *testing.T) {
	if !CompareValues(nil, nil) {
		t.Fatal("expected nil to match nil")
	}
}
