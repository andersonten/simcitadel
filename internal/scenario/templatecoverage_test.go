package scenario

import (
	"reflect"
	"strings"
	"testing"
)

// A scenario author cannot know which step field accepts a template unless the
// rule is simple, so the rule is that every field accepts one. Two hand written
// lists carry that rule. renderStep resolves each field before a step runs, and
// collectTemplateRefs reports each unresolved reference for check 4 of SDD
// section 13. A field that one list holds and the other does not is a defect
// that stays invisible until the middle of a run.
//
// Both lists drifted from the struct, which is how the captured pattern fields,
// the journal filters and the SQL arguments each lost their template support.
// The two tests below hold both lists to the Step type through reflection, so a
// new field fails the gate until an author adds it to both.

// structuralFields names each field that must not hold a template. Each entry
// states its reason, because an exclusion without a reason grows into a hole.
var structuralFields = map[string]string{
	".Type":        "a step type comes from a closed set, and validation matches it before the renderer runs",
	".Ref":         "a ref resolves against the shared step list at load time, before any variable exists",
	".As":          "an as binding names a variable, and check 4 must read that name without a render",
	".Retry.Until": "a retry condition comes from the closed set of keywords of SDD section 6.3",
}

const coverageSentinel = "{{ .sentinel }}"

// TestRenderStep_EveryTemplatableField_IsRendered fills every field of a step
// with a template and reports each field that keeps its template text. A field
// the renderer skips reaches a target as the literal text "{{ .var }}".
func TestRenderStep_EveryTemplatableField_IsRendered(t *testing.T) {
	var step Step
	paths := fillLeaves(reflect.ValueOf(&step), "", func(string) bool { return true }, coverageSentinel)
	if len(paths) == 0 {
		t.Fatal("the walker reached no field, so this test proves nothing")
	}

	rendered, err := RenderStep(&step, map[string]any{"sentinel": "resolved"})
	if err != nil {
		t.Fatalf("render a step whose every field holds a template: %v", err)
	}

	missed := unrenderedPaths(reflect.ValueOf(rendered), "")
	if len(missed) > 0 {
		t.Errorf("renderStep skips %d of %d fields, so each one reaches its target as literal template text:\n%s",
			len(missed), len(paths), strings.Join(missed, "\n"))
	}
}

// TestCollectTemplateRefs_EveryTemplatableField_IsChecked puts an undefined
// reference in one field at a time and reports each field that check 4 of SDD
// section 13 does not read. Check 4 exists so that citadel validate refuses a
// scenario that aborts halfway through a run.
func TestCollectTemplateRefs_EveryTemplatableField_IsChecked(t *testing.T) {
	var probe Step
	paths := fillLeaves(reflect.ValueOf(&probe), "", func(string) bool { return false }, "")
	if len(paths) == 0 {
		t.Fatal("the walker reached no field, so this test proves nothing")
	}

	var missed []string
	for _, path := range paths {
		var step Step
		fillLeaves(reflect.ValueOf(&step), "", func(p string) bool { return p == path }, "{{ .undefinedVariable }}")

		s := &Scenario{Name: "coverage", Variables: map[string]any{}, Steps: []Step{step}}
		found := false
		for _, err := range validateTemplateRefs(s) {
			if strings.Contains(err.Error(), "undefinedVariable") {
				found = true
				break
			}
		}
		if !found {
			missed = append(missed, path)
		}
	}

	if len(missed) > 0 {
		t.Errorf("check 4 accepts an undefined reference in %d of %d fields, so citadel validate returns 0 on a scenario that aborts mid-run:\n%s",
			len(missed), len(paths), strings.Join(missed, "\n"))
	}
}

// TestStructuralFields_EveryExclusion_IsStillAField keeps the exclusion list
// honest. A stale entry widens the blind spot of both tests above without
// anybody noticing, so an exclusion that names no field must be deleted.
func TestStructuralFields_EveryExclusion_IsStillAField(t *testing.T) {
	for path, reason := range structuralFields {
		if !fieldExists(reflect.TypeOf(Step{}), path) {
			t.Errorf("the exclusion %s names no field of Step, so delete it; its reason was %q", path, reason)
		}
	}
}

// fillLeaves reports the path of every string that a template can occupy inside
// v, and it sets the value at each path that want accepts. It allocates a nil
// pointer on the way, so enumeration reaches the field of a struct that the
// caller left empty.
func fillLeaves(v reflect.Value, path string, want func(string) bool, value string) []string {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			if !v.CanSet() {
				return nil
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		return fillLeaves(v.Elem(), path, want, value)

	case reflect.Struct:
		var paths []string
		t := v.Type()
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			sub := path + "." + field.Name
			if _, excluded := structuralFields[sub]; excluded {
				continue
			}
			paths = append(paths, fillLeaves(v.Field(i), sub, want, value)...)
		}
		return paths

	case reflect.String:
		if want(path) && v.CanSet() {
			v.SetString(value)
		}
		return []string{path}

	case reflect.Interface:
		// A YAML value of any type holds a string, and the renderer walks it.
		if want(path) && v.CanSet() {
			v.Set(reflect.ValueOf(value))
		}
		return []string{path}

	case reflect.Map:
		// One key proves the renderer and the validator reach a map value.
		sub := path + "[k]"
		if want(sub) && v.CanSet() {
			m := reflect.MakeMap(v.Type())
			m.SetMapIndex(reflect.ValueOf("k"), leafValue(v.Type().Elem(), value))
			v.Set(m)
		}
		return []string{sub}

	case reflect.Slice:
		sub := path + "[0]"
		if want(sub) && v.CanSet() {
			s := reflect.MakeSlice(v.Type(), 1, 1)
			s.Index(0).Set(leafValue(v.Type().Elem(), value))
			v.Set(s)
		}
		return []string{sub}

	default:
		// An int and a bool carry no template.
		return nil
	}
}

// leafValue builds a value of type t that holds s. The element of a map or a
// slice is a string or an empty interface, and both accept a string.
func leafValue(t reflect.Type, s string) reflect.Value {
	out := reflect.New(t).Elem()
	if t.Kind() == reflect.String {
		out.SetString(s)
		return out
	}
	out.Set(reflect.ValueOf(s))
	return out
}

// unrenderedPaths reports the path of every string inside v that still holds
// template text. It reads the rendered step, so it walks the values that are
// present rather than the fields the type declares.
func unrenderedPaths(v reflect.Value, path string) []string {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return unrenderedPaths(v.Elem(), path)

	case reflect.Struct:
		var out []string
		t := v.Type()
		for i := range t.NumField() {
			if !t.Field(i).IsExported() {
				continue
			}
			out = append(out, unrenderedPaths(v.Field(i), path+"."+t.Field(i).Name)...)
		}
		return out

	case reflect.String:
		if strings.Contains(v.String(), "{{") {
			return []string{path + " = " + v.String()}
		}
		return nil

	case reflect.Map:
		var out []string
		for _, key := range v.MapKeys() {
			out = append(out, unrenderedPaths(v.MapIndex(key), path+"["+key.String()+"]")...)
		}
		return out

	case reflect.Slice:
		var out []string
		for i := range v.Len() {
			out = append(out, unrenderedPaths(v.Index(i), path+"[0]")...)
		}
		return out

	default:
		return nil
	}
}

// fieldExists reports whether a dotted path names a field of t.
func fieldExists(t reflect.Type, path string) bool {
	for _, name := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return false
		}
		field, ok := t.FieldByName(name)
		if !ok {
			return false
		}
		t = field.Type
	}
	return true
}
