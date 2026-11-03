package product

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/7ar-chat/harness/core/store"
)

// GitHubLoginConfig turns on "Continue with GitHub" on the sign-in page: the authorization code flow with PKCE of a GitHub
// OAuth App (not the GitHub App of the connector). The OAuth App's callback URL is <public URL>/auth/signin/github/callback.
//
// A person who signs in this way is the account "github_<username>"; the prefix keeps a GitHub username from ever being
// taken for a login the identity provider hands out. When the verified primary email of the GitHub account is the email of
// an existing account, that account is signed in instead, so someone who already uses the product keeps their projects.
// Everyone else starts pending (see signup.go).
type GitHubLoginConfig struct {
	ClientID     string
	ClientSecret string
	// RedirectURL is the callback; default <PublicURL>/auth/signin/github/callback.
	RedirectURL string
	// SessionKey and SessionTTL sign the session cookie when there is no OIDC login to share it with.
	SessionKey []byte
	SessionTTL time.Duration

	// The endpoints, for tests; the defaults are GitHub's.
	authURL, tokenURL, apiURL string
}

const githubLoginPrefix = "github_"

type githubLogin struct {
	cfg   GitHubLoginConfig
	oauth oauth2.Config
	api   string
	team  store.TeamStore
	sessions
	admit func(ctx context.Context, u store.User) (*store.User, error)
	http  *http.Client
}

func newGitHubLogin(cfg GitHubLoginConfig, ss sessions, team store.TeamStore, admit func(context.Context, store.User) (*store.User, error)) *githubLogin {
	auth, tok, api := cfg.authURL, cfg.tokenURL, cfg.apiURL
	if auth == "" {
		auth = "https://github.com/login/oauth/authorize"
	}
	if tok == "" {
		tok = "https://github.com/login/oauth/access_token"
	}
	if api == "" {
		api = "https://api.github.com"
	}
	return &githubLogin{
		cfg: cfg, api: strings.TrimRight(api, "/"), team: team, sessions: ss, admit: admit,
		oauth: oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: oauth2.Endpoint{AuthURL: auth, TokenURL: tok}, Scopes: []string{"read:user", "user:email"}},
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *githubLogin) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/signin/github", g.login)
	mux.HandleFunc("GET /auth/signin/github/callback", g.callback)
}

func (g *githubLogin) login(w http.ResponseWriter, r *http.Request) {
	f := flowState{State: randomString(18), Verifier: oauth2.GenerateVerifier(),
		Next: safeNext(r.URL.Query().Get("next")), Exp: time.Now().Add(10 * time.Minute).Unix()}
	g.setCookie(w, githubFlowCookie, g.seal(f), 600)
	http.Redirect(w, r, g.oauth.AuthCodeURL(f.State, oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

const githubFlowCookie = "harness_github_flow"

func (g *githubLogin) callback(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, msg string) {
		g.setCookie(w, githubFlowCookie, "", -1)
		writeError(w, status, "login_failed", msg)
	}
	c, err := r.Cookie(githubFlowCookie)
	var f flowState
	if err != nil || g.open(c.Value, &f) != nil || time.Now().Unix() > f.Exp {
		fail(http.StatusBadRequest, "The login attempt expired; try again.")
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		fail(http.StatusUnauthorized, "GitHub refused the login.")
		return
	}
	if !hmac.Equal([]byte(q.Get("state")), []byte(f.State)) {
		fail(http.StatusBadRequest, "Invalid state.")
		return
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, g.http)
	tok, err := g.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		slog.Warn("httpserver: GitHub code exchange failed", "error", err)
		fail(http.StatusUnauthorized, "The login could not be completed.")
		return
	}
	u, err := g.userFor(ctx, tok.AccessToken)
	if err != nil {
		slog.Warn("httpserver: reading the GitHub account failed", "error", err)
		fail(http.StatusUnauthorized, "The GitHub account could not be read.")
		return
	}
	out, err := g.admit(ctx, u)
	if err != nil {
		slog.Error("httpserver: recording the user failed", "error", err)
		fail(http.StatusInternalServerError, "The login could not be completed.")
		return
	}
	g.setCookie(w, githubFlowCookie, "", -1)
	g.start(w, out.Login)
	http.Redirect(w, r, f.Next, http.StatusFound)
}

// userFor reads the GitHub account of the token and returns the user to record: a new "github_<username>" account, or the
// existing account that owns the verified email.
func (g *githubLogin) userFor(ctx context.Context, token string) (store.User, error) {
	var me struct {
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := g.get(ctx, token, "/user", &me); err != nil {
		return store.User{}, err
	}
	if me.Login == "" {
		return store.User{}, errors.New("no username")
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	email := ""
	// The email is optional (the scope can be refused): without it the account is just a new, pending one.
	if err := g.get(ctx, token, "/user/emails", &emails); err == nil {
		for _, e := range emails {
			if e.Verified && (e.Primary || email == "") {
				email = strings.ToLower(e.Email)
			}
		}
	}
	u := store.User{Login: githubLoginPrefix + strings.ToLower(me.Login), Email: email, Name: strings.TrimSpace(me.Name), AvatarURL: me.AvatarURL}
	if u.Name == "" {
		u.Name = me.Login
	}
	if email != "" {
		if known, err := g.team.GetUserByEmail(ctx, email); err == nil {
			// Same verified address: same person. Keep the name they use here, and take GitHub's picture when they have none.
			u.Login = known.Login
			if known.Name != "" {
				u.Name = known.Name
			}
			if known.AvatarURL != "" {
				u.AvatarURL = known.AvatarURL
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return store.User{}, err
		}
	}
	return u, nil
}

func (g *githubLogin) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github %s: %s", path, resp.Status)
	}
	return json.Unmarshal(b, out)
}

// redirect is the callback URL ("" for a nil config).
func (c *GitHubLoginConfig) redirect() string {
	if c == nil {
		return ""
	}
	return c.RedirectURL
}
