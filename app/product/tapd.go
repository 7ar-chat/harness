package product

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/netguard"
	"github.com/7ar-chat/harness/core/projdata"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/tapd"
)

// TAPD (Tencent's project tool) as a source of requirements. A project's owner connects one TAPD workspace; members
// import its open stories (core/tapd explains how "open" is decided and what is assumed). The credentials are sealed
// with the server key, kept in core/projdata and never returned or logged.
//
//	GET    /api/projects/{pid}/requirements/tapd         → TapdStatus                          (project viewer+)
//	PUT    /api/projects/{pid}/requirements/tapd {workspaceId,baseUrl?,user?,password?,token?,openStatuses?} → TapdStatus   (owner)
//	DELETE /api/projects/{pid}/requirements/tapd         → 204                                 (owner)
//	POST   /api/projects/{pid}/requirements/import/tapd  → RequirementImport                   (member)
//
// Saving checks the credentials against TAPD first. A blank password and token on save keep the stored ones.

const (
	tapdSecretKind = "tapd"
	tapdDomain     = "harness-tapd:"
)

// TapdStatus is what the page may know about the connection: never a secret.
type TapdStatus struct {
	Connected    bool   `json:"connected"`
	WorkspaceID  string `json:"workspaceId,omitempty"`
	BaseURL      string `json:"baseUrl,omitempty"`
	User         string `json:"user,omitempty"`
	HasPassword  bool   `json:"hasPassword,omitempty"`
	HasToken     bool   `json:"hasToken,omitempty"`
	OpenStatuses string `json:"openStatuses,omitempty"`
	// Available is false when this server cannot keep the credentials (no secret key), so there is nothing to connect.
	Available bool `json:"available"`
}

func (s *server) routeTapd() error {
	if s.digestStore() == nil || s.opt.Requirements == nil {
		return nil
	}
	if len(s.opt.SecretKey) > 0 {
		b, err := newSecretBoxFor(tapdDomain, s.opt.SecretKey)
		if err != nil {
			return err
		}
		s.tapdBox = b
	}
	s.Route("GET /projects/{pid}/requirements/tapd", s.getTapd)
	s.Route("PUT /projects/{pid}/requirements/tapd", s.putTapd)
	s.Route("DELETE /projects/{pid}/requirements/tapd", s.deleteTapd)
	s.Route("POST /projects/{pid}/requirements/import/tapd", s.importTapd)
	return nil
}

func (s *server) tapdAllowPrivate() bool {
	return s.opt.ProjectData != nil && s.opt.ProjectData.TapdAllowPrivate
}

func (s *server) tapdConfig(ctx context.Context, project string) (tapd.Config, bool, error) {
	var cfg tapd.Config
	if s.tapdBox == nil {
		return cfg, false, nil
	}
	sealed, err := s.digestStore().GetSecret(ctx, project, tapdSecretKind)
	if errors.Is(err, projdata.ErrNotFound) {
		return cfg, false, nil
	}
	if err != nil {
		return cfg, false, err
	}
	plain, err := s.tapdBox.open(sealed)
	if err != nil {
		slog.Warn("tapd: the stored credentials cannot be read; connect again", "project", project)
		return cfg, false, nil
	}
	if json.Unmarshal(plain, &cfg) != nil {
		return cfg, false, nil
	}
	return cfg, true, nil
}

func tapdStatus(cfg tapd.Config, connected, available bool) TapdStatus {
	if !connected {
		return TapdStatus{Available: available}
	}
	return TapdStatus{Connected: true, Available: available, WorkspaceID: cfg.WorkspaceID, BaseURL: cfg.BaseURL, User: cfg.User,
		HasPassword: cfg.Password != "", HasToken: cfg.Token != "", OpenStatuses: cfg.OpenStatuses}
}

func (s *server) getTapd(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	cfg, connected, err := s.tapdConfig(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tapdStatus(cfg, connected, s.tapdBox != nil))
}

