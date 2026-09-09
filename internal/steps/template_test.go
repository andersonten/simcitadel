package steps

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file locks down the file-backed templating of SDD section 6.4. These
// tests began as a proof that a payload file, a SQL file, a GraphQL query file
// and an HTTP body file all reached their destination unrendered. A payload
// that substitutes a number is not valid JSON before the render, so the
// dataFile form of every reference scenario could not run at all.

// writeContentFile writes a content file under dir/sub and returns the
// directory that holds it.
func writeContentFile(t *testing.T, dir, sub, name, body string) string {
	t.Helper()
	contentDir := filepath.Join(dir, sub)
	if err := os.MkdirAll(contentDir, 0o750); err != nil {
		t.Fatalf("create %s: %v", contentDir, err)
	}
	if err := os.WriteFile(filepath.Join(contentDir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return contentDir
}

// contentRunContext returns a run context whose content directories point at
// dir, so a step reads the file this test wrote.
func contentRunContext(t *testing.T, dir string, targets config.TargetsConfig) *RunContext {
	t.Helper()
	return &RunContext{
		Config: &config.CitadelConfig{
			Targets: targets,
			Paths: config.PathsConfig{
				Payloads: filepath.Join(dir, "payloads"),
				SQL:      filepath.Join(dir, "sql"),
				Queries:  filepath.Join(dir, "queries"),
			},
		},
		RunID:      "run-1",
		HTTPClient: http.DefaultClient,
		Variables:  map[string]any{"articleId": "515098", "spaceId": "s-1"},
		Bindings:   map[string]any{},
		DBs:        map[string]*sql.DB{},
	}
}

// TestExecute_PublishDataFileWithTemplate_RendersBeforeParse holds the shape of
// every CloudEvents payload in the reference pack: the payload substitutes a
// number, so the file is not valid JSON until it is rendered.
func TestExecute_PublishDataFileWithTemplate_RendersBeforeParse(t *testing.T) {
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"eventId": "e-1"})
	}))
	defer srv.Close()

	dir := t.TempDir()
	writeContentFile(t, dir, "payloads", "article-submitted.v1.0.0.json",
		`{"specversion":"1.0","type":"Article-Submitted","source":"Journal","id":"{{ uuid }}",`+
			`"version":"v1.0.0","subject":"/articles/{{ .articleId }}",`+
			`"data":{"articleId":{{ .articleId }},"spaceId":"{{ .spaceId }}"}}`)

	rc := contentRunContext(t, dir, config.TargetsConfig{
		EventTrigger: &config.EventTriggerTarget{URL: srv.URL},
	})
	rc.HTTPClient = srv.Client()

	step := &scenario.Step{
		Name:       "publish the submitted article",
		Type:       "event/publish",
		Exchange:   "public.a.v1",
		RoutingKey: "article-submitted",
		DataFile:   "article-submitted.v1.0.0.json",
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the publish step failed: %v", result.Err)
	}
	if strings.Contains(string(sent), "{{") {
		t.Errorf("the published body still holds a template directive: %s", sent)
	}

	var payload struct {
		Event map[string]any `json:"event"`
	}
	if err := json.Unmarshal(sent, &payload); err != nil {
		t.Fatalf("parse the published body %q: %v", sent, err)
	}
	data, ok := payload.Event["data"].(map[string]any)
	if !ok {
		t.Fatalf("event.data is %T, want a map", payload.Event["data"])
	}
	if data["articleId"] != float64(515098) {
		t.Errorf("event.data.articleId = %v, want the number 515098", data["articleId"])
	}
	if id, _ := payload.Event["id"].(string); id == "" || strings.Contains(id, "{{") {
		t.Errorf("event.id = %q, want a rendered uuid", id)
	}
}

// TestResolvePublishData_UUIDInDataFile_DiffersBetweenRenders keeps a later
// change from caching the rendered file. Every render of uuid gives a fresh
// value, as the function table of SDD section 6.4 states.
func TestResolvePublishData_UUIDInDataFile_DiffersBetweenRenders(t *testing.T) {
	dir := t.TempDir()
	writeContentFile(t, dir, "payloads", "p.json", `{"id":"{{ uuid }}","articleId":{{ .articleId }}}`)

	rc := contentRunContext(t, dir, config.TargetsConfig{})
	step := &scenario.Step{Name: "publish", Type: "event/publish", DataFile: "p.json"}

	ids := make([]string, 2)
	for i := range ids {
		data, err := resolvePublishData(step, rc)
		if err != nil {
			t.Fatalf("resolvePublishData: %v", err)
		}
		body, ok := data.(map[string]any)
		if !ok {
			t.Fatalf("payload is %T, want a map", data)
		}
		ids[i], _ = body["id"].(string)
		if ids[i] == "" {
			t.Fatalf("payload id is empty")
		}
	}
	if ids[0] == ids[1] {
		t.Errorf("two renders gave the same uuid %q, so the file is cached", ids[0])
	}
}

