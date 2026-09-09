package scenario

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
)

// validStepTypes lists every step type that citadel accepts.
var validStepTypes = map[string]bool{
	"event/publish":      true,
	"event/republish":    true,
	"wiremock/set-state": true,
	"sql/exec":           true,
	"sql/query":          true,
	"probe":              true,
	"await":              true,
	"app/graphql":        true,
	"app/http":           true,
	"event/captured":     true,
	"inbox/assert":       true,
	"event/not-captured": true,
	"wait":               true,
}

// templateRefPattern matches template variable references such as {{ .varName }}.
var templateRefPattern = regexp.MustCompile(`\{\{\s*\.([a-zA-Z_][a-zA-Z0-9_]*(?:\.[a-zA-Z_][a-zA-Z0-9_]*)*)`)

// Options carries what the checks of SDD section 13 need beyond the scenario
// itself.
type Options struct {
	// BasePath is the directory that resolves a referenced file. An empty value
	// skips every file existence check.
	BasePath string
	// Config supplies the resolved target URLs that check 6 compares against. A
	// nil value skips that check.
	Config *config.CitadelConfig
}

// Validate checks a parsed scenario for correctness without contacting any service.
// Validate returns all errors found, not just the first.
// The basePath parameter sets the directory for resolving referenced files.
// Pass an empty string to skip file existence checks.
func Validate(s *Scenario, basePath string) []error {
	return ValidateWithOptions(s, Options{BasePath: basePath})
}

// ValidateWithOptions runs every check of SDD section 13 that the given options
// make possible. Use it in place of Validate when the configuration is loaded,
// because check 6 compares resolved target URLs and cannot run without it.
func ValidateWithOptions(s *Scenario, opts Options) []error {
	var errs []error

	errs = append(errs, validateStepTypes(s)...)
	errs = append(errs, validateRequiredFields(s)...)
	errs = append(errs, validateProbeSources(s)...)
	errs = append(errs, validateExpectations(s)...)
	if opts.BasePath != "" {
		errs = append(errs, validateFileRefs(s, fileResolver{basePath: opts.BasePath, cfg: opts.Config})...)
	}
	if dir := stubDir(opts); dir != "" {
		errs = append(errs, validateWireMockStubs(dir)...)
		errs = append(errs, validateSetStateAgainstStubs(s, dir)...)
	}
	errs = append(errs, validateAsBindings(s)...)
	errs = append(errs, validateMessageRefs(s)...)
	errs = append(errs, validateRetrySpecs(s)...)
	errs = append(errs, validateTemplateRefs(s)...)
	errs = append(errs, validateNoWireMockProbes(s, opts.Config)...)

	return errs
}

// stubDir returns the directory that checks 8 and 9 of SDD section 13 read.
//
// The pack lives under paths.wiremock, which is the only place a stub comes
// from, so the checks read it whenever the configuration is loaded. Both call
// sites pass the scenario file's own directory as the base path, and under the
// layout of section 5.1 that directory holds no stub at all, so a base path
// scan makes the two checks skip every run.
//
// Without a configuration the base path is the fallback, because `citadel
// validate` runs without one.
func stubDir(opts Options) string {
	if opts.Config != nil {
		return opts.Config.WireMockDir()
	}
	return opts.BasePath
}

// probeTypes lists the step types of SDD section 6.2 that read a source and
// compare, either directly or as a named form over probe.
var probeTypes = map[string]bool{
	"probe":          true,
	"await":          true,
	"sql/query":      true,
	"app/graphql":    true,
	"app/http":       true,
	"event/captured": true,
	"inbox/assert":   true,
}

// validateProbeSources holds the first sentence of SDD section 6.5: a probe has
// exactly one source. The executor reads one source, so a second one is
// dropped without a word, and the assertion then reports on something the
// author did not write.
func validateProbeSources(s *Scenario) []error {
	var errs []error
	eachStep(s, func(phase string, i int, step *Step) {
		if !probeTypes[step.Type] {
			return
		}
		var named []string
		for _, source := range []struct {
			name    string
			present bool
		}{
			{"graphql", step.GraphQL != nil},
			{"http", step.HTTP != nil},
			{"sql", step.SQL != nil},
			{"captured", step.Captured != nil},
			{"journal", step.Journal != nil},
		} {
			if source.present {
				named = append(named, source.name)
			}
		}
		// A sql/query writes its statement at the step level, which is the sql
		// source in its named form.
		if step.Type == "sql/query" && (step.Query != "" || step.File != "") {
			named = append(named, "query")
		}
		if len(named) > 1 {
			errs = append(errs, fmt.Errorf(
				"%s[%d] %q: a probe takes exactly one source, and this step names %s",
				phase, i, step.Name, strings.Join(named, ", ")))
		}
	})
	return errs
}

// graphQLErrorsValues is the closed value set of the graphqlErrors row of SDD
// section 6.5.
var graphQLErrorsValues = map[string]bool{"none": true, "any": true}

