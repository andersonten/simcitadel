// Package health provides the self-probe that a service binary runs when the
// container health check invokes it.
//
// The services ship FROM scratch, so the image holds no shell and no curl.
// The health check therefore re-executes the service binary with a flag, and
// that invocation calls Probe.
package health

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// loopbackHost is the host that a container health check reaches its own
// listener on. This is a self-probe, not a service address, so it is not
// subject to the configuration rule that forbids a compiled-in address.
const loopbackHost = "127.0.0.1"

// ProbeTimeout bounds a single health probe.
const ProbeTimeout = 3 * time.Second

// DefaultHealthPath is the path the two sidecars serve.
const DefaultHealthPath = "/health"

// URLForListenAddr builds the health URL that matches a listen address.
// A listen address without a host, such as ":8080", resolves to loopback.
func URLForListenAddr(listenAddr string) (string, error) {
	return URLForListenAddrPath(listenAddr, DefaultHealthPath)
}

// URLForListenAddrPath builds the URL for a listen address and a path. An app
// chooses its own readiness path, and SDD section 5.1 gives the app target a
// readinessPath field for that reason, so a self-probe cannot assume one name.
func URLForListenAddrPath(listenAddr, path string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("parse listen address %q: %w", listenAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = loopbackHost
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}

// Probe performs a GET against the health URL.
// Probe returns nil only when the endpoint answers with 200.
func Probe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build health request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("health request to %s: %w", url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint %s returned status %d, expect 200", url, resp.StatusCode)
	}
	return nil
}

// ProbeListenAddr probes the health endpoint that belongs to a listen address.
func ProbeListenAddr(ctx context.Context, listenAddr string) error {
	return ProbeListenAddrPath(ctx, listenAddr, DefaultHealthPath)
}

// ProbeListenAddrPath probes a listen address on a given path.
func ProbeListenAddrPath(ctx context.Context, listenAddr, path string) error {
	if !strings.Contains(listenAddr, ":") {
		listenAddr = ":" + listenAddr
	}
	url, err := URLForListenAddrPath(listenAddr, path)
	if err != nil {
		return err
	}
	return Probe(ctx, url)
}
