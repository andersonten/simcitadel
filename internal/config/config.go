package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CitadelConfig holds the full configuration from a citadel.yaml file.
type CitadelConfig struct {
	Version     int                   `yaml:"version"`
	Environment EnvironmentConfig     `yaml:"environment"`
	Targets     TargetsConfig         `yaml:"targets"`
	Auth        map[string]AuthConfig `yaml:"auth"`
	Capture     CaptureConfig         `yaml:"capture"`
	Defaults    DefaultsConfig        `yaml:"defaults"`
	Paths       PathsConfig           `yaml:"paths"`
	Kubernetes  *KubernetesConfig     `yaml:"kubernetes"`

	// raw holds the configuration as written, before ${VAR} interpolation.
	raw *CitadelConfig

	// dir holds the directory of the config file. A relative content directory
	// in the paths block resolves against it.
	dir string
}

// EnvironmentConfig holds the environment identity.
type EnvironmentConfig struct {
	Name string `yaml:"name"`
}

// TargetsConfig holds the address of each target service.
type TargetsConfig struct {
	WireMock       *WireMockTarget       `yaml:"wiremock"`
	EventTrigger   *EventTriggerTarget   `yaml:"event-trigger"`
	EventCapture   *EventCaptureTarget   `yaml:"event-capture"`
	RabbitMQ       *RabbitMQTarget       `yaml:"rabbitmq"`
	App            *AppTarget            `yaml:"app"`
	AppDB          *DatabaseTarget       `yaml:"app-db"`
	UpstreamShadow *DatabaseTarget       `yaml:"upstream-shadow"`
	DebeziumOutbox *DebeziumOutboxTarget `yaml:"debezium-outbox"`
}

// WireMockTarget holds the WireMock service addresses.
type WireMockTarget struct {
	URL      string `yaml:"url"`
	AdminURL string `yaml:"adminUrl"`
}

// AdminBase returns the admin API root with exactly one /__admin suffix.
// The configured adminUrl of SDD section 5.1 already carries the suffix, but a
// caller may also supply the bare service address, so both forms resolve to the
// same root and no caller appends the segment a second time.
func (w *WireMockTarget) AdminBase() string {
	base := strings.TrimRight(w.AdminURL, "/")
	if strings.HasSuffix(base, "/__admin") {
		return base
	}
	return base + "/__admin"
}

// EventTriggerTarget holds the event trigger service address.
type EventTriggerTarget struct {
	URL string `yaml:"url"`
}

// EventCaptureTarget holds the event capture service address.
type EventCaptureTarget struct {
	URL string `yaml:"url"`
}

// RabbitMQTarget holds the RabbitMQ connection addresses.
type RabbitMQTarget struct {
	AMQPURL       string `yaml:"amqpUrl"`
	ManagementURL string `yaml:"managementUrl"`
}

// ManagementAuth returns the credentials for the RabbitMQ management API. The
// management plugin rejects an anonymous request with status 401, and the sample
// of SDD section 5.1 keeps the user and the password in amqpUrl alone, so every
// management caller derives them from there. A credential written into
// managementUrl wins, because a developer who puts one there means it. The last
// result is false when neither address carries a user.
func (r *RabbitMQTarget) ManagementAuth() (user, password string, ok bool) {
	if r == nil {
		return "", "", false
	}
	for _, raw := range []string{r.ManagementURL, r.AMQPURL} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.User == nil {
			continue
		}
		user = parsed.User.Username()
		if user == "" {
			continue
		}
		password, _ = parsed.User.Password()
		return user, password, true
	}
	return "", "", false
}

// AppTarget holds the application service address and related paths.
type AppTarget struct {
	URL           string `yaml:"url"`
	GraphQLPath   string `yaml:"graphqlPath"`
	ReadinessPath string `yaml:"readinessPath"`
	Auth          string `yaml:"auth"`
}