// validateExpectations holds an expect block to the expectation table of SDD
// section 6.5, which the section calls the complete list of keys.
//
// A block that names no key from the table asserts nothing, and the evaluator
// reports a pass, so a failing system reports a pass. An equals with no
// jsonPath is the same defect in its sharpest form, because equals is half of
// one key and does nothing on its own.
func validateExpectations(s *Scenario) []error {
	var errs []error
	eachStep(s, func(phase string, i int, step *Step) {
		e := step.Expect
		if e == nil {
			return
		}
		prefix := fmt.Sprintf("%s[%d] %q", phase, i, step.Name)

		if e.GraphQLErrors != "" && !graphQLErrorsValues[e.GraphQLErrors] {
			errs = append(errs, fmt.Errorf(
				"%s: expect.graphqlErrors must be \"none\" or \"any\", got %q",
				prefix, e.GraphQLErrors))
		}

		_, pathIsString := e.JSONPath.(string)
		if e.Equals != nil && !pathIsString {
			errs = append(errs, fmt.Errorf(
				"%s: expect.equals needs a jsonPath string, so on its own it asserts nothing",
				prefix))
		}

		if !hasExpectationKey(e) {
			errs = append(errs, fmt.Errorf(
				"%s: expect names no key of the table of SDD section 6.5, so it asserts nothing",
				prefix))
		}
	})
	return errs
}

// hasExpectationKey reports whether the block names at least one key that the
// evaluator acts on.
func hasExpectationKey(e *Expect) bool {
	return e.Status != 0 ||
		e.JSONPath != nil ||
		e.JSONPathExists != "" ||
		e.Rows != nil ||
		e.Count != nil ||
		e.CountAtLeast != nil ||
		len(e.Columns) > 0 ||
		len(e.NotNull) > 0 ||
		e.CloudEventsValid ||
		e.GraphQLErrors != ""
}

// eachStep calls fn for every step of every phase, in execution order.
func eachStep(s *Scenario, fn func(phase string, index int, step *Step)) {
	for _, list := range []struct {
		phase string
		steps []Step
	}{
		{"steps", s.Steps},
		{"assertions", s.Assertions},
		{"after", s.After},
	} {
		for i := range list.steps {
			fn(list.phase, i, &list.steps[i])
		}
	}
}

// validateMessageRefs implements the second half of check 5 of SDD section 13:
// every ref: names a prior as:. A ref to a later binding, or to none, resolves
// to nothing at run time and produces a confusing empty message.
func validateMessageRefs(s *Scenario) []error {
	var errs []error
	bound := make(map[string]bool)

	eachStep(s, func(phase string, i int, step *Step) {
		if step.Ref != "" && !bound[step.Ref] {
			errs = append(errs, fmt.Errorf("%s[%d] %q: ref %q does not name a prior as: binding",
				phase, i, step.Name, step.Ref))
		}
		if step.As != "" {
			bound[step.As] = true
		}
	})

	return errs
}

// validateRetrySpecs holds retry.until to the one value SDD section 6.3 defines
// for Milestone 1. An unknown value would otherwise be accepted and ignored.
func validateRetrySpecs(s *Scenario) []error {
	var errs []error

	eachStep(s, func(phase string, i int, step *Step) {
		// Every duration a step can carry is checked here. A value that
		// reaches an executor unchecked takes a default instead, and for
		// quietPeriod that shortens the window of SDD section 8.3 and turns a
		// forbidden message into an all-clear.
		for _, field := range []struct{ name, value string }{
			{"timeout", step.Timeout},
			{"quietPeriod", step.QuietPeriod},
			{"duration", step.Duration},
		} {
			if field.value == "" {
				continue
			}
			if _, err := time.ParseDuration(field.value); err != nil {
				errs = append(errs, fmt.Errorf("%s[%d] %q: %s %q is not a duration",
					phase, i, step.Name, field.name, field.value))
			}
		}
		if step.Retry == nil {
			return
		}
		if step.Retry.Until != "" && step.Retry.Until != "pass" {
			errs = append(errs, fmt.Errorf("%s[%d] %q: retry.until must be \"pass\", got %q",
				phase, i, step.Name, step.Retry.Until))
		}
		for _, field := range []struct{ name, value string }{
			{"retry.timeout", step.Retry.Timeout},
			{"retry.interval", step.Retry.Interval},
		} {
			if field.value == "" {
				continue
			}
			if _, err := time.ParseDuration(field.value); err != nil {
				errs = append(errs, fmt.Errorf("%s[%d] %q: %s %q is not a duration",
					phase, i, step.Name, field.name, field.value))
			}
		}
	})

	return errs
}

