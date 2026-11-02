package projectdocs

// Untrusted reports whether the instructions rendered for the run came (in
// part) from the repository, so the run's context was reported untrusted.
func (d *Docs) Untrusted(runID string) bool {
	d.mu.Lock()
	st, ok := d.runs[runID]
	d.mu.Unlock()
	if !ok {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.untrusted
}
