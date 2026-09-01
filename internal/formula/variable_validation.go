package formula

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// variablePattern matches {{variable}} template placeholders.
// It captures the variable name (alphanumeric + underscore, starting with letter/underscore).
var variablePattern = regexp.MustCompile(`\{\{([a-zA-Z_][a-zA-Z0-9_]*)\}\}`)

// ExtractTemplateVariables finds all {{variable}} patterns in text.
// Returns a deduplicated, sorted list of variable names.
// Handlebars helpers like {{#if}}, {{/each}}, {{else}} are excluded.
func ExtractTemplateVariables(text string) []string {
	matches := variablePattern.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	var vars []string

	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		name := match[1]

		// Skip Handlebars helpers and keywords
		if isHandlebarsKeyword(name) {
			continue
		}

		if !seen[name] {
			seen[name] = true
			vars = append(vars, name)
		}
	}

	sort.Strings(vars)
	return vars
}

// isHandlebarsKeyword returns true for Handlebars control keywords
// that look like variables but aren't (e.g., "else", "this").
func isHandlebarsKeyword(name string) bool {
	switch name {
	case "else", "this", "root", "index", "key", "first", "last",
		"end", "range", "with", "block", "define", "template", "nil":
		return true
	default:
		return false
	}
}

// ValidateTemplateVariables checks that all {{variable}} placeholders used
// in the formula are defined in the [vars] section.
//
// This catches the bug where formulas use computed variables like {{ready_count}}
// in their text but don't define them in [vars], causing bd mol wisp to fail
// with "missing required variables" error.
//
// Variables with any definition in [vars] (even with default="") are considered valid.
func (f *Formula) ValidateTemplateVariables() error {
	usedVars := ExtractTemplateVariables(formulaTemplateText(f))
	undefined := undefinedTemplateVariables(f, usedVars)

	if len(undefined) > 0 {
		return fmt.Errorf("undefined template variables: %s (add to [vars] section with default=\"\" for computed values)",
			strings.Join(undefined, ", "))
	}

	return nil
}

func formulaTemplateText(f *Formula) string {
	var text strings.Builder
	appendText(&text, f.Description)
	appendStepText(&text, f.Steps)
	appendLegText(&text, f.Legs)
	appendSynthesisText(&text, f.Synthesis)
	appendTemplateText(&text, f.Template)
	appendAspectText(&text, f.Aspects)
	appendPromptText(&text, f.Prompts)
	appendInputText(&text, f.Inputs)
	appendOutputText(&text, f.Output)
	return text.String()
}

func appendText(builder *strings.Builder, values ...string) {
	for _, value := range values {
		builder.WriteString(value)
		builder.WriteByte('\n')
	}
}

func appendStepText(builder *strings.Builder, steps []Step) {
	for _, step := range steps {
		appendText(builder, step.Title, step.Description)
	}
}

func appendLegText(builder *strings.Builder, legs []Leg) {
	for _, leg := range legs {
		appendText(builder, leg.Title, leg.Description, leg.Focus)
	}
}

func appendSynthesisText(builder *strings.Builder, synthesis *Synthesis) {
	if synthesis != nil {
		appendText(builder, synthesis.Title, synthesis.Description)
	}
}

func appendTemplateText(builder *strings.Builder, templates []Template) {
	for _, template := range templates {
		appendText(builder, template.Title, template.Description)
	}
}

func appendAspectText(builder *strings.Builder, aspects []Aspect) {
	for _, aspect := range aspects {
		appendText(builder, aspect.Title, aspect.Description, aspect.Focus)
	}
}

func appendPromptText(builder *strings.Builder, prompts map[string]string) {
	for _, prompt := range prompts {
		appendText(builder, prompt)
	}
}

func appendInputText(builder *strings.Builder, inputs map[string]Input) {
	for _, input := range inputs {
		appendText(builder, input.Description, input.Default)
	}
}

func appendOutputText(builder *strings.Builder, output *Output) {
	if output != nil {
		appendText(builder, output.Directory, output.LegPattern, output.Synthesis)
	}
}

func undefinedTemplateVariables(f *Formula, usedVars []string) []string {
	var undefined []string
	for _, variable := range usedVars {
		if _, defined := f.Vars[variable]; defined {
			continue
		}
		if _, defined := f.Inputs[variable]; defined {
			continue
		}
		undefined = append(undefined, variable)
	}
	return undefined
}