// validateNoWireMockProbes implements check 6 of SDD section 13: no
// assertion and no probe targets WireMock. The check applies to every phase,
// because a probe against the mock proves nothing about the application
// whichever list holds it.
// The check compares resolved addresses first, so a target with another name
// that points at WireMock cannot defeat it. The target name is the second line
// of defence, for the case where no config is supplied.
// A journal source reads the WireMock request journal by design, so only a
// graphql source and an http source count here.
func validateNoWireMockProbes(s *Scenario, cfg *config.CitadelConfig) []error {
	mockHosts := wireMockHosts(cfg)

	var errs []error
	eachStep(s, func(phase string, i int, step *Step) {
		if step.GraphQL == nil && step.HTTP == nil {
			return
		}
		// A step that names no target reaches targets.app, because that is
		// where the runner sends every graphql and http probe.
		name := step.Target
		if name == "" {
			name = "app"
		}
		if host := targetHost(cfg, name); host != "" && mockHosts[host] {
			errs = append(errs, fmt.Errorf(
				"%s[%d] %q: target %q resolves to the WireMock address %s, so this probe would assert against the mock",
				phase, i, step.Name, name, host))
			return
		}
		if step.Target == "wiremock" {
			errs = append(errs, fmt.Errorf(
				"%s[%d] %q: target %q is the mock, so this probe would assert against the mock",
				phase, i, step.Name, step.Target))
		}
	})
	return errs
}

// wireMockHosts returns the resolved hosts of the WireMock target.
func wireMockHosts(cfg *config.CitadelConfig) map[string]bool {
	hosts := map[string]bool{}
	if cfg == nil || cfg.Targets.WireMock == nil {
		return hosts
	}
	for _, raw := range []string{cfg.Targets.WireMock.URL, cfg.Targets.WireMock.AdminURL} {
		if host := hostOf(raw); host != "" {
			hosts[host] = true
		}
	}
	return hosts
}

// targetHost returns the resolved host of the target with the given name. It
// returns an empty string when no config is supplied, when the config omits the
// target, or when the target holds no URL.
func targetHost(cfg *config.CitadelConfig, name string) string {
	if cfg == nil {
		return ""
	}
	t := cfg.Targets
	switch name {
	case "app":
		if t.App != nil {
			return hostOf(t.App.URL)
		}
	case "wiremock":
		if t.WireMock != nil {
			return hostOf(t.WireMock.URL)
		}
	case "event-trigger":
		if t.EventTrigger != nil {
			return hostOf(t.EventTrigger.URL)
		}
	case "event-capture":
		if t.EventCapture != nil {
			return hostOf(t.EventCapture.URL)
		}
	case "debezium-outbox":
		if t.DebeziumOutbox != nil {
			return hostOf(t.DebeziumOutbox.URL)
		}
	}
	return ""
}

// hostOf returns the host and port of a URL, or an empty string when the value
// is not a URL.
func hostOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// validateStepTypes checks that every step type is in the closed set.
func validateStepTypes(s *Scenario) []error {
	var errs []error
	for _, list := range []struct {
		phase string
		steps []Step
	}{
		{"steps", s.Steps},
		{"assertions", s.Assertions},
		{"after", s.After},
	} {
		for i, step := range list.steps {
			if !validStepTypes[step.Type] {
				errs = append(errs, fmt.Errorf("%s[%d] %q: unknown step type %q", list.phase, i, step.Name, step.Type))
			}
		}
	}
	return errs
}

// validateRequiredFields checks that each step type carries its required fields.
func validateRequiredFields(s *Scenario) []error {
	var errs []error
	for _, list := range []struct {
		phase string
		steps []Step
	}{
		{"steps", s.Steps},
		{"assertions", s.Assertions},
		{"after", s.After},
	} {
		for i, step := range list.steps {
			if step.Type != "" && step.Name == "" {
				errs = append(errs, fmt.Errorf("%s[%d]: step name is required", list.phase, i))
			}
			prefix := fmt.Sprintf("%s[%d] %q", list.phase, i, step.Name)
			errs = append(errs, validateStepFields(prefix, &step)...)
			if list.phase == "assertions" && (step.Target == "wiremock" || strings.HasPrefix(step.Type, "wiremock/")) {
				errs = append(errs, fmt.Errorf("%s: assertions must not target wiremock", prefix))
			}
		}
	}
	return errs
}

