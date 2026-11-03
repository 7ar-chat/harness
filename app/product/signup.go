package product

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// Sign-up approval.
//
// An account created through "Continue with GitHub" starts pending: the person gets a session, but every route except
// GET /api/me answers 403 pending_approval until an admin approves them from the admin panel. The admins (Options.Admins)
// are told by email when someone signs up. Accounts the identity provider hands out (OIDC) are approved at once: whoever
// runs the provider already vetted them. A returning user keeps the status an admin gave them, so rejecting someone
// sticks.
//
//	GET  /api/admin/users[?status=pending|approved|rejected] → [{login, email, name, avatarUrl, createdAt, lastSeen, status}]
//	POST /api/admin/users/{login}/approve                    → the user
//	POST /api/admin/users/{login}/reject                     → the user (an admin cannot be rejected)

// isAdmin reports whether the account is one of Options.Admins, by login or by (verified) email.
func (s *server) isAdmin(login, email string) bool {
	login, email = strings.ToLower(login), strings.ToLower(email)
	for _, a := range s.opt.Admins {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && (a == login || (email != "" && a == email)) {
			return true
		}
	}
	return false
}

// admitApproved records a user handed out by the identity provider: approved on creation.
func (s *server) admitApproved(ctx context.Context, u store.User) (*store.User, error) {
	u.Status = store.UserApproved
	return s.admit(ctx, u)
}

// admitPending records a user who signed themselves up: pending on creation, unless they are an admin.
func (s *server) admitPending(ctx context.Context, u store.User) (*store.User, error) {
	u.Status = store.UserPending
	return s.admit(ctx, u)
}

// admit records the user at login with the status a new account starts with (u.Status), makes the admins active and tells
// the admins about a new account that waits.
func (s *server) admit(ctx context.Context, u store.User) (*store.User, error) {
	_, err := s.team.GetUser(ctx, u.Login)
	isNew := errors.Is(err, store.ErrNotFound)
	if err != nil && !isNew {
		return nil, err
	}
	admin := s.isAdmin(u.Login, u.Email)
	if admin {
		u.Status = store.UserApproved
	}
	out, err := s.team.UpsertUser(ctx, u)
	if err != nil {
		return nil, err
	}
	if admin && out.Status != store.UserApproved {
		if err := s.team.SetUserStatus(ctx, out.Login, store.UserApproved); err != nil {
			return nil, err
		}
		out.Status = store.UserApproved
		s.active.forget(out.Login)
	}
	if isNew && out.Status == store.UserPending {
		slog.Info("signup: a new account waits for approval", "login", out.Login)
		go s.notifyAdmins(*out)
	}
	return out, nil
}

// adminRecipients are the addresses to tell: the emails in Options.Admins, and the email of each admin named by login.
func (s *server) adminRecipients(ctx context.Context) []string {
	seen := map[string]bool{}
	var out []string
	add := func(e string) {
		e = strings.TrimSpace(e)
		if e != "" && !seen[strings.ToLower(e)] {
			seen[strings.ToLower(e)] = true
			out = append(out, e)
		}
	}
	for _, a := range s.opt.Admins {
		if strings.Contains(a, "@") {
			add(a)
			continue
		}
		if u, err := s.team.GetUser(ctx, strings.ToLower(strings.TrimSpace(a))); err == nil {
			add(u.Email)
		}
	}
	return out
}