// InboxConfig holds the inbox table and column names.
// Each column name is configurable because the standard names the concepts
// and each service names its own columns. An unset name takes the
// outbox reference schema default.
type InboxConfig struct {
	Table             string `yaml:"table"`
	MessageIDColumn   string `yaml:"messageIdColumn"`
	ProcessedAtColumn string `yaml:"processedAtColumn"`
	RetryCountColumn  string `yaml:"retryCountColumn"`
	LastErrorColumn   string `yaml:"lastErrorColumn"`
}

// Inbox column defaults follow the outbox reference schema.
const (
	DefaultInboxTable             = "inbox"
	DefaultInboxMessageIDColumn   = "message_id"
	DefaultInboxProcessedAtColumn = "processed_at"
	DefaultInboxRetryCountColumn  = "retry_count"
	DefaultInboxLastErrorColumn   = "last_error"
)

// applyDefaults fills every unset inbox name with its reference default.
func (i *InboxConfig) applyDefaults() {
	if i.Table == "" {
		i.Table = DefaultInboxTable
	}
	if i.MessageIDColumn == "" {
		i.MessageIDColumn = DefaultInboxMessageIDColumn
	}
	if i.ProcessedAtColumn == "" {
		i.ProcessedAtColumn = DefaultInboxProcessedAtColumn
	}
	if i.RetryCountColumn == "" {
		i.RetryCountColumn = DefaultInboxRetryCountColumn
	}
	if i.LastErrorColumn == "" {
		i.LastErrorColumn = DefaultInboxLastErrorColumn
	}
}

// DatabaseTarget holds a database connection configuration.
type DatabaseTarget struct {
	Driver string       `yaml:"driver"`
	DSN    string       `yaml:"dsn"`
	Inbox  *InboxConfig `yaml:"inbox"`
}

// DebeziumOutboxTarget holds the Debezium outbox connector address.
type DebeziumOutboxTarget struct {
	URL           string `yaml:"url"`
	ConnectorName string `yaml:"connectorName"`

	// OutboxTable is the table the connector reads, qualified by schema. The
	// skipped-operation check of SDD section 4.11 reads its statistics, so it needs
	// the name. An application that follows section 10.1 leaves it unset.
	OutboxTable string `yaml:"outboxTable"`
}

// DefaultOutboxTable is the table of SDD section 10.1.
const DefaultOutboxTable = "public.outbox"

// OutboxTableOrDefault returns the configured outbox table, or the table of SDD
// section 10.1 when none is set.
func (d *DebeziumOutboxTarget) OutboxTableOrDefault() string {
	if d == nil || d.OutboxTable == "" {
		return DefaultOutboxTable
	}
	return d.OutboxTable
}

// AuthConfig holds an OAuth2 client-credentials configuration.
type AuthConfig struct {
	TokenURL     string   `yaml:"tokenUrl"`
	ClientID     string   `yaml:"clientId"`
	ClientSecret string   `yaml:"clientSecret"`
	Scopes       []string `yaml:"scopes"`
}

// CaptureExchange describes one exchange that the capture service monitors.
type CaptureExchange struct {
	Name    string `yaml:"name"`
	Declare string `yaml:"declare"`
}

// CaptureConfig holds the event capture configuration.
type CaptureConfig struct {
	Exchanges []CaptureExchange `yaml:"exchanges"`
	Ignore    []string          `yaml:"ignore"`
}

// DefaultsConfig holds the default timing and behavior values.
type DefaultsConfig struct {
	AwaitTimeout     duration `yaml:"awaitTimeout"`
	AwaitInterval    duration `yaml:"awaitInterval"`
	QuietPeriod      duration `yaml:"quietPeriod"`
	PublishBudget    int      `yaml:"publishBudget"`
	AppReadyTimeout  duration `yaml:"appReadyTimeout"`
	ResetSettle      duration `yaml:"resetSettle"`
	StrictUnconsumed bool     `yaml:"strictUnconsumed"`
	StrictUnmatched  bool     `yaml:"strictUnmatched"`
}

// The default values of the defaults block, as SDD section 5.1 writes them.
const (
	DefaultAwaitTimeout    = 30 * time.Second
	DefaultAwaitInterval   = 250 * time.Millisecond
	DefaultQuietPeriod     = 5 * time.Second
	DefaultPublishBudget   = 100
	DefaultAppReadyTimeout = 60 * time.Second
	DefaultResetSettle     = 250 * time.Millisecond
)