// validateStepFields checks that a single step carries the required fields for its type.
func validateStepFields(prefix string, step *Step) []error {
	var errs []error
	switch step.Type {
	case "event/publish":
		if step.Exchange == "" {
			errs = append(errs, fmt.Errorf("%s: event/publish requires exchange", prefix))
		}
		if step.RoutingKey == "" {
			errs = append(errs, fmt.Errorf("%s: event/publish requires routingKey", prefix))
		}
		if step.DataFile == "" && step.Data == nil {
			errs = append(errs, fmt.Errorf("%s: event/publish requires dataFile or data", prefix))
		}
	case "event/republish":
		// A republish re-sends a stored message, and ref: is the only way to
		// name it, so the step cannot work without one.
		if step.Ref == "" {
			errs = append(errs, fmt.Errorf("%s: event/republish requires ref", prefix))
		}
		if step.Exchange == "" {
			errs = append(errs, fmt.Errorf("%s: event/republish requires exchange", prefix))
		}
		if step.RoutingKey == "" {
			errs = append(errs, fmt.Errorf("%s: event/republish requires routingKey", prefix))
		}
	case "wiremock/set-state":
		if step.ScenarioName == "" {
			errs = append(errs, fmt.Errorf("%s: wiremock/set-state requires scenarioName", prefix))
		}
		if step.State == "" {
			errs = append(errs, fmt.Errorf("%s: wiremock/set-state requires state", prefix))
		}
	case "sql/exec":
		if step.Target == "" {
			errs = append(errs, fmt.Errorf("%s: sql/exec requires target", prefix))
		}
		if step.File == "" && step.Query == "" {
			errs = append(errs, fmt.Errorf("%s: sql/exec requires file or query", prefix))
		}
	case "sql/query":
		if step.Target == "" {
			errs = append(errs, fmt.Errorf("%s: sql/query requires target", prefix))
		}
		if step.File == "" && step.Query == "" {
			errs = append(errs, fmt.Errorf("%s: sql/query requires file or query", prefix))
		}
		if step.Expect == nil {
			errs = append(errs, fmt.Errorf("%s: sql/query requires expect", prefix))
		}
	case "probe":
		if !hasProbeSource(step) {
			errs = append(errs, fmt.Errorf("%s: probe requires at least one source (graphql, http, sql, captured, or journal)", prefix))
		}
		if step.Expect == nil {
			errs = append(errs, fmt.Errorf("%s: probe requires expect", prefix))
		}
	case "await":
		if !hasProbeSource(step) {
			errs = append(errs, fmt.Errorf("%s: await requires at least one source (graphql, http, sql, captured, or journal)", prefix))
		}
		if step.Expect == nil {
			errs = append(errs, fmt.Errorf("%s: await requires expect", prefix))
		}
	case "app/graphql":
		if step.GraphQL == nil {
			errs = append(errs, fmt.Errorf("%s: app/graphql requires graphql source", prefix))
		}
		if step.Expect == nil {
			errs = append(errs, fmt.Errorf("%s: app/graphql requires expect", prefix))
		}
	case "app/http":
		if step.HTTP == nil {
			errs = append(errs, fmt.Errorf("%s: app/http requires http source", prefix))
		}
		if step.Expect == nil {
			errs = append(errs, fmt.Errorf("%s: app/http requires expect", prefix))
		}
	case "event/captured":
		if step.Captured == nil && step.Exchange == "" {
			errs = append(errs, fmt.Errorf("%s: event/captured requires captured source or exchange", prefix))
		}
		if step.Expect == nil {
			errs = append(errs, fmt.Errorf("%s: event/captured requires expect", prefix))
		}
	case "inbox/assert":
		if step.Target == "" {
			errs = append(errs, fmt.Errorf("%s: inbox/assert requires target", prefix))
		}
		if len(step.Where) == 0 {
			errs = append(errs, fmt.Errorf("%s: inbox/assert requires where", prefix))
		}
		if step.Expect == nil {
			errs = append(errs, fmt.Errorf("%s: inbox/assert requires expect", prefix))
		}
	case "event/not-captured":
		if !hasNotCapturedSelector(step) {
			errs = append(errs, fmt.Errorf("%s: event/not-captured requires at least one selector (exchange, routingKey, routingKeyPattern, eventType, eventTypePattern, jsonPath, or headerFilter)", prefix))
		}
	case "wait":
		if step.Duration == "" {
			errs = append(errs, fmt.Errorf("%s: wait requires duration", prefix))
		}
		if step.Reason == "" {
			errs = append(errs, fmt.Errorf("%s: wait requires reason", prefix))
		}
	}
	return errs
}

// hasProbeSource returns true if the step has at least one probe source.
func hasProbeSource(step *Step) bool {
	return step.GraphQL != nil || step.HTTP != nil || step.SQL != nil || step.Captured != nil || step.Journal != nil
}

// hasNotCapturedSelector returns true if the step has at least one selector
// that narrows which message the assertion forbids.
//
// A scenario can write a selector at the step level or inside the captured
// block, and the executor reads both shapes, so validation must accept both. A
// run ID is not a selector: it scopes the query to the run and narrows nothing
// about which message is forbidden.
func hasNotCapturedSelector(step *Step) bool {
	if step.Exchange != "" ||
		step.RoutingKey != "" ||
		step.EventType != "" ||
		step.EventTypePattern != "" ||
		len(step.HeaderFilter) > 0 {
		return true
	}

	c := step.Captured
	if c == nil {
		return false
	}
	return c.Exchange != "" ||
		c.RoutingKey != "" ||
		c.RoutingKeyPattern != "" ||
		c.EventType != "" ||
		c.EventTypePattern != "" ||
		c.JSONPath != "" ||
		len(c.Headers) > 0
}

