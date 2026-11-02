package store

// GoalEvidence is what the judge sees of a deliverable of the run's turn: its
// last checker result (OK nil: not checked since its last change) and the
// start of its extracted text.
type GoalEvidence struct {
	Path    string
	Checker string
	OK      *bool
	Summary string
	Text    string
}
