package jobhunt

import "path/filepath"

// RunState remembers the conversation the host started for the autonomous search, one per day, and how many runs it
// started in it.
type RunState struct {
	Day      string `json:"day"`
	ThreadID string `json:"threadId"`
	Runs     int    `json:"runs"`
}

// RunState returns the user's autonomous-search state.
func (s *Service) RunState(login string) (RunState, error) {
	dir, err := s.userDir(login)
	if err != nil {
		return RunState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var st RunState
	return st, readJSON(filepath.Join(dir, "state.json"), &st)
}

// SetRunState stores it.
func (s *Service) SetRunState(login string, st RunState) error {
	dir, err := s.userDir(login)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(filepath.Join(dir, "state.json"), st)
}