// validateFileRefs checks that every referenced file exists on disk.
// It also checks that JSON payload files contain valid JSON and that
// GraphQL query files start with a recognized operation keyword.
// fileResolver turns a file a scenario names into a path on disk. It uses the
// content directories of SDD section 5.1 when the config is loaded, so check 3
// looks in the same place the runner reads from. Without a config it falls back
// to the scenario's own directory.
type fileResolver struct {
	basePath string
	cfg      *config.CitadelConfig
}

// payload resolves an event payload or an HTTP body file.
func (r fileResolver) payload(file string) string {
	if r.cfg != nil {
		return r.cfg.PayloadPath(file)
	}
	return r.fallback(file)
}

// sql resolves a SQL file.
func (r fileResolver) sql(file string) string {
	if r.cfg != nil {
		return r.cfg.SQLPath(file)
	}
	return r.fallback(file)
}

// query resolves a GraphQL query file.
func (r fileResolver) query(file string) string {
	if r.cfg != nil {
		return r.cfg.QueryPath(file)
	}
	return r.fallback(file)
}

func (r fileResolver) fallback(file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	return filepath.Join(r.basePath, file)
}

func validateFileRefs(s *Scenario, resolve fileResolver) []error {
	var errs []error
	eachStep(s, func(phase string, i int, step *Step) {
		prefix := fmt.Sprintf("%s[%d] %q", phase, i, step.Name)
		if step.DataFile != "" {
			path := resolve.payload(step.DataFile)
			errs = append(errs, checkFileExists(prefix, "dataFile", step.DataFile, path)...)
			errs = append(errs, checkJSONValidity(prefix, "dataFile", step.DataFile, path)...)
		}
		if step.File != "" {
			errs = append(errs, checkFileExists(prefix, "file", step.File, resolve.sql(step.File))...)
		}
		if step.GraphQL != nil && step.GraphQL.QueryFile != "" {
			path := resolve.query(step.GraphQL.QueryFile)
			errs = append(errs, checkFileExists(prefix, "queryFile", step.GraphQL.QueryFile, path)...)
			errs = append(errs, checkGraphQLSyntax(prefix, "queryFile", step.GraphQL.QueryFile, path)...)
		}
		if step.HTTP != nil && step.HTTP.BodyFile != "" {
			path := resolve.payload(step.HTTP.BodyFile)
			errs = append(errs, checkFileExists(prefix, "bodyFile", step.HTTP.BodyFile, path)...)
		}
		if step.SQL != nil && step.SQL.File != "" {
			path := resolve.sql(step.SQL.File)
			errs = append(errs, checkFileExists(prefix, "sql.file", step.SQL.File, path)...)
		}
	})
	return errs
}

// checkFileExists returns an error if the resolved path does not exist. The
// message names the file as the scenario wrote it and the path that was tried,
// because the two differ once the paths block of SDD section 5.1 applies.
func checkFileExists(prefix, field, file, resolved string) []error {
	if _, err := os.Stat(resolved); err != nil {
		return []error{fmt.Errorf("%s: %s references %q, which does not exist at %s", prefix, field, file, resolved)}
	}
	return nil
}

// validateAsBindings checks that every as: name is unique and that every
// template reference to a binding names a prior as: value.
func validateAsBindings(s *Scenario) []error {
	var errs []error
	seen := make(map[string]bool)

	for _, list := range []struct {
		phase string
		steps []Step
	}{
		{"steps", s.Steps},
		{"assertions", s.Assertions},
		{"after", s.After},
	} {
		for i, step := range list.steps {
			if step.As == "" {
				continue
			}
			prefix := fmt.Sprintf("%s[%d] %q", list.phase, i, step.Name)
			if seen[step.As] {
				errs = append(errs, fmt.Errorf("%s: duplicate as name %q", prefix, step.As))
			}
			seen[step.As] = true
		}
	}
	return errs
}

// The message reference fields of SDD section 6.4. A sending step reports
// sentAt, and a receiving step reports receivedAt in its place.
var (
	sentMessageFields = map[string]bool{
		"id": true, "exchange": true, "routingKey": true,
		"body": true, "headers": true, "sentAt": true,
	}
	receivedMessageFields = map[string]bool{
		"id": true, "exchange": true, "routingKey": true,
		"body": true, "headers": true, "receivedAt": true,
	}
	// body is parsed JSON and headers is a map, so a reference goes deeper
	// through either one. Every other field is a scalar.
	nestedMessageFields = map[string]bool{"body": true, "headers": true}
)

// messageFields returns the fields a step of the given type binds, or nil for a
// type that binds no message. Only event/publish and event/captured bind one:
// section 6.4 names the receiving step, and every other probe binds the value
// its expectation read.
func messageFields(stepType string) map[string]bool {
	switch stepType {
	case "event/publish":
		return sentMessageFields
	case "event/captured":
		return receivedMessageFields
	}
	return nil
}

