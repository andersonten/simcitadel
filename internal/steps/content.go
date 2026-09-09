package steps

import (
	"fmt"
	"os"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// readRenderedContent reads a file that a scenario references and renders it
// with the template engine of SDD section 6.4. A payload file, a SQL file, a
// GraphQL query file and an HTTP body file are all templates, so the render
// happens at the point of read and before any parse. A payload that substitutes
// a number is not valid JSON until it is rendered.
//
// The variable set is the one RenderStep uses, so a message reference such as
// .binding.id resolves in a file exactly as it does in an inline field.
func readRenderedContent(path string, rc *RunContext) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", path, err)
	}
	rendered, err := scenario.RenderString(string(raw), rc.TemplateVars())
	if err != nil {
		return "", fmt.Errorf("render file %s: %w", path, err)
	}
	return rendered, nil
}
