package opensandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
	"github.com/7ar-chat/harness/core/sandbox"
)

func TestNewOpenSandboxRejectsAnUnusableURL(t *testing.T) {
	for _, url := range []string{"", "not a url", "http://", "://x"} {
		// Act
		b, err := opensandbox.NewOpenSandbox(opensandbox.OpenSandboxOptions{URL: url})

		// Assert
		if err == nil || b != nil {
			t.Errorf("URL %q: backend %v, err %v; want an error", url, b, err)
		}
	}
}

func TestNewOpenSandboxKeepsAPathPrefixOfTheServerURL(t *testing.T) {
	// A server behind a reverse proxy lives under a path; lifecycle calls must keep it.
	// Arrange
	f := newFakeOSB(t)
	b, err := opensandbox.NewOpenSandbox(opensandbox.OpenSandboxOptions{URL: f.srv.URL + "/osb/"})
	if err != nil {
		t.Fatal(err)
	}
	f.reply("GET /osb/v1/sandboxes/sbx-1/endpoints/44772", 200, `{"endpoint":"`+f.host()+`/execd"}`)
	f.reply("GET /osb/v1/sandboxes/sbx-1", 200, runningSandboxJSON)

	// Act
	inst, err := b.Connect(context.Background(), "sbx-1")

	// Assert
	if err != nil {
		t.Fatalf("Connect through a path prefix: %v", err)
	}
	if gone := inst.Gone(context.Background()); gone {
		t.Fatal("a running sandbox reported gone")
	}
	if n := len(f.requests("GET /osb/v1/sandboxes/sbx-1")); n != 1 {
		t.Fatalf("lifecycle info requests under the prefix = %d, want 1", n)
	}
}

