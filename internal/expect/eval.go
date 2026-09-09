package expect

import (
	"fmt"
	"strconv"
	"strings"
)

// EvalJSONPath evaluates a JSONPath expression against a JSON document.
// EvalJSONPath returns the matched values.
func EvalJSONPath(doc any, path string) ([]any, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("jsonpath must start with $: %s", path)
	}
	tokens, err := tokenize(path[1:])
	if err != nil {
		return nil, fmt.Errorf("parse jsonpath %s: %w", path, err)
	}
	results := resolve([]any{doc}, tokens)
	return results, nil
}

// CompareValues compares two values with JSON type-strict matching.
// The number 1 matches 1.0 but the string "1" does not match the number 1.
func CompareValues(expected, actual any) bool {
	if expected == nil && actual == nil {
		return true
	}
	if expected == nil || actual == nil {
		return false
	}

	en, eIsNum := toFloat64(expected)
	an, aIsNum := toFloat64(actual)
	if eIsNum && aIsNum {
		return en == an
	}

	switch ev := expected.(type) {
	case string:
		av, ok := actual.(string)
		return ok && ev == av
	case bool:
		av, ok := actual.(bool)
		return ok && ev == av
	}

	return false
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}

type token struct {
	kind  tokenKind
	value string
}

type tokenKind int

const (
	tokenField    tokenKind = iota
	tokenIndex    tokenKind = iota
	tokenWildcard tokenKind = iota
	tokenFilter   tokenKind = iota
)

func tokenize(path string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(path) {
		switch path[i] {
		case '.':
			i++
			end := i
			for end < len(path) && path[end] != '.' && path[end] != '[' {
				end++
			}
			if end == i {
				return nil, fmt.Errorf("empty field name at position %d", i)
			}
			tokens = append(tokens, token{kind: tokenField, value: path[i:end]})
			i = end
		case '[':
			i++
			end := strings.Index(path[i:], "]")
			if end < 0 {
				return nil, fmt.Errorf("unclosed bracket at position %d", i)
			}
			content := path[i : i+end]
			i = i + end + 1
			if content == "*" {
				tokens = append(tokens, token{kind: tokenWildcard})
			} else if strings.HasPrefix(content, "?(") && strings.HasSuffix(content, ")") {
				tokens = append(tokens, token{kind: tokenFilter, value: content[2 : len(content)-1]})
			} else {
				tokens = append(tokens, token{kind: tokenIndex, value: content})
			}
		default:
			return nil, fmt.Errorf("unexpected character %q at position %d", path[i], i)
		}
	}
	return tokens, nil
}

func resolve(nodes []any, tokens []token) []any {
	for _, tok := range tokens {
		var next []any
		for _, node := range nodes {
			switch tok.kind {
			case tokenField:
				next = append(next, resolveField(node, tok.value)...)
			case tokenIndex:
				next = append(next, resolveIndex(node, tok.value)...)
			case tokenWildcard:
				next = append(next, resolveWildcard(node)...)
			case tokenFilter:
				next = append(next, resolveFilter(node, tok.value)...)
			}
		}
		nodes = next
	}
	return nodes
}

func resolveField(node any, field string) []any {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	v, exists := m[field]
	if !exists {
		return nil
	}
	return []any{v}
}

func resolveIndex(node any, idx string) []any {
	arr, ok := node.([]any)
	if !ok {
		return nil
	}
	i, err := strconv.Atoi(idx)
	if err != nil || i < 0 || i >= len(arr) {
		return nil
	}
	return []any{arr[i]}
}

func resolveWildcard(node any) []any {
	arr, ok := node.([]any)
	if !ok {
		return nil
	}
	result := make([]any, len(arr))
	copy(result, arr)
	return result
}

func resolveFilter(node any, expr string) []any {
	arr, ok := node.([]any)
	if !ok {
		return nil
	}
	key, value, err := parseFilterExpr(expr)
	if err != nil {
		return nil
	}
	var results []any
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if matchFilter(m, key, value) {
			results = append(results, item)
		}
	}
	return results
}

func parseFilterExpr(expr string) (string, any, error) {
	if !strings.HasPrefix(expr, "@.") {
		return "", nil, fmt.Errorf("filter must start with @.: %s", expr)
	}
	expr = expr[2:]
	eqIdx := strings.Index(expr, "==")
	if eqIdx < 0 {
		return "", nil, fmt.Errorf("filter must contain ==: %s", expr)
	}
	key := strings.TrimSpace(expr[:eqIdx])
	return key, filterLiteral(strings.TrimSpace(expr[eqIdx+2:])), nil
}

// filterLiteral turns the right side of a filter into the JSON value it names.
// A quoted literal is a string and a bare literal is a number, a boolean or
// null, so the comparison rule of SDD section 6.5 holds inside a filter too. A
// filter selects the node the assertion then reads, so a loose comparison here
// hides the same identifier-as-a-string defect that the rule exists to catch.
func filterLiteral(raw string) any {
	if len(raw) >= 2 && (raw[0] == '\'' || raw[0] == '"') && raw[len(raw)-1] == raw[0] {
		return raw[1 : len(raw)-1]
	}
	switch raw {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		return n
	}
	// An unquoted word is not a number and not a keyword, so it names a string.
	return raw
}

func matchFilter(m map[string]any, key string, expected any) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	if expected == nil {
		return v == nil
	}
	return CompareValues(expected, v)
}
