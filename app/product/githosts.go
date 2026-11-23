package product

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/githost"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/store"
)

// Code hosts besides GitHub.com: GitLab, Gitea and Forgejo, and plain git over HTTPS or SSH. An administrator
// registers the hosts that may be used (the "Git host trust" list: the web root, the SSH host keys); every user
// then keeps a personal access token and/or an SSH key per host. The agent works with a host through the same
// tools as with GitHub (open_pr, pr_status, ci_log, pr_comments: forge.Multi), the checkout clones and pushes with
// the thread's user's credential, and what is not registered is unreachable: the clone, the push and the API
// calls all refuse a host that is not in the list. Served when Options.GitHosts is set.
//
//	GET    /api/git-hosts                              → {hosts:[{name,kind,baseUrl,sshHost,sshPort,knownHosts,api,hasServiceToken,egress,credential}], admin}
//	POST   /api/admin/git-hosts {name,kind,baseUrl?,sshHost?,sshPort?,knownHosts?,note?,serviceToken?,clearServiceToken?} → 200 host   (admin)
//	DELETE /api/admin/git-hosts/{name}                 → 204                                                     (admin)
//	POST   /api/admin/git-hosts/{name}/scan-keys       → {knownHosts:[...]}  (reads the host's SSH keys; the admin checks them) (admin)
//	PUT    /api/me/git-hosts/{host}/token {token,username?} → 204   (the host refusing the token is a 400)
//	DELETE /api/me/git-hosts/{host}/token              → 204
//	POST   /api/me/git-hosts/{host}/ssh-key            → {publicKey,fingerprint}   (generates a key pair; the private key stays on the server, sealed)
//	DELETE /api/me/git-hosts/{host}/ssh-key            → 204
//
// A token or a private key is sealed with the server secret key and never returned or logged. The user's
// connector rows (kinds "git:<host>", "gitssh:<host>") hold them.

const (
	connGitToken = "git:"
	connGitSSH   = "gitssh:"
	gitHostsBox  = "harness-githosts:"
)

// GitHosts is the registry of the allowed code hosts and the credentials users keep for them. It implements
// githost.Resolver for the forge and the checkout.
type GitHosts struct {
	reg  githost.Registry
	team store.TeamStore
	box  *secretBox
	// HTTP is the client of the token check (default: one with a 15 s timeout).
	HTTP *http.Client
	// EgressAllowlist is the sandbox's network allowlist (SANDBOX_EGRESS_ALLOWLIST), empty when the sandbox is not
	// restricted by one. The admin page shows which registered hosts it does not let through.
	EgressAllowlist []string
}

var _ githost.Resolver = (*GitHosts)(nil)

// NewGitHosts returns the service. key seals the tokens and keys (HARNESS_SECRET_KEY).
func NewGitHosts(reg githost.Registry, team store.TeamStore, key []byte) (*GitHosts, error) {
	if reg == nil || team == nil {
		return nil, errors.New("git hosts need a registry and the team store")
	}
	box, err := newSecretBoxFor(gitHostsBox, key)
	if err != nil {
		return nil, errors.New("git hosts need a secret key on the server (HARNESS_SECRET_KEY)")
	}
	return &GitHosts{reg: reg, team: team, box: box}, nil
}

// Host implements githost.Resolver.
func (g *GitHosts) Host(ctx context.Context, name string) (githost.Host, error) {
	e, err := g.reg.Get(ctx, githost.Normalize(name))
	return e.Host, err
}

type storedToken struct {
	Token    string `json:"token"`
	Username string `json:"username,omitempty"`
}

// Credential implements githost.Resolver: the user's own token and SSH key, else the host's shared token.
func (g *GitHosts) Credential(ctx context.Context, login, host string) (githost.Credential, error) {
	host = githost.Normalize(host)
	var cred githost.Credential
	if login != "" {
		if cn, err := g.team.GetConnector(ctx, login, connGitToken+host); err == nil {
			if plain, err := g.box.open(cn.Data); err == nil {
				var t storedToken
				if json.Unmarshal(plain, &t) == nil {
					cred.Token, cred.Username = t.Token, t.Username
				}
			}
		}
		if cn, err := g.team.GetConnector(ctx, login, connGitSSH+host); err == nil {
			if plain, err := g.box.open(cn.Data); err == nil {
				cred.SSHKey = plain
			}
		}
	}
	if cred.Token == "" {
		if e, err := g.reg.Get(ctx, host); err == nil && len(e.SealedToken) > 0 {
			if plain, err := g.box.open(e.SealedToken); err == nil {
				var t storedToken
				if json.Unmarshal(plain, &t) == nil {
					cred.Token, cred.Username = t.Token, t.Username
				}
			}
		}
	}
	if cred.Empty() {
		return cred, githost.ErrNoCredential
	}
	return cred, nil
}

