package tools

import "context"

// Image is a picture a tool shows to the model (read_file of an image,
// view_file pages): sent to vision models with the next model call only
// (harness v3 §3).
type Image struct {
	// Path is the sandbox file the image comes from (the stub of later steps names it).
	Path     string
	MIMEType string
	Data     []byte
}

// ImageSink is implemented by the agent's emitter: whether the run's model
// can see images, and the queue of images for its next call.
type ImageSink interface {
	CanViewImages() bool
	AttachImage(img Image)
}

// CanViewImages reports whether the current call's model accepts images
// (false without an agent emitter).
func CanViewImages(ctx context.Context) bool {
	if _, e := CallFromContext(ctx); e != nil {
		if s, ok := e.(ImageSink); ok {
			return s.CanViewImages()
		}
	}
	return false
}

// AttachImage queues an image for the model's next call; false when the
// model cannot see images (or there is no agent emitter).
func AttachImage(ctx context.Context, img Image) bool {
	if _, e := CallFromContext(ctx); e != nil {
		if s, ok := e.(ImageSink); ok && s.CanViewImages() {
			s.AttachImage(img)
			return true
		}
	}
	return false
}
