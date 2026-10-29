package opensandbox

import (
	"context"
	"fmt"
	"io"
	"path"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

var _ sandbox.StreamWriter = (*osbInstance)(nil)

// WriteFileFrom implements StreamWriter (the SDK uploads from the reader).
func (s *osbInstance) WriteFileFrom(ctx context.Context, p string, r io.Reader) error {
	if dir := path.Dir(p); dir != "/" && dir != "." {
		if err := s.MkdirAll(ctx, dir); err != nil {
			return fmt.Errorf("create parent directory: %w", err)
		}
	}
	return s.opErr(ctx, s.sb.UploadFile(ctx, r, sdk.UploadFileOptions{
		FileName: path.Base(p),
		Metadata: sdk.FileMetadata{Path: p, Mode: 644},
	}), false)
}
