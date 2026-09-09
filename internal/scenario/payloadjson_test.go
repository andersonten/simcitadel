package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePayload puts a payload file in a payloads directory and returns the
// directory that a scenario resolves a dataFile against.
func writePayload(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	payloads := filepath.Join(dir, "payloads")
	if err := os.MkdirAll(payloads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payloads, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return payloads
}

// TestCheckJSONValidity_TemplatedPayload_IsAccepted holds check 11 of SDD
// section 13 to the payload the run actually publishes. A payload substitutes a
// number, as the reference payload of section 6.4 does, so the raw file is not
// JSON until the template engine renders it. Judging the raw bytes rejects
// every payload the SDD itself writes.
func TestCheckJSONValidity_TemplatedPayload_IsAccepted(t *testing.T) {
	content := `{
  "specversion": "1.0",
  "type": "Article-Submitted",
  "source": "Journal",
  "id": "{{ uuid }}",
  "version": "v1.0.0",
  "subject": "/articles/{{ .articleId }}",
  "time": "{{ now }}",
  "data": {
    "articleId": {{ .articleId }},
    "spaceId": "{{ .spaceId }}"
  }
}`
	payloads := writePayload(t, "article.json", content)

	errs := checkJSONValidity("steps[0]", "dataFile", "article.json",
		filepath.Join(payloads, "article.json"))
	if len(errs) > 0 {
		t.Errorf("a templated payload must pass check 11, got: %v", errs)
	}
}

// TestCheckJSONValidity_MalformedPayload_IsRejected proves the check still
// earns its place. A missing comma is a real defect, and the template actions
// around it must not hide it.
func TestCheckJSONValidity_MalformedPayload_IsRejected(t *testing.T) {
	content := `{
  "id": "{{ uuid }}"
  "data": { "articleId": {{ .articleId }} }
}`
	payloads := writePayload(t, "broken.json", content)

	errs := checkJSONValidity("steps[0]", "dataFile", "broken.json",
		filepath.Join(payloads, "broken.json"))
	if len(errs) == 0 {
		t.Fatal("a payload with a missing comma must fail check 11")
	}
	if !strings.Contains(errs[0].Error(), "broken.json") {
		t.Errorf("the error must name the file, got %q", errs[0].Error())
	}
}

// TestCheckJSONValidity_UnbalancedBrace_IsRejected covers the other common
// hand-editing defect in a payload file.
func TestCheckJSONValidity_UnbalancedBrace_IsRejected(t *testing.T) {
	content := `{"id": "{{ uuid }}", "data": {"articleId": {{ .articleId }}}`
	payloads := writePayload(t, "unbalanced.json", content)

	if errs := checkJSONValidity("steps[0]", "dataFile", "unbalanced.json",
		filepath.Join(payloads, "unbalanced.json")); len(errs) == 0 {
		t.Fatal("a payload with an unbalanced brace must fail check 11")
	}
}

// TestCheckJSONValidity_PlainPayload_IsStillChecked keeps the behaviour for a
// payload that carries no template at all.
func TestCheckJSONValidity_PlainPayload_IsStillChecked(t *testing.T) {
	payloads := writePayload(t, "plain.json", `{"id": "fixed", "n": 1}`)
	if errs := checkJSONValidity("steps[0]", "dataFile", "plain.json",
		filepath.Join(payloads, "plain.json")); len(errs) > 0 {
		t.Errorf("a plain valid payload must pass, got: %v", errs)
	}

	bad := writePayload(t, "plainbad.json", `{"id": "fixed",}`)
	if errs := checkJSONValidity("steps[0]", "dataFile", "plainbad.json",
		filepath.Join(bad, "plainbad.json")); len(errs) == 0 {
		t.Error("a plain payload with a trailing comma must fail")
	}
}

// TestCheckJSONValidity_UnparseableTemplate_IsRejected covers a payload whose
// template does not compile. It never renders at run time, so validate is the
// right place to say so.
func TestCheckJSONValidity_UnparseableTemplate_IsRejected(t *testing.T) {
	payloads := writePayload(t, "badtmpl.json", `{"id": "{{ uuid "}}"}`)

	errs := checkJSONValidity("steps[0]", "dataFile", "badtmpl.json",
		filepath.Join(payloads, "badtmpl.json"))
	if len(errs) == 0 {
		t.Fatal("a payload whose template does not parse must fail validation")
	}
}
