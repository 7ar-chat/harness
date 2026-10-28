package agent

import (
	"sync"

	"github.com/GhaziBenDahmane/harness/tools"
)

// The queue of images tools show the model (tools.ImageSink, harness v3 §3):
// tools only queue; images.go turns the drained queue into message parts.

// imageQueue collects the images tools attach during a step (tools may run in parallel).
type imageQueue struct {
	mu     sync.Mutex
	images []tools.Image
}

// CanViewImages implements tools.ImageSink.
func (e *toolEmitter) CanViewImages() bool {
	v := e.st.a.Harness.Vision
	return v != nil && v(e.st.in.Run.Model)
}

// AttachImage implements tools.ImageSink: it only queues the image.
func (e *toolEmitter) AttachImage(img tools.Image) {
	if len(img.Data) == 0 {
		return
	}
	q := &e.st.h.images
	q.mu.Lock()
	defer q.mu.Unlock()
	q.images = append(q.images, img)
}

// takeToolImages drains the images queued by the tools since the last call.
func (st *runState) takeToolImages() []tools.Image {
	q := &st.h.images
	q.mu.Lock()
	defer q.mu.Unlock()
	imgs := q.images
	q.images = nil
	return imgs
}
