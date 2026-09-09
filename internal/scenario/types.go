package scenario

// Scenario represents one scenario file.
type Scenario struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Variables   map[string]any `yaml:"variables"`
	Reset       *ResetSpec     `yaml:"reset"`
	Steps       []Step         `yaml:"steps"`
	Assertions  []Step         `yaml:"assertions"`
	After       []Step         `yaml:"after"`
}

// ResetSpec holds reset configuration overrides for a scenario.
// SDD §4.9 specifies that reset is unconditional.
type ResetSpec struct {
	Truncate []string `yaml:"truncate"`
}

// Step represents one step in a scenario.
type Step struct {
	Name     string     `yaml:"name"`
	Type     string     `yaml:"type"`
	Ref      string     `yaml:"ref"`
	As       string     `yaml:"as"`
	Optional bool       `yaml:"optional"`
	Timeout  string     `yaml:"timeout"`
	Retry    *RetrySpec `yaml:"retry"`

	// event/publish and event/republish fields
	Exchange   string `yaml:"exchange"`
	RoutingKey string `yaml:"routingKey"`
	DataFile   string `yaml:"dataFile"`
	Data       any    `yaml:"data"`
	Delivery   string `yaml:"delivery"`

	// wiremock/set-state fields
	ScenarioName string `yaml:"scenarioName"`
	State        string `yaml:"state"`

	// sql/exec and sql/query fields
	Target string `yaml:"target"`
	File   string `yaml:"file"`
	Query  string `yaml:"query"`
	Args   []any  `yaml:"args"`

	// probe, await, app/graphql, app/http, event/captured, and sql/query fields
	GraphQL  *GraphQLSource `yaml:"graphql"`
	HTTP     *HTTPSource    `yaml:"http"`
	Captured *CapturedQuery `yaml:"captured"`
	Journal  *JournalQuery  `yaml:"journal"`
	SQL      *SQLSource     `yaml:"sql"`
	Expect   *Expect        `yaml:"expect"`

	// event/not-captured fields
	EventType        string            `yaml:"eventType"`
	EventTypePattern string            `yaml:"eventTypePattern"`
	QuietPeriod      string            `yaml:"quietPeriod"`
	HeaderFilter     map[string]string `yaml:"headerFilter"`

	// inbox/assert fields
	Where map[string]any `yaml:"where"`

	// wait fields
	Duration string `yaml:"duration"`
	Reason   string `yaml:"reason"`
}

// RetrySpec configures step retry behavior.
type RetrySpec struct {
	Until    string `yaml:"until"`
	Timeout  string `yaml:"timeout"`
	Interval string `yaml:"interval"`
}

// GraphQLSource defines a GraphQL query source for a probe.
type GraphQLSource struct {
	Query     string         `yaml:"query"`
	QueryFile string         `yaml:"queryFile"`
	Variables map[string]any `yaml:"variables"`
}

// HTTPSource defines an HTTP request source for a probe.
type HTTPSource struct {
	Method   string            `yaml:"method"`
	Path     string            `yaml:"path"`
	Body     any               `yaml:"body"`
	BodyFile string            `yaml:"bodyFile"`
	Headers  map[string]string `yaml:"headers"`
}

// CapturedQuery defines filters for querying captured messages.
type CapturedQuery struct {
	Exchange          string            `yaml:"exchange"`
	RoutingKey        string            `yaml:"routingKey"`
	RoutingKeyPattern string            `yaml:"routingKeyPattern"`
	EventType         string            `yaml:"eventType"`
	EventTypePattern  string            `yaml:"eventTypePattern"`
	RunID             string            `yaml:"runId"`
	JSONPath          string            `yaml:"jsonPath"`
	Headers           map[string]string `yaml:"headers"`
}

// JournalQuery defines filters for WireMock request journal queries.
type JournalQuery struct {
	Method  string `yaml:"method"`
	URLPath string `yaml:"urlPath"`
}

// SQLSource defines an SQL query source for a probe.
type SQLSource struct {
	Query string `yaml:"query"`
	File  string `yaml:"file"`
	Args  []any  `yaml:"args"`
}

// MessageRef holds result binding fields for a completed step.
// The runner populates these fields after a step executes.
type MessageRef struct {
	ID         string            `yaml:"id"`
	Exchange   string            `yaml:"exchange"`
	RoutingKey string            `yaml:"routingKey"`
	Body       any               `yaml:"body"`
	Headers    map[string]string `yaml:"headers"`
	SentAt     string            `yaml:"sentAt"`
	ReceivedAt string            `yaml:"receivedAt"`
}

// TemplateValue returns the binding as a template sees it. SDD section 6.4
// spells the fields in lower camel case, for example .accepted.routingKey, and
// a Go template matches a map key exactly, so the struct cannot be used
// directly. A receiving step reports receivedAt in place of sentAt.
func (m *MessageRef) TemplateValue() map[string]any {
	if m == nil {
		return nil
	}
	value := map[string]any{
		"id":         m.ID,
		"exchange":   m.Exchange,
		"routingKey": m.RoutingKey,
		"body":       m.Body,
		"headers":    m.Headers,
	}
	if m.ReceivedAt != "" {
		value["receivedAt"] = m.ReceivedAt
	} else {
		value["sentAt"] = m.SentAt
	}
	return value
}

// Expect defines the expected outcome of a probe.
type Expect struct {
	Status           int            `yaml:"status"`
	JSONPath         any            `yaml:"jsonPath"`
	JSONPathExists   string         `yaml:"jsonPathExists"`
	Equals           any            `yaml:"equals"`
	Rows             *int           `yaml:"rows"`
	Count            *int           `yaml:"count"`
	CountAtLeast     *int           `yaml:"countAtLeast"`
	Columns          map[string]any `yaml:"columns"`
	NotNull          []string       `yaml:"notNull"`
	CloudEventsValid bool           `yaml:"cloudEventsValid"`
	GraphQLErrors    string         `yaml:"graphqlErrors"`
}
