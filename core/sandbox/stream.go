package sandbox

import (
	"context"
	"io"
)

// StreamWriter is a sandbox that writes a file from a reader without holding
// its whole content in memory (uploads).
type StreamWriter interface {
	WriteFileFrom(ctx context.Context, path string, r io.Reader) error
}

// WriteFrom writes r into the sandbox at p: streamed when the sandbox supports
// it, else read into memory first.
func WriteFrom(ctx context.Context, sb Sandbox, p string, r io.Reader) error {
	if sw, ok := sb.(StreamWriter); ok {
		return sw.WriteFileFrom(ctx, p, r)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return sb.WriteFile(ctx, p, data)
}