func TestCreateSendsTheConfiguredSandboxSpec(t *testing.T) {
	// The sandbox is created with the image, limits, lifetime and the caller's metadata, and starts under the egress policy.
	// Arrange
	f := newFakeOSB(t)
	o := f.opts()
	o.EgressAllowlist = []string{"pypi.org"}
	b, err := opensandbox.NewOpenSandbox(o)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	inst, err := b.Create(context.Background(), sandbox.CreateOptions{Metadata: map[string]string{"thread": "t-1"}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID() != "sbx-1" {
		t.Fatalf("ID = %q", inst.ID())
	}
	reqs := f.requests("POST /v1/sandboxes")
	if len(reqs) != 1 {
		t.Fatalf("create requests = %d", len(reqs))
	}
	var got struct {
		Image          struct{ URI string }
		ResourceLimits map[string]string
		Timeout        int
		Metadata       map[string]string
		NetworkPolicy  struct {
			DefaultAction string
			Egress        []struct{ Action, Target string }
		}
		CredentialProxy *struct{ Enabled bool }
	}
	if err := json.Unmarshal(reqs[0].Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Image.URI != "img:1" || got.ResourceLimits["cpu"] != "1" || got.ResourceLimits["memory"] != "1Gi" || got.Timeout != 60 {
		t.Errorf("spec = %+v", got)
	}
	if got.Metadata["thread"] != "t-1" {
		t.Errorf("metadata = %v", got.Metadata)
	}
	if got.NetworkPolicy.DefaultAction != "deny" || len(got.NetworkPolicy.Egress) != 1 || got.NetworkPolicy.Egress[0].Target != "pypi.org" {
		t.Errorf("network policy = %+v", got.NetworkPolicy)
	}
	if got.CredentialProxy != nil {
		t.Errorf("credentialProxy sent without being configured: %+v", got.CredentialProxy)
	}
}

func TestCreateDoesNotLeakASandboxThatNeverBecomesUsable(t *testing.T) {
	// A sandbox that dies while starting is deleted again, and the caller learns why.
	// Arrange
	f := newFakeOSB(t)
	f.setState("Failed")

	// Act
	inst, err := f.backend(t).Create(context.Background(), sandbox.CreateOptions{})

	// Assert
	if err == nil || inst != nil {
		t.Fatalf("Create = %v, %v; want an error", inst, err)
	}
	if n := len(f.requests("DELETE /v1/sandboxes/sbx-1")); n != 1 {
		t.Fatalf("cleanup deletes = %d, want 1", n)
	}
}

func TestCreateDeletesTheSandboxWhenItsExecdNeverAnswers(t *testing.T) {
	// The health check (execd ping) failing is a failed create, not a half-built sandbox handed to the agent.
	// Arrange
	f := newFakeOSB(t)
	f.reply("GET /execd/ping", 500, `{"code":"BOOT","message":"execd not up"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// Act
	inst, err := f.backend(t).Create(ctx, sandbox.CreateOptions{})

	// Assert
	if err == nil || inst != nil {
		t.Fatalf("Create = %v, %v; want an error", inst, err)
	}
	if n := len(f.requests("DELETE /v1/sandboxes/sbx-1")); n != 1 {
		t.Fatalf("cleanup deletes = %d, want 1", n)
	}
}

func TestCreateReportsServerAndCancellationErrors(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	f.reply("POST /v1/sandboxes", 500, `{"code":"BOOM","message":"scheduler down"}`)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	g := newFakeOSB(t)

	// Act
	_, serverErr := f.backend(t).Create(context.Background(), sandbox.CreateOptions{})
	_, cancelErr := g.backend(t).Create(cancelled, sandbox.CreateOptions{})

	// Assert
	if serverErr == nil || sandbox.IsGone(serverErr) {
		t.Errorf("a 500 from the scheduler: %v", serverErr)
	}
	if !errors.Is(cancelErr, context.Canceled) {
		t.Errorf("a cancelled create: %v", cancelErr)
	}
}

func TestCreateWithTheCredentialProxyAsksForTheVault(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	o := f.opts()
	o.CredentialProxy = true
	b, err := opensandbox.NewOpenSandbox(o)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	inst, err := b.Create(context.Background(), sandbox.CreateOptions{})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.BrokerOf(inst) == nil {
		t.Fatal("a credential-proxy sandbox exposes no broker")
	}
	var got struct{ CredentialProxy struct{ Enabled bool } }
	_ = json.Unmarshal(f.requests("POST /v1/sandboxes")[0].Body, &got)
	if !got.CredentialProxy.Enabled {
		t.Fatalf("create body %s", f.requests("POST /v1/sandboxes")[0].Body)
	}
}

func TestConnectTellsAGoneSandboxFromATransientFailure(t *testing.T) {
	// Only a definitive answer from the lifecycle API means "no sandbox"; anything else keeps the workspace and is retried later.
	for _, c := range []struct {
		name  string
		setup func(f *fakeOSB)
		want  error // nil: any error that is not ErrNoSandbox
	}{
		{"endpoint 404 sandbox not found", func(f *fakeOSB) {
			f.reply("GET /v1/sandboxes/sbx-1/endpoints/44772", 404, notFoundBody)
		}, sandbox.ErrNoSandbox},
		{"endpoint fails but the lifecycle reports Terminated", func(f *fakeOSB) {
			f.reply("GET /v1/sandboxes/sbx-1/endpoints/44772", 500, `{"code":"X","message":"x"}`)
			f.setState("Terminated")
		}, sandbox.ErrNoSandbox},
		{"endpoint fails but the lifecycle reports Failed", func(f *fakeOSB) {
			f.reply("GET /v1/sandboxes/sbx-1/endpoints/44772", 500, `{"code":"X","message":"x"}`)
			f.setState("Failed")
		}, sandbox.ErrNoSandbox},
		{"endpoint fails while the sandbox is still running", func(f *fakeOSB) {
			f.reply("GET /v1/sandboxes/sbx-1/endpoints/44772", 500, `{"code":"X","message":"x"}`)
		}, nil},
		{"endpoint is garbage and the lifecycle is unknown", func(f *fakeOSB) {
			f.reply("GET /v1/sandboxes/sbx-1/endpoints/44772", 200, `<html>proxy error</html>`)
			f.reply("GET /v1/sandboxes/sbx-1", 500, `{"code":"X","message":"x"}`)
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			f := newFakeOSB(t)
			c.setup(f)

			// Act
			inst, err := f.backend(t).Connect(context.Background(), "sbx-1")

			// Assert
			if inst != nil || err == nil {
				t.Fatalf("Connect = %v, %v; want an error", inst, err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.want == nil && errors.Is(err, sandbox.ErrNoSandbox) {
				t.Fatalf("a transient failure was reported as a gone sandbox: %v", err)
			}
		})
	}
}

func TestConnectAnEmptyIDIsAnError(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)

	// Act
	_, err := f.backend(t).Connect(context.Background(), "")

	// Assert
	if err == nil || errors.Is(err, sandbox.ErrNoSandbox) {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectGivesUpWhenTheCallerCancels(t *testing.T) {
	// A cancelled caller is not "the sandbox is gone": its workspace must be kept.
	// Arrange
	f := newFakeOSB(t)
	f.handle("GET /v1/sandboxes/sbx-1/endpoints/44772", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	// Act
	_, err := f.backend(t).Connect(ctx, "sbx-1")

	// Assert
	if err == nil || errors.Is(err, sandbox.ErrNoSandbox) {
		t.Fatalf("err = %v, want a transient error", err)
	}
}

func TestKillIsIdempotentButReportsRealFailures(t *testing.T) {
	// Arrange
	ok, missing, broken := newFakeOSB(t), newFakeOSB(t), newFakeOSB(t)
	missing.reply("DELETE /v1/sandboxes/sbx-1", 404, notFoundBody)
	broken.reply("DELETE /v1/sandboxes/sbx-1", 500, `{"code":"BOOM","message":"no"}`)
	ctx := context.Background()

	// Act
	okErr := ok.backend(t).Kill(ctx, "sbx-1")
	missingErr := missing.backend(t).Kill(ctx, "sbx-1")
	brokenErr := broken.backend(t).Kill(ctx, "sbx-1")

	// Assert
	if okErr != nil {
		t.Errorf("kill of a live sandbox: %v", okErr)
	}
	if missingErr != nil {
		t.Errorf("killing an already-removed sandbox must succeed: %v", missingErr)
	}
	if brokenErr == nil {
		t.Error("a failed kill was reported as success")
	}
	if len(ok.requests("DELETE /v1/sandboxes/sbx-1")) != 1 {
		t.Error("no delete reached the server")
	}
}

func TestInstanceGoneIsDefinitiveOnlyForTerminalStatesAnd404(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(f *fakeOSB)
		want  bool
	}{
		{"running", func(f *fakeOSB) {}, false},
		{"paused", func(f *fakeOSB) { f.setState("Paused") }, false},
		{"terminated", func(f *fakeOSB) { f.setState("Terminated") }, true},
		{"failed", func(f *fakeOSB) { f.setState("Failed") }, true},
		{"lifecycle 404", func(f *fakeOSB) { f.reply("GET /v1/sandboxes/sbx-1", 404, notFoundBody) }, true},
		{"lifecycle 500 is not an answer", func(f *fakeOSB) { f.reply("GET /v1/sandboxes/sbx-1", 500, `{"code":"X","message":"x"}`) }, false},
		{"lifecycle returns garbage", func(f *fakeOSB) { f.reply("GET /v1/sandboxes/sbx-1", 200, `not json`) }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			f := newFakeOSB(t)
			inst := f.instance(t)
			c.setup(f)

			// Act
			got := inst.Gone(context.Background())

			// Assert
			if got != c.want {
				t.Fatalf("Gone = %v, want %v", got, c.want)
			}
		})
	}
}

func TestInstanceHealthFollowsTheExecdPing(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	healthy := inst.Healthy(context.Background())
	f.reply("GET /execd/ping", 500, `{"code":"X","message":"down"}`)

	// Act
	unhealthy := inst.Healthy(context.Background())

	// Assert
	if !healthy || unhealthy {
		t.Fatalf("healthy = %v then %v, want true then false", healthy, unhealthy)
	}
}

func TestRenewExtendsTheExpiryByTheGivenDuration(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	before := time.Now()

	// Act
	err := inst.Renew(context.Background(), 2*time.Hour)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ ExpiresAt time.Time }
	if err := json.Unmarshal(f.requests("POST /v1/sandboxes/sbx-1/renew-expiration")[0].Body, &got); err != nil {
		t.Fatal(err)
	}
	if d := got.ExpiresAt.Sub(before); d < 119*time.Minute || d > 121*time.Minute {
		t.Fatalf("expiry is %v from now, want about 2h", d)
	}
}

func TestRenewReportsAServerRefusal(t *testing.T) {
	// Arrange
	f := newFakeOSB(t)
	inst := f.instance(t)
	f.reply("POST /v1/sandboxes/sbx-1/renew-expiration", 409, `{"code":"TOO_LONG","message":"max lifetime"}`)

	// Act
	err := inst.Renew(context.Background(), time.Hour)

	// Assert
	if err == nil {
		t.Fatal("a refused renewal was reported as success")
	}
}

func TestBackendFromSettingsRejectsAnUnparsableURL(t *testing.T) {
	// Arrange
	s := backends.OpenSandboxSettings{URL: "http://"}

	// Act
	b, err := opensandbox.BackendFromSettings(s)

	// Assert
	if err == nil || b != nil {
		t.Fatalf("BackendFromSettings = %v, %v; want an error and no backend", b, err)
	}
}
