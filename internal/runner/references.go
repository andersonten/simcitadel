package runner

import (
	"sort"
	"strings"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// defaultDatabaseTarget is the target a step uses when it names none. The step
// executors of internal/steps apply the same default, so the preflight checks
// the database that the step actually reaches.
const defaultDatabaseTarget = "app-db"

// shadowDatabaseTarget holds the upstream shadow, which exists only to feed the
// inbound Debezium connector. SDD section 10.2 describes it.
const shadowDatabaseTarget = "upstream-shadow"

// targetReferences records the targets that a set of scenarios reaches.
// SDD section 4.10 checks only the targets a scenario references, so a scenario
// that does not use CDC is not blocked by a broken Debezium.
type targetReferences struct {
	wiremock     bool
	eventTrigger bool
	eventCapture bool
	app          bool
	debezium     bool
	rabbitmq     bool
	databases    map[string]bool
}

// databaseNames returns the referenced database targets in a stable order, so
// the preflight reports the same target first on every run.
func (r targetReferences) databaseNames() []string {
	names := make([]string, 0, len(r.databases))
	for name := range r.databases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// summary lists the referenced targets for the run log, so a developer sees
// which targets the preflight checked and which it left alone.
func (r targetReferences) summary() string {
	names := make([]string, 0, 6)
	if r.wiremock {
		names = append(names, "wiremock")
	}
	if r.eventTrigger {
		names = append(names, "event-trigger")
	}
	if r.eventCapture {
		names = append(names, "event-capture")
	}
	if r.rabbitmq {
		names = append(names, "rabbitmq")
	}
	if r.app {
		names = append(names, "app")
	}
	if r.debezium {
		names = append(names, "debezium")
	}
	names = append(names, r.databaseNames()...)
	return strings.Join(names, ",")
}

// referencedTargets works out which targets the scenarios of one run reach.
// The step types of SDD section 6.2 and the probe sources of section 6.5 give
// the rule. RabbitMQ is always referenced, because the environment lock and the
// reset both need the broker.
//
// The configuration takes part, because an end-of-run check of SDD section 4.11
// reaches a target that no step names.
func referencedTargets(scenarios []*scenario.Scenario, cfg *config.CitadelConfig) targetReferences {
	refs := targetReferences{rabbitmq: true, databases: make(map[string]bool)}

	// The skipped-outbox check reads the outbox table after the assertions, and
	// SDD section 4.11 runs it whatever the scenario holds. A run that opens no
	// connection to the application database leaves that check with nothing to
	// read, and a check that reads nothing reports a pass it did not earn.
	if outboxCheckReadsAppDB(cfg) {
		refs.databases[defaultDatabaseTarget] = true
	}

	for _, sc := range scenarios {
		if sc == nil {
			continue
		}
		// A truncate list reaches the application database before the first
		// step runs, so the reset needs that target as much as a step does.
		if sc.Reset != nil && len(sc.Reset.Truncate) > 0 {
			refs.databases[defaultDatabaseTarget] = true
		}
		for _, list := range [][]scenario.Step{sc.Steps, sc.Assertions, sc.After} {
			for i := range list {
				refs.addStep(&list[i])
			}
		}
	}

	// The outbox router carries every event the application emits, so an
	// assertion about a captured message depends on the connector. A scenario
	// that reads or writes the shadow database depends on the inbound
	// connector for the same reason.
	if refs.eventCapture || refs.databases[shadowDatabaseTarget] {
		refs.debezium = true
	}

	return refs
}

// addStep records the targets that one step reaches.
func (r *targetReferences) addStep(step *scenario.Step) {
	switch step.Type {
	case "wiremock/set-state":
		r.wiremock = true
	case "event/publish", "event/republish":
		r.eventTrigger = true
	case "event/captured", "event/not-captured":
		r.eventCapture = true
	case "app/graphql", "app/http":
		r.app = true
	case "sql/exec", "sql/query", "inbox/assert":
		r.databases[stepDatabaseTarget(step)] = true
	}

	// A probe and an await name their target through the source they carry,
	// so the source decides the reference for those two types.
	if step.Journal != nil {
		r.wiremock = true
	}
	if step.Captured != nil {
		r.eventCapture = true
	}
	if step.GraphQL != nil || step.HTTP != nil {
		r.app = true
	}
	if step.SQL != nil {
		r.databases[stepDatabaseTarget(step)] = true
	}
}

// stepDatabaseTarget returns the database target that a step reaches.
func stepDatabaseTarget(step *scenario.Step) string {
	if step.Target == "" {
		return defaultDatabaseTarget
	}
	return step.Target
}