// validateTemplateRefs checks that every {{ .var }} reference in string fields
// resolves from variables or a prior as: binding.
//
// A reference into a message binding is checked one level deeper, against the
// field table of SDD section 6.4, because the section makes an unresolved
// reference an error at validate time. Without the deeper check a reference to
// sentAt on a received message parses, validates, and then kills the run at the
// step that uses it.
func validateTemplateRefs(s *Scenario) []error {
	var errs []error
	available := make(map[string]bool)
	for k := range s.Variables {
		available[k] = true
	}
	messages := make(map[string]map[string]bool)

	for _, list := range []struct {
		phase string
		steps []Step
	}{
		{"steps", s.Steps},
		{"assertions", s.Assertions},
		{"after", s.After},
	} {
		for i, step := range list.steps {
			prefix := fmt.Sprintf("%s[%d] %q", list.phase, i, step.Name)
			refs := collectTemplateRefs(&step)
			for _, ref := range refs {
				segments := strings.Split(ref, ".")
				if !available[segments[0]] {
					errs = append(errs, fmt.Errorf("%s: template reference .%s does not resolve from variables or a prior as binding", prefix, ref))
					continue
				}
				errs = append(errs, checkMessageFieldRef(prefix, ref, segments, messages[segments[0]])...)
			}
			if step.As != "" {
				available[step.As] = true
				if fields := messageFields(step.Type); fields != nil {
					messages[step.As] = fields
				}
			}
		}
	}
	return errs
}

// checkMessageFieldRef holds a reference into a message binding to the field
// table of SDD section 6.4.
func checkMessageFieldRef(prefix, ref string, segments []string, fields map[string]bool) []error {
	if fields == nil || len(segments) < 2 {
		return nil
	}
	field := segments[1]
	if !fields[field] {
		return []error{fmt.Errorf(
			"%s: template reference .%s names %q, which is not a field of the message binding %q",
			prefix, ref, field, segments[0])}
	}
	if len(segments) > 2 && !nestedMessageFields[field] {
		return []error{fmt.Errorf(
			"%s: template reference .%s reads through %q, which holds a single value",
			prefix, ref, field)}
	}
	return nil
}

// collectTemplateRefs extracts all template variable references from string fields of a step.
func collectTemplateRefs(step *Step) []string {
	var refs []string
	fields := []string{
		step.Name, step.Exchange, step.RoutingKey, step.DataFile,
		step.ScenarioName, step.State, step.Target, step.File, step.Query,
		step.EventType, step.EventTypePattern, step.Duration, step.Reason,
		step.Delivery, step.Timeout, step.QuietPeriod,
	}
	for _, f := range fields {
		refs = append(refs, extractRefs(f)...)
	}
	if step.GraphQL != nil {
		refs = append(refs, extractRefs(step.GraphQL.Query)...)
		refs = append(refs, extractRefs(step.GraphQL.QueryFile)...)
		refs = append(refs, collectValueRefs(step.GraphQL.Variables)...)
	}
	if step.HTTP != nil {
		refs = append(refs, extractRefs(step.HTTP.Method)...)
		refs = append(refs, extractRefs(step.HTTP.Path)...)
		refs = append(refs, extractRefs(step.HTTP.BodyFile)...)
		for _, v := range step.HTTP.Headers {
			refs = append(refs, extractRefs(v)...)
		}
		refs = append(refs, collectValueRefs(step.HTTP.Body)...)
	}
	if step.Retry != nil {
		refs = append(refs, extractRefs(step.Retry.Timeout)...)
		refs = append(refs, extractRefs(step.Retry.Interval)...)
	}
	if step.Captured != nil {
		refs = append(refs, extractRefs(step.Captured.Exchange)...)
		refs = append(refs, extractRefs(step.Captured.RoutingKey)...)
		refs = append(refs, extractRefs(step.Captured.RoutingKeyPattern)...)
		refs = append(refs, extractRefs(step.Captured.EventType)...)
		refs = append(refs, extractRefs(step.Captured.EventTypePattern)...)
		refs = append(refs, extractRefs(step.Captured.RunID)...)
		refs = append(refs, extractRefs(step.Captured.JSONPath)...)
		for _, v := range step.Captured.Headers {
			refs = append(refs, extractRefs(v)...)
		}
	}
	if step.Journal != nil {
		refs = append(refs, extractRefs(step.Journal.Method)...)
		refs = append(refs, extractRefs(step.Journal.URLPath)...)
	}
	if step.SQL != nil {
		refs = append(refs, extractRefs(step.SQL.Query)...)
		refs = append(refs, extractRefs(step.SQL.File)...)
		refs = append(refs, collectValueRefs(step.SQL.Args)...)
	}
	if step.Expect != nil {
		refs = append(refs, extractRefs(step.Expect.JSONPathExists)...)
		refs = append(refs, extractRefs(step.Expect.GraphQLErrors)...)
		if s, ok := step.Expect.JSONPath.(string); ok {
			refs = append(refs, extractRefs(s)...)
		}
		if s, ok := step.Expect.Equals.(string); ok {
			refs = append(refs, extractRefs(s)...)
		}
		refs = append(refs, collectValueRefs(step.Expect.Columns)...)
		for _, v := range step.Expect.NotNull {
			refs = append(refs, extractRefs(v)...)
		}
	}
	// An inline payload is a real template reference, because renderAny walks it
	// to the same depth as the renderer walks an external one.
	refs = append(refs, collectValueRefs(step.Data)...)
	refs = append(refs, collectValueRefs(step.Where)...)
	refs = append(refs, collectValueRefs(step.Args)...)
	for _, v := range step.HeaderFilter {
		refs = append(refs, extractRefs(v)...)
	}
	return refs
}