// Egress lists the host names the sandbox may need to reach for the registered hosts: add them to
// SANDBOX_EGRESS_ALLOWLIST when the sandbox runs with an allowlist.
func (g *GitHosts) Egress(ctx context.Context) []string {
	entries, err := g.reg.List(ctx)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		hosts := []string{hostOnly(e.Host.Name)}
		if sh, _ := e.Host.SSHEndpoint(); sh != "" {
			hosts = append(hosts, sh)
		}
		for _, h := range hosts {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// blockedEgress returns the names the allowlist does not let through (none when there is no allowlist).
func (g *GitHosts) blockedEgress(names []string) []string {
	if len(g.EgressAllowlist) == 0 {
		return nil
	}
	var out []string
	for _, n := range names {
		allowed := false
		for _, a := range g.EgressAllowlist {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == n || strings.HasPrefix(a, "*.") && strings.HasSuffix(n, a[1:]) {
				allowed = true
				break
			}
		}
		if !allowed {
			out = append(out, n)
		}
	}
	return out
}

func hostOnly(name string) string {
	h, _, _ := strings.Cut(name, ":")
	return h
}

// ---- routes ----

type gitCredentialView struct {
	HasToken    bool   `json:"hasToken"`
	Username    string `json:"username,omitempty"`
	HasSSHKey   bool   `json:"hasSshKey"`
	PublicKey   string `json:"publicKey,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type gitHostView struct {
	Name            string       `json:"name"`
	Kind            githost.Kind `json:"kind"`
	BaseURL         string       `json:"baseUrl"`
	SSHHost         string       `json:"sshHost,omitempty"`
	SSHPort         int          `json:"sshPort,omitempty"`
	KnownHosts      []string     `json:"knownHosts"`
	Note            string       `json:"note,omitempty"`
	API             bool         `json:"api"`
	HasServiceToken bool         `json:"hasServiceToken"`
	Egress          []string     `json:"egress"`
	// EgressBlocked are the names of Egress that the sandbox's allowlist does not let through.
	EgressBlocked []string          `json:"egressBlocked"`
	Credential    gitCredentialView `json:"credential"`
}

type gitHostList struct {
	// Enabled is false on a server without the feature (it answers 200 so a browser does not log a failed request).
	Enabled bool          `json:"enabled"`
	Hosts   []gitHostView `json:"hosts"`
	Admin   bool          `json:"admin"`
}

func (s *server) routeGitHosts() {
	if s.opt.GitHosts == nil {
		s.Route("GET /git-hosts", func(w http.ResponseWriter, _ *http.Request, _ Caller) {
			writeJSON(w, http.StatusOK, gitHostList{Hosts: []gitHostView{}})
		})
		return
	}
	s.Route("GET /git-hosts", s.listGitHosts)
	s.Route("POST /admin/git-hosts", s.putGitHost)
	s.Route("DELETE /admin/git-hosts/{name}", s.deleteGitHost)
	s.Route("POST /admin/git-hosts/{name}/scan-keys", s.scanGitHostKeys)
	s.Route("PUT /me/git-hosts/{host}/token", s.putGitToken)
	s.Route("DELETE /me/git-hosts/{host}/token", s.deleteGitToken)
	s.Route("POST /me/git-hosts/{host}/ssh-key", s.makeGitSSHKey)
	s.Route("DELETE /me/git-hosts/{host}/ssh-key", s.deleteGitSSHKey)
}

func (s *server) gitHostView(ctx context.Context, e githost.Entry, login string) gitHostView {
	g := s.opt.GitHosts
	h := e.Host
	v := gitHostView{Name: h.Name, Kind: h.Kind, BaseURL: h.BaseURL, SSHHost: h.SSHHost, SSHPort: h.SSHPort,
		KnownHosts: nonNil(h.KnownHosts), Note: h.Note, API: h.Kind.HasAPI(), HasServiceToken: len(e.SealedToken) > 0,
		Egress: nonNil([]string{hostOnly(h.Name)})}
	if sh, _ := h.SSHEndpoint(); sh != hostOnly(h.Name) {
		v.Egress = append(v.Egress, sh)
	}
	v.EgressBlocked = nonNil(g.blockedEgress(v.Egress))
	if login != "" {
		if cn, err := g.team.GetConnector(ctx, login, connGitToken+h.Name); err == nil {
			v.Credential.HasToken, v.Credential.Username = true, cn.Label
		}
		if cn, err := g.team.GetConnector(ctx, login, connGitSSH+h.Name); err == nil {
			v.Credential.HasSSHKey, v.Credential.PublicKey = true, cn.Label
			v.Credential.Fingerprint = fingerprintOf(cn.Label)
		}
	}
	return v
}

func fingerprintOf(authorized string) string {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorized))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}

func (s *server) listGitHosts(w http.ResponseWriter, r *http.Request, c Caller) {
	entries, err := s.opt.GitHosts.reg.List(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	u, err := s.currentUser(r, c)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]gitHostView, 0, len(entries))
	for _, e := range entries {
		out = append(out, s.gitHostView(r.Context(), e, c.Actor))
	}
	writeJSON(w, http.StatusOK, gitHostList{Enabled: true, Hosts: out, Admin: s.isAdmin(u.Login, u.Email)})
}

func (s *server) putGitHost(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.requireAdmin(w, r, c) {
		return
	}
	var b struct {
		githost.Host
		ServiceToken      string `json:"serviceToken"`
		ServiceUsername   string `json:"serviceUsername"`
		ClearServiceToken bool   `json:"clearServiceToken"`
	}
	if !decode(w, r, &b) {
		return
	}
	h := b.Host
	if err := h.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_host", "Cannot register this host: "+err.Error())
		return
	}
	g := s.opt.GitHosts
	prev, err := g.reg.Get(r.Context(), h.Name)
	if err != nil && !errors.Is(err, githost.ErrUnknownHost) {
		fail(w, err)
		return
	}
	e := githost.Entry{Host: h, SealedToken: prev.SealedToken}
	switch {
	case b.ClearServiceToken:
		e.SealedToken = nil
	case strings.TrimSpace(b.ServiceToken) != "":
		plain, _ := json.Marshal(storedToken{Token: strings.TrimSpace(b.ServiceToken), Username: strings.TrimSpace(b.ServiceUsername)})
		e.SealedToken = g.box.seal(plain)
	}
	if err := g.reg.Put(r.Context(), e); err != nil {
		fail(w, err)
		return
	}
	s.audit(r, c, "githost.put", map[string]any{"host": h.Name, "kind": h.Kind})
	writeJSON(w, http.StatusOK, s.gitHostView(r.Context(), e, c.Actor))
}

func (s *server) deleteGitHost(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.requireAdmin(w, r, c) {
		return
	}
	name := githost.Normalize(r.PathValue("name"))
	if err := s.opt.GitHosts.reg.Delete(r.Context(), name); err != nil {
		fail(w, err)
		return
	}
	s.audit(r, c, "githost.delete", map[string]any{"host": name})
	w.WriteHeader(http.StatusNoContent)
}

// ensureUser makes sure the caller has an account row, which a connector is stored under: a server without a login
// (team mode for one person) never creates one.
func (s *server) ensureUser(ctx context.Context, login string) error {
	if _, err := s.team.GetUser(ctx, login); !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err := s.team.UpsertUser(ctx, store.User{Login: login, Name: login})
	return err
}

// audit leaves a line in the log; the values are never secrets.
func (s *server) audit(r *http.Request, c Caller, what string, data map[string]any) {
	attrs := []any{"actor", c.Actor}
	for k, v := range data {
		attrs = append(attrs, k, v)
	}
	slog.Info(what, attrs...)
}

// scanGitHostKeys reads the SSH host keys a registered host presents. It dials only the host's own SSH endpoint;
// the administrator compares the keys with what the host publishes before saving them.
func (s *server) scanGitHostKeys(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.requireAdmin(w, r, c) {
		return
	}
	h, err := s.opt.GitHosts.Host(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown_host", "Register the host first.")
		return
	}
	keys, err := scanHostKeys(r.Context(), h)
	if err != nil || len(keys) == 0 {
		writeError(w, http.StatusBadGateway, "scan_failed", "The host's SSH keys could not be read; check the SSH host and port.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"knownHosts": keys})
}

func scanHostKeys(ctx context.Context, h githost.Host) ([]string, error) {
	host, port := h.SSHEndpoint()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var out []string
	seen := map[string]bool{}
	for _, alg := range []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512} {
		var got ssh.PublicKey
		cfg := &ssh.ClientConfig{
			User:              "git",
			HostKeyAlgorithms: []string{alg},
			HostKeyCallback:   func(_ string, _ net.Addr, k ssh.PublicKey) error { got = k; return errors.New("scan only") },
			Timeout:           8 * time.Second,
		}
		d := net.Dialer{Timeout: 8 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return out, err
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		c, _, _, err := ssh.NewClientConn(conn, addr, cfg)
		if c != nil {
			_ = c.Close()
		}
		_ = conn.Close()
		_ = err // the callback refuses on purpose
		if got == nil {
			continue
		}
		line := knownHostsLine(host, port, got)
		if !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out, nil
}

func knownHostsLine(host string, port int, k ssh.PublicKey) string {
	name := host
	if port != 22 {
		name = "[" + host + "]:" + strconv.Itoa(port)
	}
	return name + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

// ---- the user's credentials ----

// registeredHost resolves {host} of the path to a registered host or answers 404.
func (s *server) registeredHost(w http.ResponseWriter, r *http.Request) (githost.Host, bool) {
	h, err := s.opt.GitHosts.Host(r.Context(), r.PathValue("host"))
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown_host", "This git host is not registered. Ask an administrator to add it.")
		return githost.Host{}, false
	}
	return h, true
}

func (s *server) putGitToken(w http.ResponseWriter, r *http.Request, c Caller) {
	h, ok := s.registeredHost(w, r)
	if !ok {
		return
	}
	var b struct {
		Token    string `json:"token"`
		Username string `json:"username"`
	}
	if !decode(w, r, &b) {
		return
	}
	b.Token, b.Username = strings.TrimSpace(b.Token), strings.TrimSpace(b.Username)
	if b.Token == "" || len(b.Token) > 4096 || strings.ContainsAny(b.Token+b.Username, "\r\n\x00") {
		writeError(w, http.StatusBadRequest, "invalid_token", "Paste the personal access token.")
		return
	}
	g := s.opt.GitHosts
	if status, err := g.checkToken(r.Context(), h, b.Token); err != nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		writeError(w, http.StatusBadRequest, "token_refused", "The host refused this token.")
		return
	}
	plain, _ := json.Marshal(storedToken{Token: b.Token, Username: b.Username})
	err := s.ensureUser(r.Context(), c.Actor)
	if err == nil {
		err = s.team.SetConnector(r.Context(), store.Connector{Login: c.Actor, Kind: connGitToken + h.Name, Account: c.Actor,
			Label: b.Username, Data: g.box.seal(plain), CreatedAt: time.Now()})
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(r, c, "githost.token.set", map[string]any{"host": h.Name})
	w.WriteHeader(http.StatusNoContent)
}

// checkToken asks the host who the token is; it returns the HTTP status when the host answered. Hosts without an
// API, and network failures, are not a refusal.
func (g *GitHosts) checkToken(ctx context.Context, h githost.Host, token string) (int, error) {
	if !h.Kind.HasAPI() {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.APIBase()+"/user", nil)
	if err != nil {
		return 0, err
	}
	if h.Kind == githost.KindGitLab {
		req.Header.Set("PRIVATE-TOKEN", token)
	} else {
		req.Header.Set("Authorization", "token "+token)
	}
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func (s *server) deleteGitToken(w http.ResponseWriter, r *http.Request, c Caller) {
	h, ok := s.registeredHost(w, r)
	if !ok {
		return
	}
	if err := s.team.DeleteConnector(r.Context(), c.Actor, connGitToken+h.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) makeGitSSHKey(w http.ResponseWriter, r *http.Request, c Caller) {
	h, ok := s.registeredHost(w, r)
	if !ok {
		return
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail(w, err)
		return
	}
	block, err := ssh.MarshalPrivateKey(priv, "harness:"+c.Actor+"@"+h.Name)
	if err != nil {
		fail(w, err)
		return
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		fail(w, err)
		return
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " harness:" + c.Actor
	pemBytes := pem.EncodeToMemory(block)
	err = s.ensureUser(r.Context(), c.Actor)
	if err == nil {
		err = s.team.SetConnector(r.Context(), store.Connector{Login: c.Actor, Kind: connGitSSH + h.Name, Account: c.Actor,
			Label: line, Data: s.opt.GitHosts.box.seal(pemBytes), CreatedAt: time.Now()})
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.audit(r, c, "githost.sshkey.create", map[string]any{"host": h.Name})
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": line, "fingerprint": ssh.FingerprintSHA256(sp)})
}

func (s *server) deleteGitSSHKey(w http.ResponseWriter, r *http.Request, c Caller) {
	h, ok := s.registeredHost(w, r)
	if !ok {
		return
	}
	if err := s.team.DeleteConnector(r.Context(), c.Actor, connGitSSH+h.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- requirements import from a registered host ----

// importIssues imports the open issues of the project's repository into its requirements, from whichever host the
// repository is on: GitHub through the App, GitLab and Gitea through the host's API with the caller's token.
//
//	POST /api/projects/{pid}/requirements/import/issues {label?} → {imported, skipped, requirements}   (member)
func (s *server) importIssues(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	var b struct {
		Label string `json:"label"`
	}
	if !decode(w, r, &b) {
		return
	}
	label := strings.TrimSpace(b.Label)
	if _, _, gh := ParseRepo(p.RepoURL); gh || s.opt.GitHosts == nil {
		r.Body = io.NopCloser(strings.NewReader(`{"label":` + strconv.Quote(label) + `}`))
		s.importRequirements(w, r, c)
		return
	}
	ref, parsed := githost.ParseRepoURL(p.RepoURL)
	if !parsed || ref.Host == "" {
		writeError(w, http.StatusConflict, "no_issue_source", "This project's repository is not on a host issues can be imported from.")
		return
	}
	host, err := s.opt.GitHosts.Host(r.Context(), ref.Host)
	if err != nil {
		writeError(w, http.StatusConflict, "unknown_host", ref.Host+" is not a registered git host. Ask an administrator to add it.")
		return
	}
	src, ok := harness.HostIssueSource(host, s.opt.GitHosts, s.opt.GitHosts.HTTP)
	if !ok {
		writeError(w, http.StatusConflict, "no_issue_source", host.Name+" is a plain git host: it has no issues to import.")
		return
	}
	issues, err := src.OpenIssues(githost.WithLogin(r.Context(), c.Actor), ref.Repo(), label, 300)
	if err != nil {
		slog.Warn("requirements: listing issues failed", "project", p.ID, "host", host.Name, "error", harness.RedactSecrets(err.Error()))
		msg := "The host could not be reached; try again."
		if errors.Is(err, githost.ErrNoCredential) || strings.Contains(err.Error(), "Git access") {
			msg = "Add a personal access token for " + host.Name + " in Profile, Git access."
		}
		writeError(w, http.StatusBadGateway, "githost_error", msg)
		return
	}
	out := RequirementImport{Requirements: []requirements.Requirement{}}
	for _, is := range issues {
		created, isNew, err := s.opt.Requirements.Create(r.Context(), requirements.Requirement{
			ProjectID: p.ID, Title: is.Title, Body: is.Body, Labels: is.Labels, Author: c.Actor,
			Source: requirements.Origin{Kind: src.Source(), Repo: ref.Repo(), Number: is.Number, URL: is.URL},
		})
		if errors.Is(err, requirements.ErrInvalid) {
			out.Skipped++
			continue
		}
		if err != nil {
			fail(w, err)
			return
		}
		if !isNew {
			out.Skipped++
			continue
		}
		out.Imported++
		out.Requirements = append(out.Requirements, *created)
	}
	s.note(inProject(c, p.ID), "", "requirement.import", map[string]any{"imported": out.Imported, "skipped": out.Skipped, "host": host.Name}, FeedEvent{Type: "requirement.changed"})
	writeJSON(w, http.StatusOK, out)
}
