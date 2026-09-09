package runner

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/andersonten/simcitadel/internal/config"
)

// management describes the RabbitMQ management API of one environment: the base
// address, and the credentials that every request to it carries.
//
// The management plugin rejects an anonymous request with status 401. The sample
// configuration of SDD section 5.1 keeps the user and the password in
// targets.rabbitmq.amqpUrl and gives managementUrl a bare address, so a caller
// that sends the address alone reaches no real broker. Every management call in
// this package goes through this type for that reason.
type management struct {
	base     string
	user     string
	password string
	hasAuth  bool
}

// managementFor builds the management endpoint of a configured broker.
func managementFor(target *config.RabbitMQTarget) management {
	m := management{}
	if target == nil {
		return m
	}
	m.user, m.password, m.hasAuth = target.ManagementAuth()

	// The credential travels in the Authorization header alone. Removing it
	// from the address keeps it out of every log line and every failure
	// message that names the address, which is what SDD section 12.2 asks of
	// output a developer pastes into a chat window.
	m.base = strings.TrimRight(target.ManagementURL, "/")
	if parsed, err := url.Parse(m.base); err == nil && parsed.User != nil {
		parsed.User = nil
		m.base = strings.TrimRight(parsed.String(), "/")
	}
	return m
}

// address joins the base address and one API path.
func (m management) address(path string) string {
	return m.base + path
}

// request builds one management API request with its credentials applied.
func (m management) request(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.address(path), payload)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if m.hasAuth {
		req.SetBasicAuth(m.user, m.password)
	}
	return req, nil
}
