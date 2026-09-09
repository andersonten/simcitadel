package steps

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// RunContext holds the state needed to execute steps within a run.
type RunContext struct {
	Config      *config.CitadelConfig
	RunID       string
	RunStart    time.Time
	Variables   map[string]any
	Bindings    map[string]any
	HTTPClient  *http.Client
	DBs         map[string]*sql.DB
	BearerToken string
	StepThrough bool
}

// NewRunContext creates a new run context.
func NewRunContext(cfg *config.CitadelConfig, runID string) *RunContext {
	return &RunContext{
		Config:     cfg,
		RunID:      runID,
		RunStart:   time.Now(),
		Variables:  make(map[string]any),
		Bindings:   make(map[string]any),
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		DBs:        make(map[string]*sql.DB),
	}
}

// Bind records the result of a step under its as: name, in the shape a template
// reads. SDD section 6.4 addresses a binding as .accepted.routingKey, which
// needs map keys rather than struct fields.
func (rc *RunContext) Bind(name string, value any) {
	if ref, ok := value.(*scenario.MessageRef); ok {
		rc.Bindings[name] = ref.TemplateValue()
		return
	}
	rc.Bindings[name] = value
}

// TemplateVars returns the variables a step template resolves against. A
// binding wins over a scenario variable of the same name, because the binding
// is the more recent fact.
func (rc *RunContext) TemplateVars() map[string]any {
	vars := make(map[string]any, len(rc.Variables)+len(rc.Bindings))
	for k, v := range rc.Variables {
		vars[k] = v
	}
	for k, v := range rc.Bindings {
		vars[k] = v
	}
	return vars
}