// checkTapdBase validates a base URL a user typed: http(s), a host, no credentials in it, and (unless the operator
// allowed private addresses) not a loopback, private or link-local address. A host name that resolves to one is
// refused when the connection is made, by the guarded transport.
func checkTapdBase(raw string, allowPrivate bool) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	if len(raw) > 300 {
		return "", errors.New("The base URL is too long.")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("The base URL must look like https://tapd.example.com")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowPrivate) {
		return "", errors.New("The base URL must use https.")
	}
	if !allowPrivate {
		host := strings.ToLower(u.Hostname())
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
			return "", errors.New("That address is not allowed: it is not a public server.")
		}
		if a, err := netip.ParseAddr(host); err == nil && netguard.Blocked(a.Unmap()) {
			return "", errors.New("That address is not allowed: it is not a public server.")
		}
	}
	return raw, nil
}

func (s *server) tapdClient(cfg tapd.Config) *tapd.Client {
	hc := &http.Client{Transport: netguard.Transport(s.tapdAllowPrivate()), Timeout: 30 * time.Second, CheckRedirect: netguard.SafeCheckRedirect}
	return &tapd.Client{HTTP: hc, Cfg: cfg}
}

func (s *server) putTapd(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	if s.tapdBox == nil {
		writeError(w, http.StatusConflict, "no_secret_key", "This server has no secret key, so it cannot keep TAPD credentials.")
		return
	}
	var b struct {
		WorkspaceID  string `json:"workspaceId"`
		BaseURL      string `json:"baseUrl"`
		User         string `json:"user"`
		Password     string `json:"password"`
		Token        string `json:"token"`
		OpenStatuses string `json:"openStatuses"`
	}
	if !decode(w, r, &b) {
		return
	}
	cfg := tapd.Config{WorkspaceID: strings.TrimSpace(b.WorkspaceID), User: strings.TrimSpace(b.User), Password: b.Password, Token: strings.TrimSpace(b.Token),
		OpenStatuses: strings.Join(tapd.ParseStatuses(b.OpenStatuses), ", ")}
	bad := func(msg string) { writeError(w, http.StatusBadRequest, "invalid_request", msg) }
	base, err := checkTapdBase(b.BaseURL, s.tapdAllowPrivate())
	if err != nil {
		bad(err.Error())
		return
	}
	cfg.BaseURL = base
	prev, had, err := s.tapdConfig(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	// Blank secrets keep the stored ones, as long as the account they belonged to is the same.
	if had && cfg.Password == "" && cfg.Token == "" && prev.User == cfg.User && prev.BaseURL == cfg.BaseURL {
		cfg.Password, cfg.Token = prev.Password, prev.Token
	}
	switch {
	case !tapd.ValidWorkspaceID(cfg.WorkspaceID):
		bad("The workspace id is the number of the TAPD project (digits only).")
	case cfg.Token == "" && (cfg.User == "" || cfg.Password == ""):
		bad("Give the API account and its password, or an access token.")
	case len(cfg.User) > 200 || len(cfg.Password) > 1000 || len(cfg.Token) > 4000 || len(cfg.OpenStatuses) > 500,
		strings.ContainsAny(cfg.User+cfg.Password+cfg.Token+cfg.OpenStatuses, "\r\n\x00"):
		bad("A value is too long or has line breaks.")
	default:
		if cfg.Token != "" {
			cfg.User, cfg.Password = "", "" // one way to sign in
		}
		if err := s.tapdClient(cfg).Check(r.Context()); err != nil {
			s.tapdFailed(w, p.ID, err)
			return
		}
		plain, _ := json.Marshal(cfg)
		if err := s.digestStore().PutSecret(r.Context(), p.ID, tapdSecretKind, s.tapdBox.seal(plain)); err != nil {
			fail(w, err)
			return
		}
		s.note(inProject(c, p.ID), "", "requirement.tapd.connect", map[string]any{"workspace": cfg.WorkspaceID}, FeedEvent{Type: "requirement.changed"})
		writeJSON(w, http.StatusOK, tapdStatus(cfg, true, true))
	}
}

// tapdFailed answers a failed call to TAPD without the address, the credentials or TAPD's own text beyond a short
// message.
func (s *server) tapdFailed(w http.ResponseWriter, project string, err error) {
	slog.Warn("tapd: request failed", "project", project, "error", err)
	switch {
	case errors.Is(err, tapd.ErrAuth):
		writeError(w, http.StatusBadRequest, "tapd_refused", "TAPD refused the credentials. Check the API account, its password or the token, and that it can see this project.")
	case errors.Is(err, tapd.ErrNoStatusInfo):
		writeError(w, http.StatusConflict, "tapd_no_statuses", "TAPD did not say which statuses are final (the workflow endpoint is unavailable to this account). List the open statuses by hand in the TAPD settings, for example: planning, developing.")
	default:
		msg := "TAPD could not be reached or refused the request; check the workspace id and base URL, then try again."
		if strings.HasPrefix(err.Error(), "tapd: status ") {
			msg = "TAPD answered with an error: " + strings.TrimPrefix(err.Error(), "tapd: ")
		}
		writeError(w, http.StatusBadGateway, "tapd_error", msg)
	}
}

func (s *server) deleteTapd(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	if err := s.digestStore().DeleteSecret(r.Context(), p.ID, tapdSecretKind); err != nil {
		fail(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "requirement.tapd.disconnect", nil, FeedEvent{Type: "requirement.changed"})
	w.WriteHeader(http.StatusNoContent)
}

// tapdRequirement maps a story to a requirement (open stories are backlog items).
func tapdRequirement(project, workspace, actor string, st tapd.Story) requirements.Requirement {
	var b strings.Builder
	b.WriteString(st.Description)
	var meta []string
	if st.Owner != "" {
		meta = append(meta, "Owner: "+strings.Trim(strings.TrimSpace(st.Owner), ";"))
	}
	if st.Creator != "" {
		meta = append(meta, "Creator: "+st.Creator)
	}
	if st.Status != "" {
		meta = append(meta, "TAPD status: "+st.Status)
	}
	if st.Priority != "" {
		meta = append(meta, "Priority: "+st.Priority)
	}
	if len(meta) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("---\n" + strings.Join(meta, " | "))
	}
	labels := []string{"tapd"}
	if st.Priority != "" {
		labels = append(labels, "priority:"+st.Priority)
	}
	return requirements.Requirement{
		ProjectID: project, Title: st.Name, Body: b.String(), Status: requirements.StatusBacklog, Priority: tapd.PriorityRank(st.Priority),
		Labels: labels, Author: actor,
		Source: requirements.Origin{Kind: requirements.SourceTAPDStory, Repo: workspace, ID: st.ID, URL: st.URL},
	}
}

func (s *server) importTapd(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	cfg, connected, err := s.tapdConfig(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if !connected {
		writeError(w, http.StatusConflict, "tapd_not_connected", "Connect a TAPD project first.")
		return
	}
	res, err := s.tapdClient(cfg).OpenStories(r.Context(), tapd.MaxImport)
	if err != nil {
		s.tapdFailed(w, p.ID, err)
		return
	}
	out := RequirementImport{Requirements: []requirements.Requirement{}, Source: res.Source, Truncated: res.Truncated}
	for _, st := range res.Stories {
		created, isNew, err := s.opt.Requirements.Create(r.Context(), tapdRequirement(p.ID, cfg.WorkspaceID, c.Actor, st))
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
	s.note(inProject(c, p.ID), "", "requirement.import", map[string]any{"imported": out.Imported, "skipped": out.Skipped, "source": "tapd"}, FeedEvent{Type: "requirement.changed"})
	writeJSON(w, http.StatusOK, out)
}
