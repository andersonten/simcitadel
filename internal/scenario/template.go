package scenario

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"reflect"
	"strings"
	"text/template"
	"text/template/parse"
	"time"
)

// allowedTemplateFuncs is the function set of SDD section 6.4. Constraint C4
// keeps the dialect to value substitution, so no other name is callable, and a
// Go built-in such as eq, printf or len is not callable either.
var allowedTemplateFuncs = map[string]bool{
	"uuid":      true,
	"now":       true,
	"nowOffset": true,
	"default":   true,
}

// rejectControlFlow returns an error if the template holds anything beyond the
// value substitution and the four functions of SDD section 6.4.
//
// The check walks the parse tree rather than the text. A trim marker, a
// pipeline and a variable declaration are all legal Go template syntax that a
// text match next to the two braces does not see, and each one reaches a
// conditional, a comparison or a loop that constraint C4 forbids.
//
// A template that does not parse returns nil here. The caller parses it next
// and reports the syntax error with its position.
func rejectControlFlow(tmpl string) error {
	if !strings.Contains(tmpl, "{{") {
		return nil
	}
	tree := parse.New("scenario")
	tree.Mode = parse.SkipFuncCheck
	parsed, err := tree.Parse(tmpl, "{{", "}}", map[string]*parse.Tree{}, templateFuncs())
	if err != nil {
		return nil
	}
	return rejectTemplateNode(parsed.Root)
}

// forbiddenDirective names the construct that constraint C4 rejects.
func forbiddenDirective(name string) error {
	return fmt.Errorf("template contains forbidden directive %q", name)
}

// rejectTemplateNode walks one parse tree node and rejects every construct
// outside the dialect of SDD section 6.4.
func rejectTemplateNode(n parse.Node) error {
	switch node := n.(type) {
	case nil:
		return nil

	case *parse.ListNode:
		if node == nil {
			return nil
		}
		for _, child := range node.Nodes {
			if err := rejectTemplateNode(child); err != nil {
				return err
			}
		}

	case *parse.TextNode, *parse.CommentNode:

	case *parse.ActionNode:
		return rejectTemplateNode(node.Pipe)

	case *parse.PipeNode:
		if node == nil {
			return nil
		}
		if len(node.Decl) > 0 {
			return forbiddenDirective(node.Decl[0].String())
		}
		for _, cmd := range node.Cmds {
			if err := rejectTemplateNode(cmd); err != nil {
				return err
			}
		}

	case *parse.CommandNode:
		for _, arg := range node.Args {
			if err := rejectTemplateNode(arg); err != nil {
				return err
			}
		}

	case *parse.IdentifierNode:
		if !allowedTemplateFuncs[node.Ident] {
			return forbiddenDirective(node.Ident)
		}

	case *parse.FieldNode, *parse.DotNode, *parse.StringNode,
		*parse.NumberNode, *parse.BoolNode, *parse.NilNode:

	case *parse.IfNode:
		return forbiddenDirective("if")

	case *parse.RangeNode:
		return forbiddenDirective("range")

	case *parse.WithNode:
		return forbiddenDirective("with")

	case *parse.TemplateNode:
		return forbiddenDirective("template")

	case *parse.BreakNode:
		return forbiddenDirective("break")

	case *parse.ContinueNode:
		return forbiddenDirective("continue")

	default:
		return forbiddenDirective(n.String())
	}
	return nil
}

// templateFuncs returns the closed function set for scenario templates.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"uuid":      generateUUID,
		"now":       func() string { return time.Now().UTC().Format(time.RFC3339) },
		"nowOffset": nowOffset,
		"default":   defaultFunc,
	}
}

// generateUUID returns a fresh UUIDv4 string using crypto/rand.
func generateUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// nowOffset returns an RFC 3339 UTC timestamp offset from the current time
// by the duration string d, for example "-72h".
func nowOffset(d string) (string, error) {
	dur, err := time.ParseDuration(d)
	if err != nil {
		return "", fmt.Errorf("parse nowOffset duration %q: %w", d, err)
	}
	return time.Now().UTC().Add(dur).Format(time.RFC3339), nil
}

