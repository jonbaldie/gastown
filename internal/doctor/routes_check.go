package doctor

// RoutesCheck verifies that beads routing is properly configured.
// It checks that routes.jsonl exists, all rigs have routing entries,
// and all routes point to valid locations.
type RoutesCheck struct {
	FixableCheck
}

// NewRoutesCheck creates a new routes configuration check.
func NewRoutesCheck() *RoutesCheck {
	return &RoutesCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "routes-config",
				CheckDescription: "Check beads routing configuration",
				CheckCategory:    CategoryConfig,
			},
		},
	}
}

func (c *RoutesCheck) Run(ctx *CheckContext) *CheckResult {
	return runRoutesCheck(c, ctx)
}

func (c *RoutesCheck) Fix(ctx *CheckContext) error {
	return fixRoutes(c, ctx)
}
