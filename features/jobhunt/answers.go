package jobhunt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The answer bank: what the agent learned about the person that no document says (a notice period, a salary figure, a
// visa detail, why they want a kind of role). The agent asks a question once, the answer is kept, and every later form and
// every later round reuses it. A question that is open never stops the search: the job waits, the rest goes on.

const (
	maxQuestions = 300
	maxQuestion  = 600
	maxAnswer    = 4000
)

// Question is one thing the agent needs to know, or has learned.
type Question struct {
	ID       string    `json:"id"`
	Text     string    `json:"text"`
	Answer   string    `json:"answer,omitempty"`
	JobID    string    `json:"jobId,omitempty"` // the job that raised it, when one did
	Asked    time.Time `json:"asked"`
	Answered time.Time `json:"answered,omitzero"`
	// Fresh is true from the moment the answer arrives until the agent has read it (jobs_answers), so the host knows a
	// round has something to resume.
	Fresh bool `json:"fresh,omitempty"`
}

type answersFile struct {
	Questions []Question `json:"questions"`
}

func questionID(text string) string {
	sum := sha256.Sum256([]byte(squash(text)))
	return hex.EncodeToString(sum[:6])
}

func (s *Service) loadAnswers(login string) (string, []Question, error) {
	dir, err := s.userDir(login)
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "answers.json")
	var f answersFile
	if err := readJSON(path, &f); err != nil {
		return "", nil, err
	}
	return path, f.Questions, nil
}

// Questions returns the bank, open questions first, then the newest answers.
func (s *Service) Questions(login string) ([]Question, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, qs, err := s.loadAnswers(login)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(qs, func(i, j int) bool {
		if (qs[i].Answer == "") != (qs[j].Answer == "") {
			return qs[i].Answer == ""
		}
		return qs[i].Asked.After(qs[j].Asked)
	})
	return qs, nil
}

// Ask records a question for the person. A question already asked (the same words, ignoring case and punctuation) is
// returned as it is, with its answer when there is one, so the agent never asks twice.
func (s *Service) Ask(login, text, jobID string) (Question, error) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > maxQuestion {
		return Question{}, fmt.Errorf("%w: a question is 1 to %d characters", ErrInvalid, maxQuestion)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, qs, err := s.loadAnswers(login)
	if err != nil {
		return Question{}, err
	}
	id := questionID(text)
	for _, q := range qs {
		if q.ID == id {
			return q, nil
		}
	}
	if len(qs) >= maxQuestions {
		return Question{}, fmt.Errorf("%w: too many questions (%d)", ErrInvalid, maxQuestions)
	}
	q := Question{ID: id, Text: text, JobID: jobID, Asked: s.now()}
	return q, writeJSON(path, answersFile{append(qs, q)})
}

// Remember stores an answer the person gave: to an open question when the words match one, otherwise as a new entry.
func (s *Service) Remember(login, text, answer string) (Question, error) {
	text, answer = strings.TrimSpace(text), strings.TrimSpace(answer)
	if text == "" || answer == "" || len(text) > maxQuestion || len(answer) > maxAnswer {
		return Question{}, fmt.Errorf("%w: a question and an answer are needed (answer up to %d characters)", ErrInvalid, maxAnswer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, qs, err := s.loadAnswers(login)
	if err != nil {
		return Question{}, err
	}
	id := questionID(text)
	now := s.now()
	for i := range qs {
		if qs[i].ID == id {
			qs[i].Answer, qs[i].Answered, qs[i].Fresh = answer, now, false
			return qs[i], writeJSON(path, answersFile{qs})
		}
	}
	if len(qs) >= maxQuestions {
		return Question{}, fmt.Errorf("%w: too many questions (%d)", ErrInvalid, maxQuestions)
	}
	q := Question{ID: id, Text: text, Answer: answer, Asked: now, Answered: now}
	return q, writeJSON(path, answersFile{append(qs, q)})
}

// Answer stores the person's answer to a question by id (the Jobs page). The next round resumes what waited on it.
func (s *Service) Answer(login, id, answer string) (Question, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" || len(answer) > maxAnswer {
		return Question{}, fmt.Errorf("%w: an answer is 1 to %d characters", ErrInvalid, maxAnswer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, qs, err := s.loadAnswers(login)
	if err != nil {
		return Question{}, err
	}
	for i := range qs {
		if qs[i].ID == id {
			qs[i].Answer, qs[i].Answered, qs[i].Fresh = answer, s.now(), true
			return qs[i], writeJSON(path, answersFile{qs})
		}
	}
	return Question{}, ErrNotFound
}

// Forget removes a question or answer.
func (s *Service) Forget(login, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, qs, err := s.loadAnswers(login)
	if err != nil {
		return err
	}
	for i := range qs {
		if qs[i].ID == id {
			return writeJSON(path, answersFile{append(qs[:i:i], qs[i+1:]...)})
		}
	}
	return ErrNotFound
}

// MarkSeen clears the fresh flag: the agent has read the new answers.
func (s *Service) MarkSeen(login string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, qs, err := s.loadAnswers(login)
	if err != nil {
		return err
	}
	changed := false
	for i := range qs {
		if qs[i].Fresh {
			qs[i].Fresh, changed = false, true
		}
	}
	if !changed {
		return nil
	}
	return writeJSON(path, answersFile{qs})
}

// AnswerCounts returns how many questions are open and how many answers the agent has not read yet.
func (s *Service) AnswerCounts(login string) (open, fresh int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, qs, err := s.loadAnswers(login)
	if err != nil {
		return 0, 0, err
	}
	for _, q := range qs {
		switch {
		case q.Answer == "":
			open++
		case q.Fresh:
			fresh++
		}
	}
	return open, fresh, nil
}
