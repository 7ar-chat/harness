package jobhunt

import (
	"path/filepath"
	"time"
)

// RunState remembers the user's job-search conversation (one for good: the mode lives in it) and how many rounds the
// host started in it today.
type RunState struct {
	// ThreadID is the conversation the agent works in, in job mode.
	ThreadID string `json:"threadId"`
	Day      string `json:"day"`
	Runs     int    `json:"runs"`
	// Discovered is when the agent was last asked to look for new boards and roles; it is asked again each day, so the
	// search does not depend on a list somebody typed once.
	Discovered time.Time `json:"discovered,omitzero"`
}

// RunState returns the user's autonomous-search state.
func (s *Service) RunState(login string) (RunState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runState(login)
}

func (s *Service) runState(login string) (RunState, error) {
	dir, err := s.userDir(login)
	if err != nil {
		return RunState{}, err
	}
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
	if err := writeJSON(filepath.Join(dir, "state.json"), st); err != nil {
		return err
	}
	if s.threads != nil {
		s.threads[Tag(login)] = st.ThreadID
	}
	return nil
}

// IsJobThread reports whether threadID is login's job-search conversation: the one that runs in job mode.
func (s *Service) IsJobThread(login, threadID string) bool {
	if login == "" || threadID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threads == nil {
		s.threads = map[string]string{}
	}
	id, ok := s.threads[Tag(login)]
	if !ok {
		st, err := s.runState(login)
		if err != nil {
			return false
		}
		id = st.ThreadID
		s.threads[Tag(login)] = id
	}
	return id == threadID
}
