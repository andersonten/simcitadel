package diagnostics

import (
	"regexp"
)

var secretKeyPattern = regexp.MustCompile(`(?i)(secret|password|token|authorization|dsn)`)
var bearerTokenPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)

// jwtPattern matches a JSON Web Token, which is the bearer token shape that a
// Hydra deployment issues. SDD section 12.2 replaces "any string matching a
// known bearer-token shape wherever it appears", and a token carries the word
// Bearer only inside an Authorization header, which the key list of the same
// section already covers. The shape rule therefore earns its place on the token
// that appears without that word: in a journal entry's URL query, and inside a
// request or response body that the journal holds as one string, where no key
// of the token reaches the key list.
//
// The first segment of a JWT encodes a JSON object, so its base64url form
// always begins with eyJ. Anchoring there keeps an ordinary dotted string out
// of the match.
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)

// urlCredentialPattern matches the password of the userinfo component of a URL,
// for example the second field of "amqp://user:password@host:5672/". The key
// list of SDD section 12.2 covers a value that is a credential, and it does not
// cover a credential embedded in an address. The reference config of section 5.1
// writes "amqp://${RABBITMQ_USER}:${RABBITMQ_PASSWORD}@127.0.0.1:5672/", so the
// resolved config in run.json holds the broker password unless this runs.
//
// The pattern keeps the scheme, the user and the host, because an address is the
// most useful line in a bundle. It replaces the password alone.
var urlCredentialPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/?#@]+):[^\s/?#@]*@`)

const redactedValue = "«redacted»"
const redactedBearer = "Bearer «redacted»"

// redactedURLCredential keeps the scheme and the user of the matched URL and
// replaces the password.
const redactedURLCredential = "$1:«redacted»@"

// Redact replaces any value whose key matches a secret pattern with "«redacted»".
// Redact also replaces strings that look like bearer tokens.
// Redact walks nested maps and slices recursively.
func Redact(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	result := make(map[string]any, len(data))
	for k, v := range data {
		if secretKeyPattern.MatchString(k) {
			result[k] = redactedValue
			continue
		}
		result[k] = redactValue(v)
	}
	return result
}

// RedactString replaces a bearer token and the password of any URL that carries
// one, wherever either appears in the string. A connection string reaches a
// bundle inside an error message as well as inside the resolved config, so the
// replacement cannot depend on the key that holds it.
func RedactString(s string) string {
	s = bearerTokenPattern.ReplaceAllString(s, redactedBearer)
	s = jwtPattern.ReplaceAllString(s, redactedValue)
	return urlCredentialPattern.ReplaceAllString(s, redactedURLCredential)
}

func redactValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return Redact(val)
	case []any:
		return redactSlice(val)
	case string:
		return RedactString(val)
	default:
		return v
	}
}

func redactSlice(items []any) []any {
	result := make([]any, len(items))
	for i, item := range items {
		switch val := item.(type) {
		case map[string]any:
			result[i] = Redact(val)
		case []any:
			result[i] = redactSlice(val)
		case string:
			result[i] = RedactString(val)
		default:
			result[i] = item
		}
	}
	return result
}
