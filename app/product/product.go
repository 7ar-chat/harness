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
	"errors"
	"net/http"
	"strings"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/recipes"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/workflows"
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
	// GitHubLogin, with Team, adds "Continue with GitHub" to the sign-in page (see githublogin.go). It works with or
	// without OIDC; the accounts it creates wait for an admin's approval (see signup.go).
	GitHubLogin *GitHubLoginConfig
	// Admins are the logins or emails of the administrators: they see the admin panel, approve new accounts and are
	// emailed when someone signs up. Their own accounts are always approved.
	Admins []string
	// Mail sends the notice to the admins; nil: the admins are not emailed (the panel still lists the requests).
	Mail Mailer
	// Supervisor, with Team, adds the global agent (see supervisor.go).
	Supervisor *Supervisor
	// GitHub, with Team, connects projects to GitHub through a GitHub App (see github.go).
	GitHub *GitHubApp
	// Telegram, with Team, lets users talk to their assistant from Telegram (see telegram.go).
	Telegram *Telegram
	// Slack, with Team, lets users talk to their assistant from Slack (see slack.go).
	Slack *Slack
	// Preview, with Team, runs a project's app in the sandbox of its thread and serves it under /preview/ (see preview.go).
	Preview *Preview
	// SkillAPI, with Capabilities, serves the connections of skills that need an account (see skillconn.go).
	SkillAPI *SkillAPI
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
	// Requirements, with Team, serves requirements tracking (see requirements.go). Nil: no routes.
	Requirements requirements.Store
	// MCPTester lists a server's tools for the marketplace's "test" action (nil: the action answers 501).
	MCPTester MCPTester
	// Roles, with Team, serves the roles routes (see roles.go): the agent configurations of projects and users.
	// Nil: no routes. The command gives the harness the same store through NewRoleRegistry.
	Roles roles.Store
	// Executors, with Team, serves the external coding-agent executors and the sealed credentials users keep for them
	// (see executors.go). Nil: no routes.
	Executors *ExecutorAPI
	// Schedules, with Team, serves scheduled agent tasks and fires them (see schedules.go). Nil: no routes, nothing runs.
	Schedules *SchedulesOptions
	// Recipes, with Team, serves the recipe gallery and the recipes of the instance, projects and people (see
	// recipes.go); the builtin gallery is code. Nil: no routes.
	Recipes recipes.Store
	// Usage, with Team, serves the usage report and the spend caps (see usage.go). Nil: no routes.
	Usage *UsageOptions
	// Workflows, with Team, serves the workflow routes (see workflows.go): the procedures of projects and their
	// runs. Nil: no routes. WorkflowEngine runs them (the command builds it over the factory's task service);
	// without it a workflow can be designed but not run.
	Workflows      workflows.Store
	WorkflowEngine workflows.Engine
	// GitHosts serves the code hosts besides GitHub.com (see githosts.go): the admin's list of allowed hosts and the
	// users' tokens and SSH keys. The same value is given to the factory (FactoryOptions.Hosts). Nil: no routes.
	GitHosts *GitHosts
	// Notifications configures the notification rules, channels and delivery log (see notifications.go). Nil: the rules
	// live in memory; the defaults (the app plus each person's linked Telegram, Slack and email) still apply.
	Notifications *NotificationsOptions
	// ProjectData, with Team, serves the weekly digest and the TAPD import (see digest.go, tapd.go). Nil: no routes.
	ProjectData *ProjectDataOptions
	// TrustIdentityHeaders makes the server take the signed-in user from the headers of a trusted gateway (see
	// trustedheaders.go). Nil: off.
	TrustIdentityHeaders *TrustedHeaders
}

type server struct {
	*httpapi.Server
	roles *RoleRegistry
	opt   Options
	team  store.TeamStore
	sup   *Supervisor
	box   *secretBox
	// capBox seals the marketplace credentials (nil without a SecretKey).
	capBox *secretBox
	tg     *telegramBridge
	sk     *slackBridge
	hub    *hub
	oidc   *oidcAuth
	ghl    *githubLogin
	// active caches the sign-up state of the accounts (see signup.go).
	active activeCache
	// sched is the scheduler of scheduled agent tasks (nil without Options.Schedules).
	sched *schedulerState
	// nf is the notification engine (see notifications.go).
	nf *notifier
	// dg and tapdBox serve the weekly digest and the TAPD import (see digest.go, tapd.go).
	dg      *digestState
	tapdBox *secretBox
}

