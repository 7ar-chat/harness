package ai

import (
	"io"
	"sync"
)

type item[T any] struct {
	v   T
	err error
}

// StreamReader is the consuming end of a stream: Recv until io.EOF or an error, then Close. One
// goroutine reads.
type StreamReader[T any] struct {
	ch   <-chan item[T]
	done chan struct{}
	once sync.Once
}

// StreamWriter is the producing end: Send the chunks, then Close.
type StreamWriter[T any] struct {
	ch   chan<- item[T]
	done <-chan struct{}
	once sync.Once
}

// Pipe returns the two ends of a stream that buffers up to capacity chunks.
func Pipe[T any](capacity int) (*StreamReader[T], *StreamWriter[T]) {
	ch := make(chan item[T], max(capacity, 0))
	done := make(chan struct{})
	return &StreamReader[T]{ch: ch, done: done}, &StreamWriter[T]{ch: ch, done: done}
}

// Send delivers a chunk, or an error that ends the stream for the reader. It reports whether the
// reader has closed the stream (stop producing).
func (w *StreamWriter[T]) Send(chunk T, err error) (closed bool) {
	select {
	case <-w.done:
		return true
	default:
	}
	select {
	case w.ch <- item[T]{v: chunk, err: err}:
		return false
	case <-w.done:
		return true
	}
}

// Close tells the reader no more chunks come: its Recv then returns io.EOF.
func (w *StreamWriter[T]) Close() { w.once.Do(func() { close(w.ch) }) }

// Recv returns the next chunk, io.EOF once the writer closed the stream, or the error it sent.
func (r *StreamReader[T]) Recv() (T, error) {
	it, ok := <-r.ch
	if !ok {
		var zero T
		return zero, io.EOF
	}
	return it.v, it.err
}

// Close releases the stream; the writer's Send then reports it closed. Safe to call more than once.
func (r *StreamReader[T]) Close() { r.once.Do(func() { close(r.done) }) }
