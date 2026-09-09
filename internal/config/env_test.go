package config

import (
	"sync"
	"testing"
	"time"
)

func withFatalCapture(t *testing.T, fn func()) (called bool) {
	t.Helper()
	var mu sync.Mutex
	var exitCalled bool
	original := exitFunc
	exitFunc = func(code int) {
		mu.Lock()
		exitCalled = true
		mu.Unlock()
	}
	t.Cleanup(func() { exitFunc = original })
	fn()
	mu.Lock()
	defer mu.Unlock()
	return exitCalled
}

func TestMustURL_ValidURL_ReturnsURL(t *testing.T) {
	t.Setenv("TEST_URL", "http://localhost:8080/path")
	u := MustURL("TEST_URL")
	if u.String() != "http://localhost:8080/path" {
		t.Fatalf("got %q, want %q", u.String(), "http://localhost:8080/path")
	}
}

func TestMustURL_MissingVar_CallsFatal(t *testing.T) {
	called := withFatalCapture(t, func() {
		MustURL("NONEXISTENT_URL_VAR")
	})
	if !called {
		t.Fatal("MustURL did not call fatal for a missing variable")
	}
}

func TestMustURL_InvalidURL_CallsFatal(t *testing.T) {
	t.Setenv("TEST_URL", "://not-valid")
	called := withFatalCapture(t, func() {
		MustURL("TEST_URL")
	})
	if !called {
		t.Fatal("MustURL did not call fatal for an invalid URL")
	}
}

func TestMustString_ValidVar_ReturnsValue(t *testing.T) {
	t.Setenv("TEST_STR", "hello")
	v := MustString("TEST_STR")
	if v != "hello" {
		t.Fatalf("got %q, want %q", v, "hello")
	}
}

func TestMustString_MissingVar_CallsFatal(t *testing.T) {
	called := withFatalCapture(t, func() {
		MustString("NONEXISTENT_STR_VAR")
	})
	if !called {
		t.Fatal("MustString did not call fatal for a missing variable")
	}
}

func TestMustString_EmptyVar_CallsFatal(t *testing.T) {
	t.Setenv("TEST_STR", "")
	called := withFatalCapture(t, func() {
		MustString("TEST_STR")
	})
	if !called {
		t.Fatal("MustString did not call fatal for an empty variable")
	}
}

func TestOptDuration_ValidDuration_ReturnsParsed(t *testing.T) {
	t.Setenv("TEST_DUR", "5s")
	d := OptDuration("TEST_DUR", time.Second)
	if d != 5*time.Second {
		t.Fatalf("got %v, want %v", d, 5*time.Second)
	}
}

func TestOptDuration_MissingVar_ReturnsDefault(t *testing.T) {
	d := OptDuration("NONEXISTENT_DUR_VAR", 10*time.Second)
	if d != 10*time.Second {
		t.Fatalf("got %v, want %v", d, 10*time.Second)
	}
}

func TestOptDuration_InvalidDuration_CallsFatal(t *testing.T) {
	t.Setenv("TEST_DUR", "not-a-duration")
	called := withFatalCapture(t, func() {
		OptDuration("TEST_DUR", time.Second)
	})
	if !called {
		t.Fatal("OptDuration did not call fatal for an invalid duration")
	}
}
