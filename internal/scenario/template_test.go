package scenario

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRenderString_SimpleVariable_Substitutes(t *testing.T) {
	vars := map[string]any{"articleId": "12345"}
	got, err := RenderString("ID is {{ .articleId }}", vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "ID is 12345"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderString_UUIDFunction_ReturnsUUID(t *testing.T) {
	got, err := RenderString("{{ uuid }}", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidPattern.MatchString(got) {
		t.Fatalf("got %q, which does not match UUIDv4 pattern", got)
	}
}

func TestRenderString_NowFunction_ReturnsTimestamp(t *testing.T) {
	before := time.Now().UTC().Add(-1 * time.Second)
	got, err := RenderString("{{ now }}", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := time.Now().UTC().Add(1 * time.Second)

	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", got, err)
	}
	if parsed.Before(before) || parsed.After(after) {
		t.Fatalf("timestamp %q is outside the expected window", got)
	}
}

func TestRenderString_NowOffsetFunction_ReturnsOffsetTimestamp(t *testing.T) {
	got, err := RenderString(`{{ nowOffset "-1h" }}`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", got, err)
	}
	expected := time.Now().UTC().Add(-1 * time.Hour)
	diff := parsed.Sub(expected)
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("timestamp %q differs from expected by %v", got, diff)
	}
}

func TestRenderString_DefaultFunction_UsesDefaultWhenEmpty(t *testing.T) {
	vars := map[string]any{"x": ""}
	got, err := RenderString(`{{ default .x 42 }}`, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "42" {
		t.Fatalf("got %q, want %q", got, "42")
	}
}

func TestRenderString_DefaultFunction_UsesValueWhenSet(t *testing.T) {
	vars := map[string]any{"x": "hello"}
	got, err := RenderString(`{{ default .x "fallback" }}`, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

func TestRenderString_UnresolvedVariable_ReturnsError(t *testing.T) {
	_, err := RenderString("{{ .missing }}", map[string]any{})
	if err == nil {
		t.Fatal("expected an error for an unresolved variable")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error %q does not mention the missing variable", err.Error())
	}
}

func TestRenderString_NoTemplate_ReturnsOriginal(t *testing.T) {
	got, err := RenderString("plain text", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "plain text" {
		t.Fatalf("got %q, want %q", got, "plain text")
	}
}

func TestRender_RendersStepFields(t *testing.T) {
	s := &Scenario{
		Name:      "test-{{ .env }}",
		Variables: map[string]any{"env": "local"},
		Steps: []Step{
			{
				Name:       "Publish to {{ .env }}",
				Type:       "event/publish",
				Exchange:   "{{ .env }}.exchange",
				RoutingKey: "key",
				Data:       "payload",
			},
		},
	}
	vars := map[string]any{"env": "local"}
	rendered, err := Render(s, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rendered.Name != "test-local" {
		t.Fatalf("name: got %q, want %q", rendered.Name, "test-local")
	}
	if rendered.Steps[0].Name != "Publish to local" {
		t.Fatalf("steps[0].name: got %q", rendered.Steps[0].Name)
	}
	if rendered.Steps[0].Exchange != "local.exchange" {
		t.Fatalf("steps[0].exchange: got %q", rendered.Steps[0].Exchange)
	}
}

func TestRender_DoesNotMutateOriginal(t *testing.T) {
	s := &Scenario{
		Name: "test-{{ .env }}",
		Steps: []Step{
			{
				Name: "Step {{ .env }}",
				Type: "wait",
			},
		},
	}
	vars := map[string]any{"env": "prod"}
	_, err := Render(s, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Name != "test-{{ .env }}" {
		t.Fatalf("original scenario name was mutated to %q", s.Name)
	}
}

func TestRenderString_UUIDUniqueness(t *testing.T) {
	first, err := RenderString("{{ uuid }}", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := RenderString("{{ uuid }}", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first == second {
		t.Fatalf("two uuid calls returned the same value: %q", first)
	}
}

func TestRenderString_ForbiddenControlFlow_ReturnsError(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
	}{
		{"if directive", "{{ if .x }}yes{{ end }}"},
		{"range directive", "{{ range .items }}item{{ end }}"},
		{"with directive", "{{ with .x }}val{{ end }}"},
		{"eq function", "{{ eq .a .b }}"},
		{"ne function", "{{ ne .a .b }}"},
		{"lt function", "{{ lt .a .b }}"},
		{"gt function", "{{ gt .a .b }}"},
		{"le function", "{{ le .a .b }}"},
		{"ge function", "{{ ge .a .b }}"},
		{"and function", "{{ and .a .b }}"},
		{"or function", "{{ or .a .b }}"},
		{"not function", "{{ not .a }}"},
		{"call function", "{{ call .fn }}"},
		{"print function", "{{ print .a }}"},
		{"printf function", `{{ printf "%s" .a }}`},
		{"println function", "{{ println .a }}"},
		{"html function", "{{ html .a }}"},
		{"js function", "{{ js .a }}"},
		{"urlquery function", "{{ urlquery .a }}"},
		{"slice function", "{{ slice .a 1 }}"},
		{"index function", "{{ index .a 0 }}"},
		{"len function", "{{ len .a }}"},
		{"else directive", "{{ if .x }}y{{else}}n{{ end }}"},
		{"whitespace before if", "{{  if .x }}yes{{ end }}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RenderString(tc.tmpl, map[string]any{"x": "1", "a": "1", "b": "2", "items": []string{"a"}, "fn": "f"})
			if err == nil {
				t.Fatal("expected an error for a forbidden directive")
			}
			if !strings.Contains(err.Error(), "forbidden directive") {
				t.Fatalf("error %q does not mention forbidden directive", err.Error())
			}
		})
	}
}

func TestRenderString_AllowedFunctions_NoError(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
	}{
		{"uuid", "{{ uuid }}"},
		{"now", "{{ now }}"},
		{"nowOffset", `{{ nowOffset "-1h" }}`},
		{"default", `{{ default .x "fallback" }}`},
		{"variable", "{{ .x }}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RenderString(tc.tmpl, map[string]any{"x": "val"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