// notifyAdmins emails the admins that u waits for approval. It runs in the background: a mail failure is logged and never
// blocks a login.
func (s *server) notifyAdmins(u store.User) {
	if s.opt.Mail == nil {
		slog.Warn("signup: no SMTP_HOST configured, the admins are not emailed", "login", u.Login)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	to := s.adminRecipients(ctx)
	if len(to) == 0 {
		slog.Warn("signup: no admin email known (HARNESS_ADMINS), nobody is emailed", "login", u.Login)
		return
	}
	link := s.configuredBase()
	if link != "" {
		link += "/#/admin"
	}
	body := fmt.Sprintf("%s signed up and waits for your approval.\n\nName:  %s\nLogin: %s\nEmail: %s\n\n", u.Label(), u.Label(), u.Login, orDash(u.Email))
	if link != "" {
		body += "Review the request: " + link + "\n"
	} else {
		body += "Review the request in the admin panel.\n"
	}
	if err := s.opt.Mail.Send(ctx, to, "New sign-up waiting for approval: "+u.Label(), body); err != nil {
		slog.Error("signup: emailing the admins failed", "error", err)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- the gate ----

// activeCache remembers for a few seconds whether an account may use the product, so the gate does not query the store on
// every request. An approval or a rejection made on this replica takes effect at once; on another, after the TTL.
type activeCache struct {
	mu sync.Mutex
	m  map[string]activeEntry
}

type activeEntry struct {
	status string
	at     time.Time
}

const activeTTL = 5 * time.Second

func (c *activeCache) forget(login string) {
	c.mu.Lock()
	delete(c.m, login)
	c.mu.Unlock()
}

// status returns the sign-up state of login ("" for approved or unknown).
func (s *server) status(ctx context.Context, login string) (string, error) {
	c := &s.active
	c.mu.Lock()
	e, ok := c.m[login]
	c.mu.Unlock()
	if ok && time.Since(e.at) < activeTTL {
		return e.status, nil
	}
	u, err := s.team.GetUser(ctx, login)
	st := ""
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return "", err
	default:
		st = u.Status
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]activeEntry{}
	}
	c.m[login] = activeEntry{st, time.Now()}
	c.mu.Unlock()
	return st, nil
}

// gate refuses the accounts that are not approved, except on the route that tells them where they stand.
func (s *server) gate(r *http.Request, actor string) error {
	if p := r.URL.Path; p == "/api/me" || p == "/me" {
		return nil
	}
	st, err := s.status(r.Context(), actor)
	if err != nil {
		return err
	}
	switch st {
	case store.UserPending:
		return &accessError{http.StatusForbidden, "pending_approval", "Your account is waiting for an administrator's approval."}
	case store.UserRejected:
		return &accessError{http.StatusForbidden, "account_rejected", "Your account was not approved."}
	}
	return nil
}

// ---- the admin panel ----

type adminUserView struct {
	Login     string    `json:"login"`
	Email     string    `json:"email,omitempty"`
	Name      string    `json:"name"`
	AvatarURL string    `json:"avatarUrl,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	Status    string    `json:"status"`
}

func adminViewOf(u store.User) adminUserView {
	st := u.Status
	if st == "" {
		st = store.UserApproved
	}
	return adminUserView{Login: u.Login, Email: u.Email, Name: u.Label(), AvatarURL: u.Avatar(), CreatedAt: u.CreatedAt, LastSeen: u.LastSeen, Status: st}
}

func (s *server) routeAdmin() {
	s.Route("GET /admin/users", s.adminUsers)
	s.Route("POST /admin/users/{login}/approve", s.adminSetStatus(store.UserApproved))
	s.Route("POST /admin/users/{login}/reject", s.adminSetStatus(store.UserRejected))
}

// requireAdmin answers 403 unless the caller is an admin.
func (s *server) requireAdmin(w http.ResponseWriter, r *http.Request, c Caller) bool {
	u, err := s.currentUser(r, c)
	if err != nil {
		fail(w, err)
		return false
	}
	if !s.isAdmin(u.Login, u.Email) {
		writeError(w, http.StatusForbidden, "forbidden", "Only administrators can do this.")
		return false
	}
	return true
}

func (s *server) adminUsers(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.requireAdmin(w, r, c) {
		return
	}
	users, err := s.team.ListUsers(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	want := r.URL.Query().Get("status")
	out := []adminUserView{}
	for _, u := range users {
		if v := adminViewOf(u); want == "" || v.Status == want {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, http.StatusOK, out)
}

func (s *server) adminSetStatus(status string) handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) {
		if !s.requireAdmin(w, r, c) {
			return
		}
		login := strings.ToLower(r.PathValue("login"))
		u, err := s.team.GetUser(r.Context(), login)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such user.")
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		if status == store.UserRejected && s.isAdmin(u.Login, u.Email) {
			writeError(w, http.StatusBadRequest, "invalid_request", "An administrator cannot be rejected.")
			return
		}
		if err := s.team.SetUserStatus(r.Context(), login, status); err != nil {
			fail(w, err)
			return
		}
		s.active.forget(login)
		slog.Info("signup: account "+status, "login", login, "by", c.Actor)
		u.Status = status
		writeJSON(w, http.StatusOK, adminViewOf(*u))
	}
}
