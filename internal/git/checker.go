package git

// Checker adapts *Git to branch-existence checks used by beads.BranchChecker.
type Checker struct {
	Git *Git
}

func (c Checker) BranchExists(name string) (bool, error) {
	if c.Git == nil {
		return false, nil
	}
	return BranchExists(c.Git, name)
}

func (c Checker) RemoteBranchExists(remote, name string) (bool, error) {
	if c.Git == nil {
		return false, nil
	}
	return RemoteBranchExists(c.Git, remote, name)
}