// defaultFunc returns val if it is set and non-empty, otherwise returns fallback.
func defaultFunc(val any, fallback any) any {
	if val == nil {
		return fallback
	}
	v := reflect.ValueOf(val)
	switch v.Kind() {
	case reflect.String:
		if v.String() == "" {
			return fallback
		}
	case reflect.Map, reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			return fallback
		}
	}
	return val
}

// Render applies Go template rendering to all string fields in the scenario.
// Render uses the provided variables map.
// The template function set is: uuid, now, nowOffset, default.
//
// Render cannot resolve a message reference, because a binding of SDD section
// 6.4 exists only after the step that declares as: has run. Render the metadata
// with RenderMetadata and each step with RenderStep at the moment it executes.
func Render(s *Scenario, vars map[string]any) (*Scenario, error) {
	out, err := RenderMetadata(s, vars)
	if err != nil {
		return nil, err
	}

	out.Steps, err = renderSteps(s.Steps, vars)
	if err != nil {
		return nil, err
	}
	out.Assertions, err = renderSteps(s.Assertions, vars)
	if err != nil {
		return nil, err
	}
	out.After, err = renderSteps(s.After, vars)
	if err != nil {
		return nil, err
	}

	return out, nil
}

// RenderMetadata renders the scenario name and description. It leaves every
// step untouched, because a step can reference a binding that does not exist
// until an earlier step has run.
func RenderMetadata(s *Scenario, vars map[string]any) (*Scenario, error) {
	out := *s

	var err error
	out.Name, err = RenderString(s.Name, vars)
	if err != nil {
		return nil, fmt.Errorf("render scenario name: %w", err)
	}
	out.Description, err = RenderString(s.Description, vars)
	if err != nil {
		return nil, fmt.Errorf("render scenario description: %w", err)
	}

	return &out, nil
}

// RenderStep renders every templated field of one step. Call it immediately
// before the step executes, so a message reference bound by an earlier step
// resolves.
func RenderStep(step *Step, vars map[string]any) (*Step, error) {
	return renderStep(step, vars)
}

// RenderString applies template rendering to a single string.
func RenderString(tmpl string, vars map[string]any) (string, error) {
	if !strings.Contains(tmpl, "{{") {
		return tmpl, nil
	}
	if err := rejectControlFlow(tmpl); err != nil {
		return "", err
	}
	t, err := template.New("").
		Funcs(templateFuncs()).
		Option("missingkey=error").
		Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", tmpl, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("execute template %q: %w", tmpl, err)
	}
	return buf.String(), nil
}

// renderSteps renders all string fields in a slice of steps.
func renderSteps(steps []Step, vars map[string]any) ([]Step, error) {
	if len(steps) == 0 {
		return steps, nil
	}
	out := make([]Step, len(steps))
	for i := range steps {
		rendered, err := renderStep(&steps[i], vars)
		if err != nil {
			return nil, fmt.Errorf("render step %d %q: %w", i, steps[i].Name, err)
		}
		out[i] = *rendered
	}
	return out, nil
}