// applyDefaults fills every unset timing and budget value with the default of
// SDD section 5.1. A config that omits the block must behave the same as one
// that writes every documented value.
func (d *DefaultsConfig) applyDefaults() {
	if d.AwaitTimeout == 0 {
		d.AwaitTimeout = duration(DefaultAwaitTimeout)
	}
	if d.AwaitInterval == 0 {
		d.AwaitInterval = duration(DefaultAwaitInterval)
	}
	if d.QuietPeriod == 0 {
		d.QuietPeriod = duration(DefaultQuietPeriod)
	}
	if d.PublishBudget == 0 {
		d.PublishBudget = DefaultPublishBudget
	}
	if d.AppReadyTimeout == 0 {
		d.AppReadyTimeout = duration(DefaultAppReadyTimeout)
	}
	if d.ResetSettle == 0 {
		d.ResetSettle = duration(DefaultResetSettle)
	}
}

// The default content directories of SDD section 5.1.
const (
	DefaultScenariosPath = "./scenarios"
	DefaultPayloadsPath  = "./payloads"
	DefaultSQLPath       = "./sql"
	DefaultQueriesPath   = "./queries"
	DefaultWireMockPath  = "./wiremock"
)

// PayloadPath resolves an event payload or HTTP body file against paths.payloads.
func (c *CitadelConfig) PayloadPath(file string) string {
	return c.resolveContentPath(c.Paths.Payloads, file)
}

// SQLPath resolves a SQL file against paths.sql.
func (c *CitadelConfig) SQLPath(file string) string {
	return c.resolveContentPath(c.Paths.SQL, file)
}

// QueryPath resolves a GraphQL query file against paths.queries.
func (c *CitadelConfig) QueryPath(file string) string {
	return c.resolveContentPath(c.Paths.Queries, file)
}

// WireMockPath resolves a WireMock stub file against paths.wiremock.
func (c *CitadelConfig) WireMockPath(file string) string {
	return c.resolveContentPath(c.Paths.WireMock, file)
}

// WireMockDir resolves paths.wiremock to a directory.
//
// The stub loader walks the whole pack rather than resolving one file, so it
// needs the directory itself. A relative value resolves against the directory
// holding the config file, which is what keeps a run independent of the working
// directory it starts from.
func (c *CitadelConfig) WireMockDir() string {
	dir := c.Paths.WireMock
	if dir == "" {
		dir = DefaultWireMockPath
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(c.dir, dir)
	}
	return dir
}

// ScenarioPath resolves a scenario file against paths.scenarios.
func (c *CitadelConfig) ScenarioPath(file string) string {
	return c.resolveContentPath(c.Paths.Scenarios, file)
}

// resolveContentPath joins a content directory of SDD section 5.1 with a file
// the scenario names. A relative content directory is itself resolved against
// the directory holding the config file, so a run gives the same answer from
// any working directory, and validate and the runner agree on one path.
func (c *CitadelConfig) resolveContentPath(contentDir, file string) string {
	if file == "" {
		return ""
	}
	if filepath.IsAbs(file) {
		return file
	}
	if contentDir != "" && !filepath.IsAbs(contentDir) {
		contentDir = filepath.Join(c.dir, contentDir)
	}
	if contentDir == "" {
		contentDir = c.dir
	}
	return filepath.Join(contentDir, file)
}

// applyDefaults fills every unset content directory with the default of SDD
// section 5.1.
func (p *PathsConfig) applyDefaults() {
	if p.Scenarios == "" {
		p.Scenarios = DefaultScenariosPath
	}
	if p.Payloads == "" {
		p.Payloads = DefaultPayloadsPath
	}
	if p.SQL == "" {
		p.SQL = DefaultSQLPath
	}
	if p.Queries == "" {
		p.Queries = DefaultQueriesPath
	}
	if p.WireMock == "" {
		p.WireMock = DefaultWireMockPath
	}
}