// collectValueRefs returns every template reference inside an arbitrarily
// nested YAML value. It walks the same shapes as renderAny, so check 4 of SDD
// section 13 sees every string the renderer renders.
func collectValueRefs(v any) []string {
	switch value := v.(type) {
	case string:
		return extractRefs(value)
	case map[string]any:
		var refs []string
		for _, item := range value {
			refs = append(refs, collectValueRefs(item)...)
		}
		return refs
	case []any:
		var refs []string
		for _, item := range value {
			refs = append(refs, collectValueRefs(item)...)
		}
		return refs
	default:
		return nil
	}
}

// extractRefs returns the root-level variable names referenced in a template string.
func extractRefs(s string) []string {
	matches := templateRefPattern.FindAllStringSubmatch(s, -1)
	refs := make([]string, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, m[1])
	}
	return refs
}

// templateActionPattern matches one template action, including one that spans
// more than one line.
var templateActionPattern = regexp.MustCompile(`(?s)\{\{.*?\}\}`)

// templatePlaceholder stands in for a rendered value while check 11 judges the
// shape of a payload. A bare zero is valid JSON on its own and valid inside a
// string, so it fits wherever a payload substitutes a value.
const templatePlaceholder = "0"

// checkJSONValidity holds check 11 of SDD section 13: a payload file is valid
// JSON. A payload is a template, and section 6.4 lets it substitute a number,
// so the raw bytes of a correct payload are not JSON until the engine renders
// them. The check therefore judges the payload after it stands a placeholder in
// for every action, which still catches a missing comma or an unbalanced brace.
// The function skips a file that does not end with ".json".
func checkJSONValidity(prefix, field, file, resolved string) []error {
	if !strings.HasSuffix(file, ".json") {
		return nil
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil
	}

	content := string(data)
	if strings.Contains(content, "{{") {
		if err := rejectControlFlow(content); err != nil {
			return []error{fmt.Errorf("%s: %s references %q: %w", prefix, field, file, err)}
		}
		if _, err := template.New(file).Funcs(templateFuncs()).Parse(content); err != nil {
			return []error{fmt.Errorf("%s: %s references %q, whose template does not parse: %w",
				prefix, field, file, err)}
		}
		content = templateActionPattern.ReplaceAllLiteralString(content, templatePlaceholder)
	}

	if !json.Valid([]byte(content)) {
		return []error{fmt.Errorf("%s: %s references %q, which contains invalid JSON", prefix, field, file)}
	}
	return nil
}

// graphqlOpPattern matches the first non-whitespace token in a GraphQL document.
// Valid starts are: query, mutation, subscription, fragment, or a bare brace.
var graphqlOpPattern = regexp.MustCompile(`^\s*(?:query|mutation|subscription|fragment|\{)`)

// checkGraphQLSyntax reads a GraphQL query file and returns an error if the
// content does not start with a recognized operation keyword or a bare brace.
func checkGraphQLSyntax(prefix, field, file, resolved string) []error {
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil
	}
	if !graphqlOpPattern.Match(data) {
		return []error{fmt.Errorf(
			"%s: %s references %q, which does not start with a valid GraphQL operation",
			prefix, field, file,
		)}
	}
	return nil
}

// wireMockMapping represents the subset of a WireMock mapping file that the
// validator inspects.
type wireMockMapping struct {
	Mappings []wireMockEntry `json:"mappings"`
}

type wireMockEntry struct {
	ScenarioName          string `json:"scenarioName"`
	RequiredScenarioState string `json:"requiredScenarioState"`
	NewScenarioState      string `json:"newScenarioState"`
	PostServeActions      []any  `json:"postServeActions"`
}

// validateWireMockStubs scans the basePath directory tree for JSON files that
// contain WireMock mappings. For every mapping that has a postServeActions
// entry, the function requires that newScenarioState is also set.
// The function skips the check silently if no JSON files exist.
func validateWireMockStubs(basePath string) []error {
	var errs []error
	entries, err := findJSONFiles(basePath)
	if err != nil {
		return nil
	}
	for _, path := range entries {
		errs = append(errs, checkStubPostServeActions(path)...)
	}
	return errs
}