// renderStep renders all string fields in a single step.
func renderStep(step *Step, vars map[string]any) (*Step, error) {
	out := *step
	var err error

	out.Name, err = RenderString(step.Name, vars)
	if err != nil {
		return nil, err
	}
	out.Exchange, err = RenderString(step.Exchange, vars)
	if err != nil {
		return nil, err
	}
	out.RoutingKey, err = RenderString(step.RoutingKey, vars)
	if err != nil {
		return nil, err
	}
	out.DataFile, err = RenderString(step.DataFile, vars)
	if err != nil {
		return nil, err
	}
	out.ScenarioName, err = RenderString(step.ScenarioName, vars)
	if err != nil {
		return nil, err
	}
	out.State, err = RenderString(step.State, vars)
	if err != nil {
		return nil, err
	}
	out.Target, err = RenderString(step.Target, vars)
	if err != nil {
		return nil, err
	}
	out.File, err = RenderString(step.File, vars)
	if err != nil {
		return nil, err
	}
	out.Query, err = RenderString(step.Query, vars)
	if err != nil {
		return nil, err
	}
	out.EventType, err = RenderString(step.EventType, vars)
	if err != nil {
		return nil, err
	}
	out.EventTypePattern, err = RenderString(step.EventTypePattern, vars)
	if err != nil {
		return nil, err
	}
	out.Duration, err = RenderString(step.Duration, vars)
	if err != nil {
		return nil, err
	}
	out.Reason, err = RenderString(step.Reason, vars)
	if err != nil {
		return nil, err
	}
	out.Delivery, err = RenderString(step.Delivery, vars)
	if err != nil {
		return nil, err
	}
	out.Timeout, err = RenderString(step.Timeout, vars)
	if err != nil {
		return nil, err
	}
	out.QuietPeriod, err = RenderString(step.QuietPeriod, vars)
	if err != nil {
		return nil, err
	}

	// A retry duration is a duration like the step timeout above, so it accepts
	// a template for the same reason. A new struct keeps the loaded scenario
	// intact, because out starts as a shallow copy that shares this pointer.
	if step.Retry != nil {
		r := *step.Retry
		r.Timeout, err = RenderString(step.Retry.Timeout, vars)
		if err != nil {
			return nil, err
		}
		r.Interval, err = RenderString(step.Retry.Interval, vars)
		if err != nil {
			return nil, err
		}
		out.Retry = &r
	}

	out.Args, err = renderAnySlice(step.Args, vars)
	if err != nil {
		return nil, err
	}

	if step.GraphQL != nil {
		g := *step.GraphQL
		g.Query, err = RenderString(step.GraphQL.Query, vars)
		if err != nil {
			return nil, err
		}
		g.QueryFile, err = RenderString(step.GraphQL.QueryFile, vars)
		if err != nil {
			return nil, err
		}
		g.Variables, err = renderValueMap(step.GraphQL.Variables, vars)
		if err != nil {
			return nil, err
		}
		out.GraphQL = &g
	}

	if step.HTTP != nil {
		h := *step.HTTP
		h.Method, err = RenderString(step.HTTP.Method, vars)
		if err != nil {
			return nil, err
		}
		h.Path, err = RenderString(step.HTTP.Path, vars)
		if err != nil {
			return nil, err
		}
		h.BodyFile, err = RenderString(step.HTTP.BodyFile, vars)
		if err != nil {
			return nil, err
		}
		// A nested body renders to the same depth as an inline data payload, so
		// a reference that check 4 of SDD section 13 accepts always resolves.
		h.Body, err = renderAny(step.HTTP.Body, vars)
		if err != nil {
			return nil, err
		}
		h.Headers, err = renderStringStringMap(step.HTTP.Headers, vars)
		if err != nil {
			return nil, err
		}
		out.HTTP = &h
	}

	if step.Captured != nil {
		c := *step.Captured
		c.Exchange, err = RenderString(step.Captured.Exchange, vars)
		if err != nil {
			return nil, err
		}
		c.RoutingKey, err = RenderString(step.Captured.RoutingKey, vars)
		if err != nil {
			return nil, err
		}
		c.EventType, err = RenderString(step.Captured.EventType, vars)
		if err != nil {
			return nil, err
		}
		c.RoutingKeyPattern, err = RenderString(step.Captured.RoutingKeyPattern, vars)
		if err != nil {
			return nil, err
		}
		c.EventTypePattern, err = RenderString(step.Captured.EventTypePattern, vars)
		if err != nil {
			return nil, err
		}
		c.RunID, err = RenderString(step.Captured.RunID, vars)
		if err != nil {
			return nil, err
		}
		c.JSONPath, err = RenderString(step.Captured.JSONPath, vars)
		if err != nil {
			return nil, err
		}
		// A new map keeps the scenario as it was loaded intact, so a retry
		// renders the original text again rather than an already rendered value.
		if step.Captured.Headers != nil {
			c.Headers, err = renderStringStringMap(step.Captured.Headers, vars)
			if err != nil {
				return nil, err
			}
		}
		out.Captured = &c
	}

	if step.Journal != nil {
		j := *step.Journal
		j.Method, err = RenderString(step.Journal.Method, vars)
		if err != nil {
			return nil, err
		}
		j.URLPath, err = RenderString(step.Journal.URLPath, vars)
		if err != nil {
			return nil, err
		}
		out.Journal = &j
	}

	if step.SQL != nil {
		sq := *step.SQL
		sq.Query, err = RenderString(step.SQL.Query, vars)
		if err != nil {
			return nil, err
		}
		sq.File, err = RenderString(step.SQL.File, vars)
		if err != nil {
			return nil, err
		}
		sq.Args, err = renderAnySlice(step.SQL.Args, vars)
		if err != nil {
			return nil, err
		}
		out.SQL = &sq
	}

	if step.Expect != nil {
		e := *step.Expect
		e.JSONPathExists, err = RenderString(step.Expect.JSONPathExists, vars)
		if err != nil {
			return nil, err
		}
		e.GraphQLErrors, err = RenderString(step.Expect.GraphQLErrors, vars)
		if err != nil {
			return nil, err
		}
		if s, ok := step.Expect.JSONPath.(string); ok {
			e.JSONPath, err = RenderString(s, vars)
			if err != nil {
				return nil, err
			}
		}
		if s, ok := step.Expect.Equals.(string); ok {
			e.Equals, err = RenderString(s, vars)
			if err != nil {
				return nil, err
			}
		}
		e.Columns, err = renderValueMap(step.Expect.Columns, vars)
		if err != nil {
			return nil, err
		}
		e.NotNull, err = renderStringSlice(step.Expect.NotNull, vars)
		if err != nil {
			return nil, err
		}
		out.Expect = &e
	}

	// An inline data payload is templated to the same depth as an external one,
	// so the two forms of SDD section 6.3 behave alike.
	out.Data, err = renderAny(step.Data, vars)
	if err != nil {
		return nil, err
	}

	out.Where, err = renderValueMap(step.Where, vars)
	if err != nil {
		return nil, err
	}
	out.HeaderFilter, err = renderStringStringMap(step.HeaderFilter, vars)
	if err != nil {
		return nil, err
	}

	return &out, nil
}