// duration wraps time.Duration to support YAML unmarshal from a duration string such as "30s".
type duration time.Duration

// UnmarshalYAML parses a duration string from YAML.
func (d *duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", s, err)
	}
	*d = duration(parsed)
	return nil
}

// Duration returns the underlying time.Duration value.
func (d duration) Duration() time.Duration {
	return time.Duration(d)
}

// PathsConfig holds the file system paths for scenario content.
type PathsConfig struct {
	Scenarios string `yaml:"scenarios"`
	Payloads  string `yaml:"payloads"`
	SQL       string `yaml:"sql"`
	Queries   string `yaml:"queries"`
	WireMock  string `yaml:"wiremock"`
}

// PortForward describes one kubectl port-forward mapping.
type PortForward struct {
	Service    string `yaml:"service"`
	LocalPort  int    `yaml:"localPort"`
	RemotePort int    `yaml:"remotePort"`
}

// KubernetesConfig holds the Kubernetes cluster connection details.
type KubernetesConfig struct {
	Context   string        `yaml:"context"`
	Namespace string        `yaml:"namespace"`
	Forwards  []PortForward `yaml:"forwards"`
}

// envVarPattern matches ${VAR} and ${VAR:-default} references in string values.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// Load reads a citadel.yaml file at path, parses YAML, performs environment variable
// interpolation on string values, and returns the configuration.
// Load rejects unknown keys at every level.
func Load(path string) (*CitadelConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	if err := rejectUnknownKeys(data); err != nil {
		return nil, err
	}

	if err := validateAddressDefaults(data); err != nil {
		return nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	// Decode once before interpolation so that the literal-secret check of
	// section 5.3 sees the ${VAR} references rather than their values.
	raw := &CitadelConfig{}
	if err := doc.Decode(raw); err != nil {
		return nil, fmt.Errorf("decode config file %s: %w", path, err)
	}

	if err := interpolateNode(&doc); err != nil {
		return nil, err
	}

	cfg := &CitadelConfig{}
	if err := doc.Decode(cfg); err != nil {
		return nil, fmt.Errorf("decode config file %s: %w", path, err)
	}
	cfg.raw = raw

	if cfg.Version == 0 {
		return nil, fmt.Errorf("config file %s: version is required", path)
	}

	if cfg.Environment.Name == "" {
		return nil, fmt.Errorf("config file %s: environment.name is required", path)
	}

	cfg.dir = filepath.Dir(path)
	cfg.Defaults.applyDefaults()
	cfg.Paths.applyDefaults()

	if cfg.Targets.AppDB != nil && cfg.Targets.AppDB.Inbox != nil {
		cfg.Targets.AppDB.Inbox.applyDefaults()
	}
	if cfg.Targets.UpstreamShadow != nil && cfg.Targets.UpstreamShadow.Inbox != nil {
		cfg.Targets.UpstreamShadow.Inbox.applyDefaults()
	}

	return cfg, nil
}

// Raw returns the configuration as written, before ${VAR} interpolation.
// Raw returns nil when the configuration did not come from Load.
func (c *CitadelConfig) Raw() *CitadelConfig {
	return c.raw
}

// interpolateNode walks the YAML node tree and replaces ${VAR} and ${VAR:-default}
// references in scalar string nodes. It returns an error that names the variable
// if a referenced variable is not set and has no default.
func interpolateNode(node *yaml.Node) error {
	if node == nil {
		return nil
	}

	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		replaced, err := interpolateString(node.Value)
		if err != nil {
			return err
		}
		node.Value = replaced
		return nil
	}

	for _, child := range node.Content {
		if err := interpolateNode(child); err != nil {
			return err
		}
	}
	return nil
}

// interpolateString replaces all ${VAR} and ${VAR:-default} patterns in a single string.
func interpolateString(s string) (string, error) {
	var firstErr error
	result := envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		if firstErr != nil {
			return match
		}
		groups := envVarPattern.FindStringSubmatch(match)
		varName := groups[1]
		defaultVal := groups[2]
		hasDefault := strings.Contains(match, ":-")

		val, ok := os.LookupEnv(varName)
		if !ok {
			if hasDefault {
				return defaultVal
			}
			firstErr = fmt.Errorf("environment variable %s referenced in config is not set", varName)
			return match
		}
		return val
	})
	if firstErr != nil {
		return "", firstErr
	}
	return result, nil
}

