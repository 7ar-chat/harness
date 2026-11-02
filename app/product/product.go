// Package product is what makes the HTTP API a team product: identity (OIDC),
// projects whose members share threads, an audit log and a live status feed,
// the user's assistant (supervisor), connectors (GitHub, Telegram) and the
// memory review pages. It extends hosts/httpapi through httpapi.Extension and
// does not import the engine: everything it needs comes from the facade
// (package harness), the client contract and core.
//
// Without Options.Team it adds only the memory routes, and the API behaves as
// the plain single-user server.
package product

import (
	"context"
	"net/http"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// Options configures the product.
type Options struct {
	// Team turns on team mode: projects with members, threads shared inside a
	// project, authorship of messages, the status feed. Users then come from
	// OIDC (or httpapi.Options.Authenticate).
	Team store.TeamStore
	// OIDC, with Team, adds the login routes (/auth/login, /auth/callback,
	// /auth/logout) and authenticates requests by session cookie.
	OIDC *OIDCConfig
	// Supervisor, with Team, adds the global agent (see supervisor.go).
	Supervisor *Supervisor
	// GitHub, with Team, connects projects to GitHub through a GitHub App (see github.go).
	GitHub *GitHubApp
	// Telegram, with Team, lets users talk to their assistant from Telegram (see telegram.go).
	Telegram *Telegram
	// Bus, with Team, shares the live feed between replicas (see feedbus.go): events produced by one
	// replica reach the subscribers connected to the others. Nil keeps the feed in the process.
	// harness-web uses redisx.NewFeedBus when REDIS_HOST is set.
	Bus FeedBus
	// Factory configures the routes of the agent factory (tasks, triggers, the GitHub webhook), which are
	// served when the harness was built with harness.FactoryFeature.
	Factory FactoryOptions
	// SecretKey encrypts the connector tokens kept in the store (HARNESS_SECRET_KEY, else the OIDC
	// session key). Without it, connectors are unavailable.
	SecretKey []byte
	// PublicURL is the address users reach the server at (https://harness.example.com); OAuth callbacks
	// are built from it. Default: derived from the OIDC redirect URL, else from the request.
	PublicURL string
	// Capabilities, with Team, serves the MCP marketplace routes (see capabilities.go); the store holds the
	// catalog (seeded by the command from Catalog) and the installs of projects and users. Nil: no routes.
	// Credentials a user fills in are sealed with SecretKey; without it a server that needs one cannot be set up.
	Capabilities capabilities.Store
	// MCPTester lists a server's tools for the marketplace's "test" action (nil: the action answers 501).
	MCPTester MCPTester
}

type server struct {
	*httpapi.Server
	opt  Options
	team store.TeamStore
	sup  *Supervisor
	box  *secretBox
	// capBox seals the marketplace credentials (nil without a SecretKey).
	capBox *secretBox
	tg     *telegramBridge
	hub    *hub
	oidc   *oidcAuth
}

// New returns the HTTP handler: the API of h with the product on top.
func New(h *harness.Harness, api httpapi.Options, o Options) (http.Handler, error) {
	api.Extension = &server{opt: o, team: o.Team, hub: newHub()}
	return httpapi.NewServer(h, api)
}

// Setup implements httpapi.Extension: it creates the OIDC login and takes it as the authenticator.
func (s *server) Setup(o *httpapi.Options, mux *http.ServeMux) error {
	if s.opt.OIDC == nil {
		return nil
	}
	a, err := newOIDC(context.Background(), *s.opt.OIDC, s.team)
	if err != nil {
		return err
	}
	s.oidc = a
	a.mount(mux)
	if o.Authenticate == nil {
		o.Authenticate = a.authenticator()
	}
	return nil
}

// Install implements httpapi.Extension: it adds the product's routes.
func (s *server) Install(hs *httpapi.Server) error {
	s.Server = hs
	s.routeMemory()
	if s.team == nil {
		return nil
	}
	if len(s.opt.SecretKey) > 0 {
		var err error
		if s.box, err = newSecretBox(s.opt.SecretKey); err != nil {
			return err
		}
	}
	if len(s.opt.SecretKey) > 0 && s.opt.Capabilities != nil {
		var err error
		if s.capBox, err = newSecretBoxFor(capabilitiesDomain, s.opt.SecretKey); err != nil {
			return err
		}
	}
	if s.opt.Bus != nil {
		s.hub.attach(context.Background(), s.opt.Bus)
	}
	s.routeTeam()
	s.routeCapabilities()
	s.routeFactory()
	if s.opt.Supervisor != nil {
		s.sup = s.opt.Supervisor
		s.routeSupervisor()
		s.sup.bind(s)
	}
	return nil
}

// ---- httpapi.Policy ----

var _ httpapi.Extension = (*server)(nil)

// Author is how the actor is recorded on what they send.
func (s *server) Author(c Caller) harness.Author {
	if s.team == nil || c.Actor == "" {
		return harness.Author{}
	}
	return harness.Author{Login: c.Actor, Kind: store.AuthorUser}
}

// BeforeRun applies the supervisor thread's run settings: no sandbox, approvals on.
func (s *server) BeforeRun(r *http.Request, threadID string, o *client.SendOptions) {
	s.forceSupervisorRun(r, threadID, o)
}

// BeforeResume makes the supervisor's approvals interactive and returns the tool awaiting a decision.
func (s *server) BeforeResume(r *http.Request, c Caller, threadID string, body *client.ResumeRequest) string {
	if s.sup != nil && s.sup.isSupervisorThread(r.Context(), threadID) {
		body.Interactive = true
	}
	if s.team == nil {
		return ""
	}
	return s.pendingToolName(r, c, threadID)
}

// AfterRunAction records a cancel or a steer of the run named by the route.
func (s *server) AfterRunAction(r *http.Request, c Caller, action string) {
	if s.team == nil {
		return
	}
	run, err := s.H.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		return
	}
	s.note(c, run.ThreadID, action, nil, FeedEvent{Type: "message.sent", RunID: run.ID})
}

// AfterFork puts the fork in the same project, started by the actor.
func (s *server) AfterFork(w http.ResponseWriter, r *http.Request, c Caller, th *client.Thread) bool {
	if s.team == nil {
		return false
	}
	meta := store.ThreadMeta{ThreadID: th.ID, ProjectID: c.ProjectID, Owner: c.Login, CreatedBy: c.Actor, Kind: store.ThreadChat}
	if err := s.team.SetThreadMeta(r.Context(), meta); err != nil {
		_, _ = s.H.Store.DeleteThread(r.Context(), c.Login, th.ID)
		fail(w, err)
		return true
	}
	s.note(c, th.ID, "thread.fork", map[string]any{"from": r.PathValue("id")}, FeedEvent{Type: "thread.created", Title: th.Title})
	return false
}
