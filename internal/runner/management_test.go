package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// The RabbitMQ management plugin rejects an anonymous request with 401. The
// sample configuration of SDD section 5.1 writes
//
//	amqpUrl:       "amqp://${RABBITMQ_USER}:${RABBITMQ_PASSWORD}@127.0.0.1:5672/"
//	managementUrl: http://127.0.0.1:15672
//
// so the credentials belong to amqpUrl alone and no management call carried
// them. Step 3 of section 4.9 could not purge a queue, step 7 could not read a
// depth, the unroutable check of section 4.11 could not run, and the two
// RabbitMQ files of the section 12.1 bundle came back empty. Against the
// shipped compose stack, which sets a user and a password, every run died in
// preflight at exit 3.

// brokerStub answers the management API paths this package reads, and it
// demands basic auth the way the management plugin does.
func brokerStub(t *testing.T, user, password string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPassword, ok := r.BasicAuth()
		if !ok || gotUser != user || gotPassword != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="RabbitMQ Management"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		switch {
		case r.URL.Path == "/api/overview":
			_, _ = w.Write([]byte(`{"rabbitmq_version":"4.3.4"}`))
		case r.URL.Path == "/api/queues":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"app.inbound","messages":0}]`))
		case r.URL.Path == "/api/exchanges":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"name":"public.a.v1"}]`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/contents"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"citadel.unroutable","messages":0}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mgmtTarget names a broker by its management address alone, which is what a
// test against a stub server that demands no credential needs.
func mgmtTarget(managementURL string) *config.RabbitMQTarget {
	return &config.RabbitMQTarget{ManagementURL: managementURL}
}

// brokerConfig returns a configuration in the shape of the section 5.1 sample:
// the credentials sit in amqpUrl and managementUrl carries none.
func brokerConfig(managementURL string) *config.CitadelConfig {
	cfg := &config.CitadelConfig{}
	cfg.Targets.RabbitMQ = &config.RabbitMQTarget{
		AMQPURL:       "amqp://citadel:citadel@127.0.0.1:5672/",
		ManagementURL: managementURL,
	}
	return cfg
}

// TestPurgeQueues_CredentialsOnlyInTheAMQPURL_PurgesTheQueue proves step 3 of
// SDD section 4.9 against a broker that demands authentication.
func TestPurgeQueues_CredentialsOnlyInTheAMQPURL_PurgesTheQueue(t *testing.T) {
	srv := brokerStub(t, "citadel", "citadel")
	cfg := brokerConfig(srv.URL)

	purged, err := purgeQueues(context.Background(), cfg, srv.Client())
	if err != nil {
		t.Fatalf("purge the queues: %v", err)
	}
	if !slices.Contains(purged, "app.inbound") {
		t.Errorf("purged = %v, and step 3 of section 4.9 must purge app.inbound", purged)
	}
}

// TestCheckUnroutableDepth_CredentialsOnlyInTheAMQPURL_ReadsTheQueue proves the
// unroutable check of SDD section 4.11 can read the depth it judges.
func TestCheckUnroutableDepth_CredentialsOnlyInTheAMQPURL_ReadsTheQueue(t *testing.T) {
	srv := brokerStub(t, "citadel", "citadel")
	cfg := brokerConfig(srv.URL)

	if err := checkUnroutableDepth(context.Background(), cfg.Targets.RabbitMQ, srv.Client()); err != nil {
		t.Errorf("the unroutable depth check reported %v against an empty queue", err)
	}
}

// TestQueueDepths_CredentialsOnlyInTheAMQPURL_ReadsEveryDepth proves step 7 of
// SDD section 4.9, which verifies that every purged queue reports depth 0.
func TestQueueDepths_CredentialsOnlyInTheAMQPURL_ReadsEveryDepth(t *testing.T) {
	srv := brokerStub(t, "citadel", "citadel")
	cfg := brokerConfig(srv.URL)

	entries, err := queueDepths(context.Background(), cfg.Targets.RabbitMQ, srv.Client())
	if err != nil {
		t.Fatalf("read the queue depths: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "app.inbound" {
		t.Errorf("queueDepths = %v, want the one queue the broker reports", entries)
	}
}

// TestManagementFor_ManagementURLWithItsOwnCredentials_KeepsThem holds the
// precedence rule. A developer who writes the user and the password into
// managementUrl means them, so amqpUrl must not override them.
func TestManagementFor_ManagementURLWithItsOwnCredentials_KeepsThem(t *testing.T) {
	target := &config.RabbitMQTarget{
		AMQPURL:       "amqp://fromamqp:amqppass@127.0.0.1:5672/",
		ManagementURL: "http://frommgmt:mgmtpass@127.0.0.1:15672",
	}

	req, err := managementFor(target).request(context.Background(), http.MethodGet, "/api/queues", nil)
	if err != nil {
		t.Fatalf("build the management request: %v", err)
	}

	user, password, ok := req.BasicAuth()
	if !ok {
		t.Fatal("the management request carries no credentials")
	}
	if user != "frommgmt" || password != "mgmtpass" {
		t.Errorf("the request carries %s, want the credentials of managementUrl", user)
	}
}

// TestManagementFor_NoCredentialsAnywhere_SendsNone keeps an unauthenticated
// broker reachable, so the fix does not invent a credential.
func TestManagementFor_NoCredentialsAnywhere_SendsNone(t *testing.T) {
	target := &config.RabbitMQTarget{ManagementURL: "http://127.0.0.1:15672"}

	req, err := managementFor(target).request(context.Background(), http.MethodGet, "/api/queues", nil)
	if err != nil {
		t.Fatalf("build the management request: %v", err)
	}
	if _, _, ok := req.BasicAuth(); ok {
		t.Error("the management request carries credentials that no target supplied")
	}
}