// defaultRefPattern matches ${VAR:-default} references that address fields must not use.
var defaultRefPattern = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:-[^}]*\}`)

// addressFieldPaths lists the YAML paths to URL and DSN fields that must not use default references.
var addressFieldPaths = [][]string{
	{"targets", "wiremock", "url"},
	{"targets", "wiremock", "adminUrl"},
	{"targets", "event-trigger", "url"},
	{"targets", "event-capture", "url"},
	{"targets", "rabbitmq", "amqpUrl"},
	{"targets", "rabbitmq", "managementUrl"},
	{"targets", "app", "url"},
	{"targets", "app-db", "dsn"},
	{"targets", "upstream-shadow", "dsn"},
	{"targets", "debezium-outbox", "url"},
}

// validateAddressDefaults checks that no address field uses a ${VAR:-default} reference.
// validateAddressDefaults walks the raw YAML to inspect values before interpolation.
func validateAddressDefaults(data []byte) error {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil
	}

	for _, path := range addressFieldPaths {
		val := walkRawPath(raw, path)
		if val == "" {
			continue
		}
		if defaultRefPattern.MatchString(val) {
			return fmt.Errorf("address field %s must not use a default reference (:-)", strings.Join(path, "."))
		}
	}

	if err := validateAuthTokenURLDefaults(raw); err != nil {
		return err
	}

	return nil
}

// validateAuthTokenURLDefaults checks that no auth entry uses a default reference in tokenUrl.
func validateAuthTokenURLDefaults(raw map[string]any) error {
	authRaw, ok := raw["auth"]
	if !ok {
		return nil
	}
	authMap, ok := authRaw.(map[string]any)
	if !ok {
		return nil
	}
	for name, entry := range authMap {
		entryMap, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		tokenURL, ok := entryMap["tokenUrl"].(string)
		if !ok {
			continue
		}
		if defaultRefPattern.MatchString(tokenURL) {
			return fmt.Errorf("address field auth.%s.tokenUrl must not use a default reference (:-)", name)
		}
	}
	return nil
}

// walkRawPath traverses nested maps and returns the string value at the given key path.
func walkRawPath(m map[string]any, path []string) string {
	current := any(m)
	for _, key := range path[:len(path)-1] {
		sub, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = sub[key]
		if !ok {
			return ""
		}
	}
	sub, ok := current.(map[string]any)
	if !ok {
		return ""
	}
	val, ok := sub[path[len(path)-1]].(string)
	if !ok {
		return ""
	}
	return val
}

// knownTopLevelKeys lists the keys that citadel.yaml accepts at the top level.
var knownTopLevelKeys = map[string]bool{
	"version":     true,
	"environment": true,
	"targets":     true,
	"auth":        true,
	"capture":     true,
	"defaults":    true,
	"paths":       true,
	"kubernetes":  true,
}

// knownTargetKeys lists the keys that the targets block accepts.
var knownTargetKeys = map[string]bool{
	"wiremock":        true,
	"event-trigger":   true,
	"event-capture":   true,
	"rabbitmq":        true,
	"app":             true,
	"app-db":          true,
	"upstream-shadow": true,
	"debezium-outbox": true,
}

// knownNestedKeys maps a dotted path prefix to the set of accepted keys inside that block.
var knownNestedKeys = map[string]map[string]bool{
	"environment":             {"name": true},
	"targets.wiremock":        {"url": true, "adminUrl": true},
	"targets.event-trigger":   {"url": true},
	"targets.event-capture":   {"url": true},
	"targets.rabbitmq":        {"amqpUrl": true, "managementUrl": true},
	"targets.app":             {"url": true, "graphqlPath": true, "readinessPath": true, "auth": true},
	"targets.app-db":          {"driver": true, "dsn": true, "inbox": true},
	"targets.upstream-shadow": {"driver": true, "dsn": true, "inbox": true},
	"targets.debezium-outbox": {"url": true, "connectorName": true, "outboxTable": true},
	"defaults": {
		"awaitTimeout": true, "awaitInterval": true, "quietPeriod": true,
		"publishBudget": true, "appReadyTimeout": true, "resetSettle": true,
		"strictUnconsumed": true, "strictUnmatched": true,
	},
	"paths":                         {"scenarios": true, "payloads": true, "sql": true, "queries": true, "wiremock": true},
	"capture":                       {"exchanges": true, "ignore": true},
	"kubernetes":                    {"context": true, "namespace": true, "forwards": true},
	"targets.app-db.inbox":          inboxKeys,
	"targets.upstream-shadow.inbox": inboxKeys,
}

// knownElementKeys lists the accepted keys of each object inside a config list.
// A list element needs its own spec, because the walker cannot reach it through
// the map path of its parent.
var knownElementKeys = map[string]map[string]bool{
	"capture.exchanges":   {"name": true, "declare": true},
	"kubernetes.forwards": {"service": true, "localPort": true, "remotePort": true},
}

// inboxKeys holds the accepted keys of an inbox block. Both database targets
// share it, so the two cannot drift apart.
var inboxKeys = map[string]bool{
	"table":             true,
	"messageIdColumn":   true,
	"processedAtColumn": true,
	"retryCountColumn":  true,
	"lastErrorColumn":   true,
}

// rejectUnknownKeys parses the raw YAML as a generic map and returns an error
// if any key at any nesting level is not in the known set.
func rejectUnknownKeys(data []byte) error {
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config for key validation: %w", err)
	}

	for key := range raw {
		if !knownTopLevelKeys[key] {
			return fmt.Errorf("unknown top-level key %q in config", key)
		}
	}

	targetsRaw, ok := raw["targets"]
	if ok {
		targetsMap, ok := targetsRaw.(map[string]interface{})
		if ok {
			for key := range targetsMap {
				if !knownTargetKeys[key] {
					return fmt.Errorf("unknown target key %q in config", key)
				}
			}
		}
	}

	return rejectUnknownKeysDeep(raw, "")
}

// knownAuthEntryKeys lists the keys that each auth provider entry accepts.
var knownAuthEntryKeys = map[string]bool{
	"tokenUrl":     true,
	"clientId":     true,
	"clientSecret": true,
	"scopes":       true,
}

// rejectUnknownKeysDeep walks the raw map tree and rejects unknown keys at every level.
func rejectUnknownKeysDeep(m map[string]interface{}, prefix string) error {
	for key, val := range m {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}

		if list, isList := val.([]interface{}); isList {
			if err := rejectUnknownElementKeys(list, path); err != nil {
				return err
			}
			continue
		}

		sub, isMap := val.(map[string]interface{})

		if prefix == "auth" && isMap {
			for subKey := range sub {
				if !knownAuthEntryKeys[subKey] {
					return fmt.Errorf("unknown key %q in auth.%s", subKey, key)
				}
			}
			continue
		}

		allowed, hasSpec := knownNestedKeys[path]
		if hasSpec && isMap {
			for subKey := range sub {
				if !allowed[subKey] {
					return fmt.Errorf("unknown key %q in %s", subKey, path)
				}
			}
			if err := rejectUnknownKeysDeep(sub, path); err != nil {
				return err
			}
		} else if isMap {
			if err := rejectUnknownKeysDeep(sub, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// rejectUnknownElementKeys checks every object inside a config list against the
// spec for that list.
func rejectUnknownElementKeys(list []interface{}, path string) error {
	allowed, hasSpec := knownElementKeys[path]
	if !hasSpec {
		return nil
	}
	for i, item := range list {
		element, isMap := item.(map[string]interface{})
		if !isMap {
			continue
		}
		for key := range element {
			if !allowed[key] {
				return fmt.Errorf("unknown key %q in %s[%d]", key, path, i)
			}
		}
	}
	return nil
}
