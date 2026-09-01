package doctor

// PrimingCheck verifies the priming subsystem is correctly configured.
// This ensures agents receive proper context on startup via the gt prime chain.
type PrimingCheck struct {
	FixableCheck
	issues []primingIssue
}

type primingIssue struct {
	location    string
	issueType   string
	description string
	fixable     bool
	agentType   string
	rigName     string
}

// NewPrimingCheck creates a new priming subsystem check.
func NewPrimingCheck() *PrimingCheck {
	return &PrimingCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "priming",
				CheckDescription: "Verify priming subsystem is correctly configured",
			},
		},
	}
}

func (c *PrimingCheck) Run(ctx *CheckContext) *CheckResult {
	result := runPrimingCheck(c, ctx)
	_ = c.issues
	for _, issue := range c.issues {
		_, _, _, _, _, _ = issue.location, issue.issueType, issue.description, issue.fixable, issue.agentType, issue.rigName
	}
	return result
}

func (c *PrimingCheck) Fix(ctx *CheckContext) error {
	_ = c.issues
	return fixPrimingIssues(c, ctx)
}
