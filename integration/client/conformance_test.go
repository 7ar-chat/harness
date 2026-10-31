package client_integration_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/clienttest"
	"github.com/7ar-chat/harness/hosts/httpapi"
	"github.com/7ar-chat/harness/hosts/inproc"
)

func TestInProcessConformance(t *testing.T) {
	clienttest.Run(t, func(_ *testing.T, h *harness.Harness, user string) client.Client {
		return inproc.New(h, user, inproc.WithModels(clienttest.Models, clienttest.DefaultModel))
	})
}

// TestHTTPConformance runs the same suite through the HTTP API of hosts/web wrapping the in-process
// engine; the bearer token is the login, so each client is a different user.
func TestHTTPConformance(t *testing.T) {
	clienttest.Run(t, func(t *testing.T, h *harness.Harness, user string) client.Client {
		srv := httptest.NewServer(httpapi.New(h, httpapi.Options{
			Authenticate: func(r *http.Request) (string, error) {
				return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), nil
			},
			Models: clienttest.Models, DefaultModel: clienttest.DefaultModel,
		}))
		t.Cleanup(srv.Close)
		return client.HTTP(srv.URL, user)
	})
}
