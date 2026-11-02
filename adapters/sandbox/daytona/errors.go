package daytona

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	sdkerrors "github.com/daytonaio/daytona/libs/sdk-go/pkg/errors"

	"github.com/7ar-chat/harness/core/sandbox"
)

// statusOf returns the HTTP status the SDK attached to err (0 when it has none).
func statusOf(err error) int {
	var (
		nf  *sdkerrors.DaytonaNotFoundError
		rl  *sdkerrors.DaytonaRateLimitError
		au  *sdkerrors.DaytonaAuthenticationError
		fb  *sdkerrors.DaytonaForbiddenError
		cf  *sdkerrors.DaytonaConflictError
		va  *sdkerrors.DaytonaValidationError
		sv  *sdkerrors.DaytonaServerError
		gen *sdkerrors.DaytonaError
	)
	switch {
	case errors.As(err, &nf):
		return 404
	case errors.As(err, &rl):
		return 429
	case errors.As(err, &au):
		return 401
	case errors.As(err, &fb):
		return 403
	case errors.As(err, &cf):
		return 409
	case errors.As(err, &va):
		return 400
	case errors.As(err, &sv):
		if sv.DaytonaError != nil && sv.StatusCode != 0 {
			return sv.StatusCode
		}
		return 500
	case errors.As(err, &gen):
		return gen.StatusCode
	}
	return 0
}

// isNotFound reports a 404 from Daytona.
func isNotFound(err error) bool { return err != nil && statusOf(err) == 404 }

// isQuota reports an account limit (Daytona answers 403 "... quota exceeded" / "limit exceeded").
func isQuota(err error) bool {
	if err == nil {
		return false
	}
	st := statusOf(err)
	if st != 403 && st != 400 && st != 409 {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "quota") || strings.Contains(m, "limit exceeded") || strings.Contains(m, "maximum number")
}

// isTransient reports failures worth retrying: network errors, timeouts, 429 and 5xx, and SDK errors without a
// status (connection and WebSocket failures).
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var (
		to *sdkerrors.DaytonaTimeoutError
		ne net.Error
	)
	if errors.As(err, &to) || errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	st := statusOf(err)
	return st == 0 || st == 429 || st >= 500
}

// wrap maps a Daytona error to the sentinels the manager understands (always with %w): quota to ErrQuota, transient
// failures to ErrUnavailable. Anything else (authentication, validation) is returned as is, with its message. A
// cancelled context is returned unchanged.
func wrap(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, sandbox.ErrGone), errors.Is(err, sandbox.ErrNotFound),
		errors.Is(err, sandbox.ErrNoSandbox), errors.Is(err, sandbox.ErrQuota), errors.Is(err, sandbox.ErrUnavailable):
		return err
	case isQuota(err):
		return fmt.Errorf("%w: %w", sandbox.ErrQuota, err)
	case isTransient(err):
		return fmt.Errorf("%w: %w", sandbox.ErrUnavailable, err)
	}
	return err
}
