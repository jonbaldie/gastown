package daemon

// IsEnabled returns whether Dolt server management is enabled.
func (m *DoltServerManager) IsEnabled() bool {
	return m.config != nil && m.config.Enabled
}

// IsExternal returns whether the Dolt server is externally managed.
func (m *DoltServerManager) IsExternal() bool {
	return m.config != nil && m.config.External
}
