package opensandbox

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/7ar-chat/harness/core/sandbox"
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

var _ sandbox.PortExposer = (*osbInstance)(nil)

// Endpoint implements sandbox.PortExposer: the address (through the server's proxy when it is configured) of a port the
// workload listens on.
func (s *osbInstance) Endpoint(ctx context.Context, port int) (sandbox.Endpoint, error) {
	ep, err := s.sb.GetEndpoint(ctx, port)
	if err != nil {
		return sandbox.Endpoint{}, s.opErr(ctx, err, true)
	}
	u := s.conn.RewriteEndpointURL(ep.Endpoint)
	if !strings.HasPrefix(u, "http") {
		u = s.conn.GetProtocol() + "://" + u
	}
	return sandbox.Endpoint{URL: strings.TrimRight(u, "/"), Headers: ep.Headers}, nil
}