// checkStubPostServeActions checks a single WireMock JSON file.
func checkStubPostServeActions(path string) []error {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	entries := parseWireMockEntries(data)
	var errs []error
	for _, m := range entries {
		if len(m.PostServeActions) > 0 && m.NewScenarioState == "" {
			errs = append(errs, fmt.Errorf(
				"wiremock stub %q: mapping with postServeActions must set newScenarioState", path,
			))
		}
	}
	return errs
}

// validateSetStateAgainstStubs implements check 9 of SDD section 13: every
// wiremock/set-state names a scenario and a state that appear in some stub
// file, matched case-sensitively. A typo in the state name moves the scenario
// to a state no stub requires, and every later stub then misses for a reason
// the output does not show.
// The function skips the check silently if no JSON stub files exist, because
// that is what lets a scenario validate without a WireMock pack.
func validateSetStateAgainstStubs(s *Scenario, basePath string) []error {
	entries, err := findJSONFiles(basePath)
	if err != nil || len(entries) == 0 {
		return nil
	}
	knownStates := make(map[string]map[string]bool)
	for _, path := range entries {
		for name, states := range extractScenarioStates(path) {
			if knownStates[name] == nil {
				knownStates[name] = make(map[string]bool)
			}
			for state := range states {
				knownStates[name][state] = true
			}
		}
	}
	if len(knownStates) == 0 {
		return nil
	}
	var errs []error
	for _, list := range []struct {
		phase string
		steps []Step
	}{
		{"steps", s.Steps},
		{"assertions", s.Assertions},
		{"after", s.After},
	} {
		for i, step := range list.steps {
			if step.Type != "wiremock/set-state" {
				continue
			}
			if step.ScenarioName == "" {
				continue
			}
			states, ok := knownStates[step.ScenarioName]
			if !ok {
				errs = append(errs, fmt.Errorf(
					"%s[%d] %q: scenarioName %q does not match any stub file",
					list.phase, i, step.Name, step.ScenarioName,
				))
				continue
			}
			if step.State != "" && !states[step.State] {
				errs = append(errs, fmt.Errorf(
					"%s[%d] %q: state %q does not appear in any stub for scenario %q",
					list.phase, i, step.Name, step.State, step.ScenarioName,
				))
			}
		}
	}
	return errs
}

// extractScenarioStates reads a WireMock JSON file and returns, for every
// scenarioName, the set of states its mappings name. A mapping names a state
// through requiredScenarioState and through newScenarioState, so both count.
func extractScenarioStates(path string) map[string]map[string]bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	states := make(map[string]map[string]bool)
	for _, m := range parseWireMockEntries(data) {
		if m.ScenarioName == "" {
			continue
		}
		if states[m.ScenarioName] == nil {
			states[m.ScenarioName] = make(map[string]bool)
		}
		for _, state := range []string{m.RequiredScenarioState, m.NewScenarioState} {
			if state != "" {
				states[m.ScenarioName][state] = true
			}
		}
	}
	return states
}

// parseWireMockEntries parses a JSON file as either a wrapper with a "mappings"
// array or as a single WireMock mapping object.
func parseWireMockEntries(data []byte) []wireMockEntry {
	var wrapper wireMockMapping
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Mappings) > 0 {
		return wrapper.Mappings
	}
	var single wireMockEntry
	if err := json.Unmarshal(data, &single); err == nil {
		return []wireMockEntry{single}
	}
	return nil
}

// findJSONFiles returns the paths of all .json files under dir.
func findJSONFiles(dir string) ([]string, error) {
	var files []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			sub, err := findJSONFiles(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			files = append(files, sub...)
			continue
		}
		if strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}

// ValidateConfigSecrets checks that auth client secrets and database DSN values
// reference environment variables through ${} interpolation instead of
// containing literal secret values.
// The check runs against the configuration as written, before interpolation,
// because after interpolation every reference already holds its value.
func ValidateConfigSecrets(cfg *config.CitadelConfig) []error {
	source := cfg.Raw()
	if source == nil {
		source = cfg
	}

	var errs []error
	for name, auth := range source.Auth {
		if auth.ClientSecret != "" && !strings.Contains(auth.ClientSecret, "${") {
			errs = append(errs, fmt.Errorf(
				"auth %q: clientSecret contains a literal value; use a ${VAR} reference",
				name,
			))
		}
	}
	if source.Targets.AppDB != nil && source.Targets.AppDB.DSN != "" {
		if !strings.Contains(source.Targets.AppDB.DSN, "${") {
			errs = append(errs, fmt.Errorf("targets.app-db: dsn contains a literal value; use a ${VAR} reference"))
		}
	}
	if source.Targets.UpstreamShadow != nil && source.Targets.UpstreamShadow.DSN != "" {
		if !strings.Contains(source.Targets.UpstreamShadow.DSN, "${") {
			errs = append(errs, fmt.Errorf("targets.upstream-shadow: dsn contains a literal value; use a ${VAR} reference"))
		}
	}
	return errs
}
