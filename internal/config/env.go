package config

import (
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// exitFunc holds the process exit function. Tests replace it to avoid termination.
var exitFunc = os.Exit

// MustURL reads the environment variable named key, parses it as a URL, and returns it.
// MustURL terminates the process if the variable is absent or not a valid URL.
func MustURL(key string) *url.URL {
	v, ok := os.LookupEnv(key)
	if !ok {
		slog.Error("required environment variable is not set", "key", key)
		exitFunc(1)
		return nil
	}
	u, err := url.Parse(v)
	if err != nil {
		slog.Error("environment variable is not a valid URL", "key", key, "error", err)
		exitFunc(1)
		return nil
	}
	if u.Scheme == "" {
		slog.Error("environment variable is not a valid URL: missing scheme", "key", key)
		exitFunc(1)
		return nil
	}
	return u
}

// MustString reads the environment variable named key and returns its value.
// MustString terminates the process if the variable is absent or empty.
func MustString(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok {
		slog.Error("required environment variable is not set", "key", key)
		exitFunc(1)
		return ""
	}
	if v == "" {
		slog.Error("required environment variable is empty", "key", key)
		exitFunc(1)
		return ""
	}
	return v
}

// OptString reads the environment variable named key.
// OptString returns def if the variable is absent or empty.
//
// An empty value returns the default rather than an empty string. A compose file
// that carries an unset "${VAR}" produces an empty value, and treating that as a
// deliberate empty exchange name is how an unset variable reaches a broker as a
// confusing failure far from its cause.
func OptString(key, def string) string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	return v
}

// OptInt reads the environment variable named key and parses it as an int.
// OptInt returns def if the variable is absent or empty.
// OptInt terminates the process if the value is present but not a positive int.
func OptInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		slog.Error("environment variable is not a valid integer", "key", key, "error", err)
		exitFunc(1)
		return 0
	}
	if n <= 0 {
		slog.Error("environment variable must be positive", "key", key, "value", n)
		exitFunc(1)
		return 0
	}
	return n
}

// OptStringSlice reads the environment variable named key and splits it on sep.
// OptStringSlice returns def if the variable is absent or empty. Each element is
// trimmed of surrounding whitespace, and empty elements are dropped.
func OptStringSlice(key, sep string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	parts := strings.Split(v, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// OptDuration reads the environment variable named key and parses it as a time.Duration.
// OptDuration returns def if the variable is absent.
// OptDuration terminates the process if the value is present but not a valid duration.
func OptDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Error("environment variable is not a valid duration", "key", key, "error", err)
		exitFunc(1)
		return 0
	}
	return d
}
