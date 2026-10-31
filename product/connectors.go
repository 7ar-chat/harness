package product

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// Connectors link a user to outside accounts.
//
//	GET    /api/connectors                      → [{kind, configured, connected, account?, label?, message?, connectUrl?, pending?}]
//	GET    /api/connectors/github/connect       → redirects through GitHub (OAuth) and back to /#/profile
//	DELETE /api/connectors/github
//	GET    /api/connectors/github/repos?q=      → {repos:[{fullName, private, description, defaultBranch, installationId, imported?}], installUrl}
//	POST   /api/projects/import {repo}          → 201 project (the user's repository, with its GitHub App installation)
//	POST   /api/connectors/telegram/link        → {code, url, expiresAt}
//	POST   /api/connectors/telegram/test        → 204
//	DELETE /api/connectors/telegram
//
// GitHub needs the GitHub App's client id and secret (the App's "user
// authorization"): a user token can only see repositories the App is
// installed on and the user can access, which is exactly what can be imported.
// Tokens are stored encrypted (SecretKey).

type connectorView struct {
	Kind       string       `json:"kind"`
	Configured bool         `json:"configured"`
	Connected  bool         `json:"connected"`
	Account    string       `json:"account,omitempty"`
	Label      string       `json:"label,omitempty"`
	Message    string       `json:"message,omitempty"`
	ConnectURL string       `json:"connectUrl,omitempty"`
	Pending    *linkPending `json:"pending,omitempty"`
}

func (s *server) routeConnectors() {
	s.Route("GET /connectors", s.listConnectors)
	s.Route("GET /connectors/github/connect", s.githubConnectStart)
	s.Route("DELETE /connectors/github", s.githubDisconnect)
	s.Route("GET /connectors/github/repos", s.githubRepos)
	s.Route("POST /projects/import", s.importProject)
	s.Route("POST /connectors/telegram/link", s.telegramLink)
	s.Route("POST /connectors/telegram/test", s.telegramTest)
	s.Route("DELETE /connectors/telegram", s.telegramDisconnect)
	for _, prefix := range []string{"/api", ""} { // the OAuth return: a browser navigation, cookie-authenticated
		s.Mux.HandleFunc("GET "+prefix+"/auth/github/callback", s.githubCallback)
	}
	if s.opt.Telegram != nil && s.box != nil {
		s.tg = newTelegramBridge(s)
		go s.tg.run(context.Background())
	}
}

