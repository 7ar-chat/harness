package websearch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/GhaziBenDahmane/harness/store"
)

// Vault stores the token of each run encrypted (store.CredentialStore + Sealer).
type Vault struct {
	Store  store.CredentialStore
	Sealer *Sealer
	Now    func() time.Time
}

func (v *Vault) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// Put stores tok for the run of login (only while the run is active). An
// existing token is replaced when replace is true or tok expires later.
func (v *Vault) Put(ctx context.Context, runID, login string, tok Token, replace bool) (bool, error) {
	if tok.Value == "" {
		return false, ErrNoToken
	}
	if !tok.ExpiresAt.After(v.now()) {
		return false, ErrTokenExpired
	}
	sealed, err := v.Sealer.Seal([]byte(tok.Value), runID, login)
	if err != nil {
		return false, err
	}
	return v.Store.PutRunCredential(ctx, store.RunCredential{RunID: runID, UserLogin: login, Sealed: sealed, ExpiresAt: tok.ExpiresAt}, replace)
}

// Token returns the run's token: ErrNoToken when it has none (or it belongs to
// another user / cannot be decrypted), ErrTokenExpired once it expired.
func (v *Vault) Token(ctx context.Context, runID, login string) (Token, error) {
	c, err := v.Store.GetRunCredential(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return Token{}, ErrNoToken
	}
	if err != nil {
		return Token{}, err
	}
	if c.UserLogin != login {
		return Token{}, ErrNoToken
	}
	if !c.ExpiresAt.After(v.now()) {
		return Token{}, ErrTokenExpired
	}
	plain, err := v.Sealer.Open(c.Sealed, runID, login)
	if err != nil {
		return Token{}, ErrNoToken
	}
	return Token{Value: string(plain), ExpiresAt: c.ExpiresAt}, nil
}

// Delete removes the run's token.
func (v *Vault) Delete(ctx context.Context, runID string) error {
	return v.Store.DeleteRunCredential(ctx, runID)
}

// Handoff gives the token of run from to run to (same user), unless to already
// has a token expiring later. Used for the goal run started by a run's end.
func (v *Vault) Handoff(ctx context.Context, from, to, login string) (bool, error) {
	tok, err := v.Token(ctx, from, login)
	if err != nil {
		return false, err
	}
	return v.Put(ctx, to, login, tok, false)
}

// Capturer is the api side: it resolves the caller's token and stores it for a run.
type Capturer struct {
	Broker *Broker
	Vault  *Vault
}

// CaptureRun stores the token for a new run. Failures only disable web search
// for that run (logged without the token).
func (c *Capturer) CaptureRun(ctx context.Context, runID, login, accessToken string) {
	c.capture(ctx, runID, login, accessToken, true)
}

// RefreshRun stores a fresher token for an active run (a steer), when it expires
// later than the stored one.
func (c *Capturer) RefreshRun(ctx context.Context, runID, login, accessToken string) {
	c.capture(ctx, runID, login, accessToken, false)
}

func (c *Capturer) capture(ctx context.Context, runID, login, accessToken string, replace bool) {
	if accessToken == "" {
		if replace {
			slog.Info("web search: no access token in the request, web search is not offered to the run", "run_id", runID)
		}
		return
	}
	tok, err := c.Broker.Resolve(ctx, login, accessToken)
	if err != nil {
		slog.Warn("web search: no token for the run, web search is not offered", "run_id", runID, "reason", err.Error())
		return
	}
	if _, err := c.Vault.Put(ctx, runID, login, tok, replace); err != nil {
		slog.Warn("web search: storing the run token failed", "run_id", runID, "reason", err.Error())
	}
}
