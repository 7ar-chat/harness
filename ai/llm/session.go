package llm

import (
	"context"
	"net/http"
)

type sessionKey struct{}

// WithSession returns a context whose model calls carry id in the header named by
// Options.SessionHeader (for example x-session-id): gateways that route on a session
// send every call of a conversation to the same backend, which keeps that backend's
// prompt cache warm. Without a configured header the id is ignored.
func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}

// sessionTransport sets the session header of calls made under WithSession.
type sessionTransport struct {
	base   http.RoundTripper
	header string
}

func (t *sessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if id, _ := req.Context().Value(sessionKey{}).(string); id != "" {
		req = req.Clone(req.Context())
		req.Header.Set(t.header, id)
	}
	return t.base.RoundTrip(req)
}
