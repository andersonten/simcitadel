package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	citadelamqp "github.com/andersonten/simcitadel/internal/amqp"
	"github.com/andersonten/simcitadel/internal/config"
)

// Step 3 of SDD section 4.9 purges "every queue in the environment", and it names
// what it means: the app's queues, the spy queues and the unroutable queue. The
// lock queue is none of those. It is the mechanism that stops two runs sharing an
// environment, it holds no test data, and section 4.8 declares it exclusive, so
// the management API refuses to purge it with status 400 while a run holds it.
//
// The effect was total. Reset runs before every scenario, so citadel run failed at
// exit 3 on every run against a broker where the lock queue exists, which is every
// broker during a run.

// TestPurgeQueues_TheLockQueue_IsNotPurged proves the exclusion.
func TestPurgeQueues_TheLockQueue_IsNotPurged(t *testing.T) {
	lockQueue := citadelamqp.LockQueueName()

	var purged []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/queues":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"name":"citadel.spy.public.a.v1","messages":0},
				{"name":"` + lockQueue + `","messages":0},
				{"name":"citadel.unroutable","messages":0},
				{"name":"app.inbound","messages":0}
			]`))
		// The vhost travels as %2F, and URL.Path decodes it, so the raw path is
		// the only place the queue name survives intact.
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.EscapedPath(), "/contents"):
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/api/queues/%2F/"), "/contents")
			// The lock queue is exclusive to the connection that holds the lock,
			// so a real broker answers 400 here. Answering 400 makes this stub
			// behave like the broker rather than merely record the call.
			if name == lockQueue {
				purged = append(purged, name)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			purged = append(purged, name)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := &config.CitadelConfig{}
	cfg.Targets.RabbitMQ = &config.RabbitMQTarget{ManagementURL: srv.URL}

	names, err := purgeQueues(context.Background(), cfg, srv.Client())
	if err != nil {
		t.Fatalf("purge the queues: %v", err)
	}

	if slices.Contains(purged, lockQueue) {
		t.Errorf("reset purged %s, which is exclusive to the run that holds the lock", lockQueue)
	}
	if slices.Contains(names, lockQueue) {
		t.Errorf("reset returned %s among the purged queues, so the settle check of step 7 verifies a queue it did not purge", lockQueue)
	}

	// The queues that do hold test data are still purged, so the exclusion is
	// narrow rather than a way to make the step pass.
	for _, want := range []string{"citadel.spy.public.a.v1", "citadel.unroutable", "app.inbound"} {
		if !slices.Contains(purged, want) {
			t.Errorf("reset did not purge %s, which step 3 requires", want)
		}
	}
}
