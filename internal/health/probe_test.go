package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestURLForListenAddr_HostlessAddress_ResolvesToLoopback(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"port only", ":8080", "http://127.0.0.1:8080/health"},
		{"wildcard IPv4", "0.0.0.0:8080", "http://127.0.0.1:8080/health"},
		{"wildcard IPv6", "[::]:8080", "http://127.0.0.1:8080/health"},
		{"explicit host", "127.0.0.1:9090", "http://127.0.0.1:9090/health"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := URLForListenAddr(tc.input)
			if err != nil {
				t.Fatalf("URLForListenAddr(%q) returned error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("URLForListenAddr(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestURLForListenAddr_MalformedAddress_ReturnsError(t *testing.T) {
	if _, err := URLForListenAddr("not-an-address"); err == nil {
		t.Fatal("URLForListenAddr with a malformed address returned no error, want one")
	}
}

func TestProbe_HealthyEndpoint_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := Probe(t.Context(), srv.URL+"/health"); err != nil {
		t.Fatalf("Probe against a healthy endpoint returned error: %v", err)
	}
}

func TestProbe_UnhealthyEndpoint_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := Probe(t.Context(), srv.URL+"/health"); err == nil {
		t.Fatal("Probe against a 503 endpoint returned no error, want one")
	}
}

func TestProbe_UnreachableEndpoint_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL + "/health"
	srv.Close()

	if err := Probe(t.Context(), url); err == nil {
		t.Fatal("Probe against a closed listener returned no error, want one")
	}
}