// New returns the HTTP handler: the API of h with the product on top.
func New(h *harness.Harness, api httpapi.Options, o Options) (http.Handler, error) {
	if o.Preview != nil && len(o.Preview.Key) == 0 {
		o.Preview.Key = o.SecretKey
	}
	api.Extension = &server{opt: o, team: o.Team, hub: newHub()}
	return httpapi.NewServer(h, api)
}

// Setup implements httpapi.Extension: it creates the logins (OIDC and GitHub) and takes their session as the authenticator.
// With Options.TrustIdentityHeaders, requests that come through the trusted gateway are authenticated by their headers
// first (see trustedheaders.go).
func (s *server) Setup(o *httpapi.Options, mux *http.ServeMux) error {
	if err := s.setupLogins(o, mux); err != nil {
		return err
	}
	if th := s.opt.TrustIdentityHeaders; th != nil {
		if s.team == nil {
			return errors.New("product: trusted identity headers need Options.Team")
		}
		ta, err := newTrustedAuth(*th, o.Authenticate, s.admitApproved)
		if err != nil {
			return err
		}
		o.Authenticate = ta.authenticate
	}
	return nil
}

func (s *server) setupLogins(o *httpapi.Options, mux *http.ServeMux) error {
	if s.opt.OIDC == nil && s.opt.GitHubLogin == nil {
		return nil
	}
	if s.team == nil {
		return errors.New("product: a login needs Options.Team")
	}
	var ss *sessions
	if s.opt.OIDC != nil {
		a, err := newOIDC(context.Background(), *s.opt.OIDC, s.team)
		if err != nil {
			return err
		}
		s.oidc, ss = a, &a.sessions
		a.admit = s.admitApproved
		if s.opt.OIDC.RequireApproval {
			a.admit = s.admitPending
		}
		a.mount(mux)
	}
	if g := s.opt.GitHubLogin; g != nil {
		cfg := *g
		if cfg.ClientID == "" || cfg.ClientSecret == "" {
			return errors.New("product: the GitHub login needs a client id and a secret")
		}
		if cfg.RedirectURL == "" {
			base := s.configuredBase()
			if base == "" {
				return errors.New("product: the GitHub login needs HARNESS_PUBLIC_URL or a redirect URL")
			}
			cfg.RedirectURL = base + "/auth/signin/github/callback"
		}
		if ss == nil {
			n := newSessions(cfg.SessionKey, cfg.SessionTTL, strings.HasPrefix(cfg.RedirectURL, "https://"))
			ss = &n
			mux.HandleFunc("POST /auth/logout", ss.logout)
		}
		s.ghl = newGitHubLogin(cfg, *ss, s.team, s.admitPending)
		s.ghl.mount(mux)
	}
	mux.HandleFunc("GET /auth/providers", s.providers)
	if o.Authenticate == nil {
		o.Authenticate = ss.authenticator()
	}
	return nil
}

// providers lists the ways to sign in, for the sign-in page.
func (s *server) providers(w http.ResponseWriter, _ *http.Request) {
	type provider struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	list := []provider{}
	if s.oidc != nil {
		list = append(list, provider{"oidc", "Single sign-on", "/auth/login"})
	}
	if s.ghl != nil {
		list = append(list, provider{"github", "GitHub", "/auth/signin/github"})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
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
	if err := s.setupNotifications(); err != nil {
		return err
	}
	s.routeTeam()
	s.routeNotifications()
	s.routeAdmin()
	s.routeCapabilities()
	s.routeRoles()
	s.routeExecutors()
	s.routeFactory()
	s.routeInbox()
	s.routeBoard()
	s.routeRequirements()
	s.routeSchedules()
	s.routeRecipes()
	s.routeUsage()
	s.routeInsights()
	s.routeDigest()
	s.routeActivity()
	if err := s.routeTapd(); err != nil {
		return err
	}
	s.routeWorkflows()
	s.routeGitHosts()
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
		_, _ = s.H.DeleteThread(r.Context(), c.Login, th.ID)
		fail(w, err)
		return true
	}
	s.note(c, th.ID, "thread.fork", map[string]any{"from": r.PathValue("id")}, FeedEvent{Type: "thread.created", Title: th.Title})
	return false
}