func (s *server) listConnectors(w http.ResponseWriter, r *http.Request, c Caller) {
	conns, err := s.team.ListConnectors(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	by := map[string]store.Connector{}
	for _, cn := range conns {
		by[cn.Kind] = cn
	}
	gh := connectorView{Kind: store.ConnectorGitHub}
	switch g := s.opt.GitHub; {
	case s.box == nil:
		gh.Message = "Connectors need a secret key on the server (HARNESS_SECRET_KEY)."
	case g == nil || g.ClientID == "" || g.ClientSecret == "":
		gh.Message = "The server has no GitHub App with OAuth credentials (GITHUB_APP_CLIENT_ID / GITHUB_APP_CLIENT_SECRET)."
	default:
		gh.Configured, gh.ConnectURL = true, "/api/connectors/github/connect"
	}
	if cn, ok := by[store.ConnectorGitHub]; ok {
		gh.Connected, gh.Account, gh.Label = true, cn.Account, cn.Label
	}
	tg := connectorView{Kind: store.ConnectorTelegram}
	switch {
	case s.box == nil:
		tg.Message = "Connectors need a secret key on the server (HARNESS_SECRET_KEY)."
	case s.tg == nil:
		tg.Message = "The server has no Telegram bot (TELEGRAM_BOT_TOKEN)."
	default:
		tg.Configured = true
		tg.Pending = s.tg.pendingFor(c.Actor)
	}
	if cn, ok := by[store.ConnectorTelegram]; ok {
		tg.Connected, tg.Account, tg.Label, tg.Pending = true, cn.Account, cn.Label, nil
	}
	writeJSON(w, http.StatusOK, []connectorView{gh, tg})
}

// ---- GitHub ----

func (s *server) publicBase(r *http.Request) string {
	if s.opt.PublicURL != "" {
		return strings.TrimRight(s.opt.PublicURL, "/")
	}
	if s.opt.OIDC != nil {
		if u, err := url.Parse(s.opt.OIDC.RedirectURL); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type oauthState struct {
	Login string `json:"l"`
	Exp   int64  `json:"e"`
}

func (s *server) githubConfigured() bool {
	return s.box != nil && s.opt.GitHub != nil && s.opt.GitHub.ClientID != "" && s.opt.GitHub.ClientSecret != ""
}

func (s *server) githubReady(w http.ResponseWriter) bool {
	if !s.githubConfigured() {
		writeError(w, http.StatusNotImplemented, "not_configured", "GitHub connections are not configured on this server.")
		return false
	}
	return true
}

func (s *server) githubConnectStart(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.githubReady(w) {
		return
	}
	st, _ := json.Marshal(oauthState{c.Actor, time.Now().Add(10 * time.Minute).Unix()})
	g := s.opt.GitHub
	q := url.Values{"client_id": {g.ClientID}, "redirect_uri": {s.publicBase(r) + "/auth/github/callback"},
		"state": {encodeState(s.box.seal(st))}}
	http.Redirect(w, r, g.web()+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
}

func encodeState(b []byte) string { return b64(b) }

type githubTokens struct {
	Access  string    `json:"access"`
	Refresh string    `json:"refresh,omitempty"`
	Expires time.Time `json:"expires,omitempty"`
}

func (g *GitHubApp) tokenRequest(ctx context.Context, form url.Values) (githubTokens, error) {
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.web()+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return githubTokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.client().Do(req)
	if err != nil {
		return githubTokens{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int    `json:"expires_in"`
		Error   string `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(b, &out); err != nil || out.Access == "" {
		return githubTokens{}, fmt.Errorf("github oauth: %s", out.Error)
	}
	t := githubTokens{Access: out.Access, Refresh: out.Refresh}
	if out.Expires > 0 {
		t.Expires = time.Now().Add(time.Duration(out.Expires) * time.Second)
	}
	return t, nil
}

func (s *server) githubCallback(w http.ResponseWriter, r *http.Request) {
	login, err := s.Auth(r)
	if err != nil || login == "" {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape("/#/profile"), http.StatusFound)
		return
	}
	back := func(msg string) { http.Redirect(w, r, "/#/profile?github="+url.QueryEscape(msg), http.StatusFound) }
	if !s.githubConfigured() {
		back("not_configured")
		return
	}
	raw, derr := decodeB64(r.URL.Query().Get("state"))
	var st oauthState
	if plain, oerr := s.box.open(raw); derr != nil || oerr != nil || json.Unmarshal(plain, &st) != nil || st.Login != login || time.Now().Unix() > st.Exp {
		back("invalid_state")
		return
	}
	g := s.opt.GitHub
	tok, err := g.tokenRequest(r.Context(), url.Values{"code": {r.URL.Query().Get("code")}, "redirect_uri": {s.publicBase(r) + "/auth/github/callback"}})
	if err != nil {
		back("failed")
		return
	}
	var me struct {
		Login string `json:"login"`
	}
	if _, err := g.do(r.Context(), http.MethodGet, "/user", tok.Access, nil, &me); err != nil || me.Login == "" {
		back("failed")
		return
	}
	data, _ := json.Marshal(tok)
	err = s.team.SetConnector(r.Context(), store.Connector{Login: login, Kind: store.ConnectorGitHub, Account: strings.ToLower(me.Login),
		Label: "@" + me.Login, Data: s.box.seal(data)})
	switch {
	case errors.Is(err, store.ErrConflict):
		back("account_in_use")
	case err != nil:
		back("failed")
	default:
		s.note(Caller{Actor: login}, "", "connector.github", nil, FeedEvent{Type: "project.changed", affects: login})
		back("connected")
	}
}

func decodeB64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func (s *server) githubDisconnect(w http.ResponseWriter, r *http.Request, c Caller) {
	if err := s.team.DeleteConnector(r.Context(), c.Actor, store.ConnectorGitHub); err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// githubUserToken returns the user's GitHub token, refreshed when it expired.
func (s *server) githubUserToken(ctx context.Context, login string) (string, error) {
	cn, err := s.team.GetConnector(ctx, login, store.ConnectorGitHub)
	if err != nil {
		return "", err
	}
	plain, err := s.box.open(cn.Data)
	if err != nil {
		return "", err
	}
	var t githubTokens
	if err := json.Unmarshal(plain, &t); err != nil {
		return "", err
	}
	if !t.Expires.IsZero() && time.Until(t.Expires) < time.Minute {
		if t.Refresh == "" {
			return "", errGitHubExpired
		}
		nt, err := s.opt.GitHub.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.Refresh}})
		if err != nil {
			return "", errGitHubExpired
		}
		data, _ := json.Marshal(nt)
		cn.Data = s.box.seal(data)
		if err := s.team.SetConnector(ctx, *cn); err != nil {
			return "", err
		}
		t = nt
	}
	return t.Access, nil
}

var errGitHubExpired = errors.New("the GitHub connection expired: connect again")

type repoView struct {
	FullName       string `json:"fullName"`
	Private        bool   `json:"private"`
	Description    string `json:"description"`
	DefaultBranch  string `json:"defaultBranch"`
	InstallationID int64  `json:"installationId"`
	Imported       string `json:"imported,omitempty"`
}

// userRepos lists the repositories the user can reach through the App's installations.
func (s *server) userRepos(ctx context.Context, login string) ([]repoView, error) {
	tok, err := s.githubUserToken(ctx, login)
	if err != nil {
		return nil, err
	}
	g := s.opt.GitHub
	var inst struct {
		Installations []struct {
			ID int64 `json:"id"`
		} `json:"installations"`
	}
	if _, err := g.do(ctx, http.MethodGet, "/user/installations?per_page=100", tok, nil, &inst); err != nil {
		return nil, err
	}
	var out []repoView
	for _, in := range inst.Installations {
		var page struct {
			Repositories []struct {
				FullName      string `json:"full_name"`
				Private       bool   `json:"private"`
				Description   string `json:"description"`
				DefaultBranch string `json:"default_branch"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/user/installations/%d/repositories?per_page=100", in.ID)
		if _, err := g.do(ctx, http.MethodGet, path, tok, nil, &page); err != nil {
			return nil, err
		}
		for _, rp := range page.Repositories {
			out = append(out, repoView{rp.FullName, rp.Private, rp.Description, rp.DefaultBranch, in.ID, ""})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return out, nil
}

func (s *server) githubRepoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusConflict, "not_connected", "Connect your GitHub account first.")
	case errors.Is(err, errGitHubExpired):
		writeError(w, http.StatusConflict, "expired", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "github_error", "GitHub could not be reached; try again.")
	}
}

func (s *server) githubRepos(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.githubReady(w) {
		return
	}
	repos, err := s.userRepos(r.Context(), c.Actor)
	if err != nil {
		s.githubRepoError(w, err)
		return
	}
	imported := map[string]string{}
	if list, err := s.team.ListProjects(r.Context(), c.Actor); err == nil {
		for _, p := range list {
			if o, n, ok := ParseRepo(p.RepoURL); ok {
				imported[strings.ToLower(o+"/"+n)] = p.ID
			}
		}
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := []repoView{}
	for _, rp := range repos {
		if q != "" && !strings.Contains(strings.ToLower(rp.FullName+" "+rp.Description), q) {
			continue
		}
		rp.Imported = imported[strings.ToLower(rp.FullName)]
		out = append(out, rp)
	}
	res := map[string]any{"repos": out}
	if s.opt.GitHub.Slug != "" {
		res["installUrl"] = "https://github.com/apps/" + s.opt.GitHub.Slug + "/installations/new"
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) importProject(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.githubReady(w) {
		return
	}
	var body struct {
		Repo string `json:"repo"`
	}
	if !decode(w, r, &body) {
		return
	}
	// The repository must be one the user can reach: look it up, never trust the client's installation id.
	repos, err := s.userRepos(r.Context(), c.Actor)
	if err != nil {
		s.githubRepoError(w, err)
		return
	}
	var found *repoView
	for i := range repos {
		if strings.EqualFold(repos[i].FullName, strings.TrimSpace(body.Repo)) {
			found = &repos[i]
		}
	}
	if found == nil {
		writeError(w, http.StatusNotFound, "not_found", "That repository is not available to you through the GitHub App.")
		return
	}
	repoURL := "https://github.com/" + found.FullName
	if list, err := s.team.ListProjects(r.Context(), c.Actor); err == nil {
		for _, p := range list {
			if o, n, ok := ParseRepo(p.RepoURL); ok && strings.EqualFold(o+"/"+n, found.FullName) {
				writeError(w, http.StatusConflict, "already_imported", "You already have a project for this repository.")
				return
			}
		}
	}
	name := found.FullName[strings.Index(found.FullName, "/")+1:]
	p, err := s.team.CreateProject(r.Context(), store.Project{Name: name, Description: found.Description, RepoURL: repoURL,
		DefaultBranch: found.DefaultBranch, GitHubInstallation: found.InstallationID}, c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	s.note(Caller{Actor: c.Actor, ProjectID: p.ID}, "", "project.import", map[string]any{"repo": found.FullName}, FeedEvent{Type: "project.changed", affects: c.Actor})
	if s.H.Memory != nil { // the repository's AGENTS.md becomes the project's pinned memory
		proj := *p
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := s.syncAgentsMD(ctx, &proj, c.Actor); err != nil && !errors.Is(err, errNoRepoFile) {
				slog.Warn("project import: AGENTS.md not imported", "project", proj.ID, "error", err)
			}
		}()
	}
	writeJSON(w, http.StatusCreated, projectView{*p, store.RoleOwner})
}
