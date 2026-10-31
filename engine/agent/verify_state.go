package agent

import (
	"path"
	"strings"

	"github.com/7ar-chat/harness/core/store"
)

// visualTypes need a view_file of their pages, for vision models.
var visualTypes = map[string]bool{".docx": true, ".pptx": true, ".pdf": true}

// deliverable is the verification state of one output file in the current turn.
type deliverable struct {
	changed int // sequence of the last change (0: not changed in this turn)
	checked int // sequence of the latest check result
	viewed  int // sequence of the last view_file whose pages the model saw
	// viewFailed: sequence of the last view_file that could not render it, and why.
	viewFailed int
	viewError  string
	last       CheckResult
	hasLast    bool
	// runtime: the latest result is the runtime's own check (it wins over a
	// passing model-run one).
	runtime bool
	// pending: changed past maxChecksPerCall in one call; checked before the
	// next model call (origin: that call's id).
	pending bool
	origin  string
}

// verdict of a deliverable after its last change: checked and passing,
// checked and failing, or no usable result (the checker could not run or none ran).
func (d *deliverable) verdict() (checked, ok bool) {
	if !d.hasLast || d.checked < d.changed || d.last.Failed {
		return false, false
	}
	return true, d.last.OK
}

// pendingView is a view_file whose page images were queued for the next model
// call: it counts as a view (at seq) only once the model was sent all of them.
type pendingView struct {
	path   string
	seq    int
	images []string
}

type verifyState struct {
	seq      int
	files    map[string]*deliverable
	views    []pendingView
	reminded bool
}

func (v *verifyState) file(p string) *deliverable {
	if v.files == nil {
		v.files = map[string]*deliverable{}
	}
	d := v.files[p]
	if d == nil {
		d = &deliverable{}
		v.files[p] = d
	}
	return d
}

func (v *verifyState) next() int { v.seq++; return v.seq }

func (v *verifyState) changed(p, change string) {
	if change == store.ChangeDeleted {
		delete(v.files, p)
		return
	}
	d := v.file(p)
	d.changed, d.pending = v.next(), false
}

// result records a checker result: the latest one decides, except that a
// passing model-run result never overrides the runtime's own verdict on the
// same content.
func (v *verifyState) result(r CheckResult, runtime bool) {
	d := v.file(r.Path)
	if !runtime && r.OK && d.runtime && d.checked > d.changed && !d.last.Failed {
		return
	}
	d.checked = v.next()
	d.last, d.hasLast, d.runtime = r, true, runtime
	d.pending = false
}

// viewed records a view of p seen by the model (seq: when it was made).
func (v *verifyState) viewed(p string, seq int) {
	if d, ok := v.files[p]; ok && seq > d.viewed {
		d.viewed = seq
	}
}

// tracked resolves the file a model-run checker reported against the turn's
// deliverables: absolute paths as they are, relative ones under /workspace,
// else the one deliverable ending with that relative path ("" when none).
func (v *verifyState) tracked(file string) string {
	file = strings.TrimSpace(file)
	if file == "" {
		return ""
	}
	if strings.HasPrefix(file, "/") {
		p := path.Clean(file)
		if isDeliverable(p) {
			return p
		}
		return ""
	}
	rel := path.Clean(file)
	if p := path.Clean("/workspace/" + rel); isDeliverable(p) {
		if _, ok := v.files[p]; ok {
			return p
		}
	}
	match := ""
	for p, d := range v.files {
		if d.changed > 0 && strings.HasSuffix(p, "/"+rel) {
			if match != "" {
				return "" // ambiguous
			}
			match = p
		}
	}
	return match
}

func isDeliverable(p string) bool { return strings.HasPrefix(p, OutputsDir) }

// changedOutputs are the deliverables a tool call created or changed (artifact snapshot metadata).
func changedOutputs(metadata map[string]any) (changed []string, all [][2]string) {
	items, _ := metadata["artifacts"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		p, _ := m["path"].(string)
		change, _ := m["change"].(string)
		if !isDeliverable(p) {
			continue
		}
		all = append(all, [2]string{p, change})
		if change != store.ChangeDeleted {
			changed = append(changed, p)
		}
	}
	return changed, all
}

// viewsSent counts the pending views whose page images were all sent to the
// model (sent: the image paths of the request); the others are dropped (their
// images are stubbed as not shown).
func (v *verifyState) viewsSent(sent map[string]bool) {
	for _, pv := range v.views {
		all := true
		for _, img := range pv.images {
			all = all && sent[img]
		}
		if all {
			v.viewed(pv.path, pv.seq)
		}
	}
	v.views = nil
}

func absPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/workspace/" + p
	}
	return path.Clean(p)
}
