package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/hosts/client"
	"github.com/GhaziBenDahmane/harness/hosts/client/clienttest"
	httpserver "github.com/GhaziBenDahmane/harness/hosts/web"
)

func TestInProcessConformance(t *testing.T) {
	clienttest.Run(t, func(_ *testing.T, h *harness.Harness, user string) client.Client {
		return client.InProcess(h, user, client.WithModels(clienttest.Models, clienttest.DefaultModel))
	})
}

// TestHTTPConformance runs the same suite through the HTTP API of hosts/web wrapping the in-process
// engine; the bearer token is the login, so each client is a different user.
func TestHTTPConformance(t *testing.T) {
	clienttest.Run(t, func(t *testing.T, h *harness.Harness, user string) client.Client {
		srv := httptest.NewServer(httpserver.New(h, httpserver.Options{
			Authenticate: func(r *http.Request) (string, error) {
				return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), nil
			},
			Models: clienttest.Models, DefaultModel: clienttest.DefaultModel,
		}))
		t.Cleanup(srv.Close)
		return client.HTTP(srv.URL, user)
	})
}
