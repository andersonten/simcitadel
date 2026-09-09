package steps

var stepTypes = []string{
	"event/publish",
	"event/republish",
	"wiremock/set-state",
	"sql/exec",
	"sql/query",
	"probe",
	"await",
	"app/graphql",
	"app/http",
	"event/captured",
	"inbox/assert",
	"event/not-captured",
	"wait",
}

var stepTypeSet map[string]bool

func init() {
	stepTypeSet = make(map[string]bool, len(stepTypes))
	for _, t := range stepTypes {
		stepTypeSet[t] = true
	}
}

// StepTypes returns the closed set of valid step type strings.
func StepTypes() []string {
	result := make([]string, len(stepTypes))
	copy(result, stepTypes)
	return result
}

// IsValidType reports whether the given type string is in the closed set.
func IsValidType(t string) bool {
	return stepTypeSet[t]
}