// TestExecuteProbe_HTTPBodyFileWithTemplate_RendersBeforeSend holds the
// bodyFile form of the http source of SDD section 6.5.
func TestExecuteProbe_HTTPBodyFileWithTemplate_RendersBeforeSend(t *testing.T) {
	var sent []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	dir := t.TempDir()
	writeContentFile(t, dir, "payloads", "body.json", `{"articleId":{{ .articleId }},"spaceId":"{{ .spaceId }}"}`)

	rc := contentRunContext(t, dir, config.TargetsConfig{App: &config.AppTarget{URL: srv.URL}})
	rc.HTTPClient = srv.Client()

	step := &scenario.Step{
		Name:   "post the article body",
		Type:   "app/http",
		HTTP:   &scenario.HTTPSource{Method: http.MethodPost, Path: "/articles", BodyFile: "body.json"},
		Expect: &scenario.Expect{Status: 200},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the probe failed: %v", result.Expect)
	}
	if strings.Contains(string(sent), "{{") {
		t.Errorf("the request body still holds a template directive: %s", sent)
	}
	if !strings.Contains(string(sent), "515098") {
		t.Errorf("the request body %q does not hold the substituted article id", sent)
	}
}

// TestExecuteProbe_GraphQLQueryFileWithTemplate_RendersBeforeSend holds the
// queryFile form of the graphql source of SDD section 6.5.
func TestExecuteProbe_GraphQLQueryFileWithTemplate_RendersBeforeSend(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		query, _ = body["query"].(string)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"ok": true}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	writeContentFile(t, dir, "queries", "pe-tasks.graphql", `query { pe_tasks(subjectId: "{{ .articleId }}") { status } }`)

	rc := contentRunContext(t, dir, config.TargetsConfig{
		App: &config.AppTarget{URL: srv.URL, GraphQLPath: "/graphql"},
	})
	rc.HTTPClient = srv.Client()

	step := &scenario.Step{
		Name: "read the tasks of the article",
		Type: "app/graphql",
		GraphQL: &scenario.GraphQLSource{
			QueryFile: "pe-tasks.graphql",
		},
		Expect: &scenario.Expect{Status: 200},
	}

	result, err := Execute(context.Background(), step, rc)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Pass {
		t.Fatalf("the probe failed: %v", result.Expect)
	}
	if strings.Contains(query, "{{") {
		t.Errorf("the sent query still holds a template directive: %s", query)
	}
	if !strings.Contains(query, "515098") {
		t.Errorf("the sent query %q does not hold the substituted article id", query)
	}
}

// TestExecuteSQLExec_FileWithTemplate_RendersBeforeRun holds the SQL file form
// of SDD section 6.5 and the external SQL rule of CONTRIBUTING.md.
func TestExecuteSQLExec_FileWithTemplate_RendersBeforeRun(t *testing.T) {
	recorder := &queryRecorder{}
	handle := openRecordingDB(t, recorder)

	dir := t.TempDir()
	writeContentFile(t, dir, "sql", "seed.sql", "insert into article (id, space_id) values ({{ .articleId }}, '{{ .spaceId }}');")

	rc := contentRunContext(t, dir, config.TargetsConfig{})
	rc.DBs["app-db"] = handle

	step := &scenario.Step{
		Name:   "seed the article",
		Type:   "sql/exec",
		Target: "app-db",
		File:   "seed.sql",
	}

	if _, err := Execute(context.Background(), step, rc); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ran := recorder.all()
	if len(ran) != 1 {
		t.Fatalf("the step ran %d statements, want 1", len(ran))
	}
	if strings.Contains(ran[0], "{{") {
		t.Errorf("the statement that ran still holds a template directive: %s", ran[0])
	}
	if !strings.Contains(ran[0], "515098") {
		t.Errorf("the statement %q does not hold the substituted article id", ran[0])
	}
}

// queryRecorder records every statement a test database prepared.
type queryRecorder struct {
	mu      sync.Mutex
	queries []string
}

func (q *queryRecorder) add(query string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries = append(q.queries, query)
}

func (q *queryRecorder) all() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.queries...)
}

type recordingDriver struct{ recorder *queryRecorder }

func (d recordingDriver) Open(string) (driver.Conn, error) {
	return recordingConn{recorder: d.recorder}, nil
}

type recordingConn struct{ recorder *queryRecorder }

func (c recordingConn) Prepare(query string) (driver.Stmt, error) {
	c.recorder.add(query)
	return recordingStmt{}, nil
}
func (c recordingConn) Close() error              { return nil }
func (c recordingConn) Begin() (driver.Tx, error) { return nil, io.EOF }

type recordingStmt struct{}

func (recordingStmt) Close() error  { return nil }
func (recordingStmt) NumInput() int { return -1 }
func (recordingStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (recordingStmt) Query([]driver.Value) (driver.Rows, error) {
	return &stubRows{cols: []string{"id"}}, nil
}

// openRecordingDB registers a driver that records every statement it prepares.
func openRecordingDB(t *testing.T, recorder *queryRecorder) *sql.DB {
	t.Helper()
	name := "citadel-recorder-" + strings.ReplaceAll(t.Name(), "/", "-")
	sql.Register(name, recordingDriver{recorder: recorder})
	handle, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open recording database: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	return handle
}
