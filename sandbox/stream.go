package sandbox

import (
	"context"
	"fmt"
	"io"
	"path"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
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

var _ StreamWriter = (*osbInstance)(nil)

// WriteFileFrom implements StreamWriter (the SDK uploads from the reader).
func (s *osbInstance) WriteFileFrom(ctx context.Context, p string, r io.Reader) error {
	if dir := path.Dir(p); dir != "/" && dir != "." {
		if err := s.MkdirAll(ctx, dir); err != nil {
			return fmt.Errorf("create parent directory: %w", err)
		}
	}
	return s.opErr(ctx, s.sb.UploadFile(ctx, r, opensandbox.UploadFileOptions{
		FileName: path.Base(p),
		Metadata: opensandbox.FileMetadata{Path: p, Mode: 644},
	}), false)
}