// renderAny renders every string inside an arbitrarily nested YAML value.
func renderAny(v any, vars map[string]any) (any, error) {
	switch value := v.(type) {
	case nil:
		return nil, nil
	case string:
		return RenderString(value, vars)
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, item := range value {
			rendered, err := renderAny(item, vars)
			if err != nil {
				return nil, err
			}
			out[k] = rendered
		}
		return out, nil
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			rendered, err := renderAny(item, vars)
			if err != nil {
				return nil, err
			}
			out[i] = rendered
		}
		return out, nil
	default:
		return v, nil
	}
}

// renderValueMap renders every string inside a map[string]any, to any depth.
// A nested map or list holds a template reference too, and the validator counts
// it, so the renderer must resolve it.
func renderValueMap(m map[string]any, vars map[string]any) (map[string]any, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		rendered, err := renderAny(v, vars)
		if err != nil {
			return nil, err
		}
		out[k] = rendered
	}
	return out, nil
}

// renderAnySlice renders every string inside a list of YAML values, to any
// depth. A SQL argument list and a republish argument list each reach a target
// as a value, so a template inside one must resolve first.
func renderAnySlice(list []any, vars map[string]any) ([]any, error) {
	if list == nil {
		return nil, nil
	}
	out := make([]any, len(list))
	for i, v := range list {
		rendered, err := renderAny(v, vars)
		if err != nil {
			return nil, err
		}
		out[i] = rendered
	}
	return out, nil
}

// renderStringSlice renders every element of a list of strings.
func renderStringSlice(list []string, vars map[string]any) ([]string, error) {
	if list == nil {
		return nil, nil
	}
	out := make([]string, len(list))
	for i, v := range list {
		rendered, err := RenderString(v, vars)
		if err != nil {
			return nil, err
		}
		out[i] = rendered
	}
	return out, nil
}

// renderStringStringMap renders values in a map[string]string.
func renderStringStringMap(m map[string]string, vars map[string]any) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		rendered, err := RenderString(v, vars)
		if err != nil {
			return nil, err
		}
		out[k] = rendered
	}
	return out, nil
}
