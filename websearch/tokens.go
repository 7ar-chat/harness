package websearch

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GhaziBenDahmane/harness/config"
)

// RFC 8693 constants (Studio token_exchange.py).
const (
	tokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessTokenType        = "urn:ietf:params:oauth:token-type:access_token"
	maxTokenLifetime       = 365 * 24 * time.Hour
)

// MaxStoredLifetime caps how long a captured token is kept (opaque tokens
// without a known expiry included).
const MaxStoredLifetime = 12 * time.Hour

// Token is an access token and its expiry (zero: unknown).
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// String never prints the token value.
func (t Token) String() string { return "Token(redacted)" }

// Errors of the token broker (never contain the token).
var (
	// ErrNoToken means the request carried no access token.
	ErrNoToken = errors.New("no access token in the request")
	// ErrTokenExpired means the token (or the run's stored token) is expired.
	ErrTokenExpired = errors.New("the access token is expired")
	// ErrExchange means the RFC 8693 exchange failed (endpoint, HTTP status, body).
	ErrExchange = errors.New("token exchange failed")
	// ErrInsufficientValidity means the exchanged token does not cover the required validity horizon.
	ErrInsufficientValidity = errors.New("the exchanged token does not cover the required validity horizon")
)

// ExchangeConfig is the RFC 8693 client configuration (WORK_TOKEN_EXCHANGE_*).
type ExchangeConfig struct {
	Endpoint     string
	ClientID     string
	ClientSecret string
	Audience     string
	Scope        string
}

// Exchanger performs RFC 8693 token exchanges like Studio's exchange_token_result.
type Exchanger struct {
	Config ExchangeConfig
	// HTTPClient defaults to a client with a 10 s timeout.
	HTTPClient *http.Client
	Now        func() time.Time
}

func (e *Exchanger) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Exchange trades the subject token for a token of the configured audience.
// Neither token is ever logged or put in an error.
func (e *Exchanger) Exchange(ctx context.Context, subject string) (Token, error) {
	if e.Config.Endpoint == "" {
		return Token{}, fmt.Errorf("%w: no token endpoint", ErrExchange)
	}
	form := url.Values{
		"grant_type":           {tokenExchangeGrantType},
		"subject_token":        {subject},
		"subject_token_type":   {accessTokenType},
		"requested_token_type": {accessTokenType},
		"audience":             {e.Config.Audience},
	}
	if e.Config.Scope != "" {
		form.Set("scope", e.Config.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Config.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("%w: invalid token endpoint", ErrExchange)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(e.Config.ClientID, e.Config.ClientSecret) // like httpx auth=(id, secret) in Studio
	client := e.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("%w: request failed", ErrExchange)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("%w: reading the response failed", ErrExchange)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		slog.Warn("work_token_exchange_rejected", "status_code", resp.StatusCode)
		return Token{}, fmt.Errorf("%w with HTTP %d", ErrExchange, resp.StatusCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return Token{}, fmt.Errorf("%w: the response was not valid JSON", ErrExchange)
	}
	access, _ := payload["access_token"].(string)
	access = strings.TrimSpace(access)
	if access == "" {
		return Token{}, fmt.Errorf("%w: the response did not include an access token", ErrExchange)
	}
	return Token{Value: access, ExpiresAt: tokenExpiry(payload, access, e.now())}, nil
}

// tokenExpiry is the earliest of now + expires_in and the JWT exp (zero when neither is usable).
func tokenExpiry(body map[string]any, access string, now time.Time) time.Time {
	var out time.Time
	if in, ok := body["expires_in"].(float64); ok && !math.IsInf(in, 0) && !math.IsNaN(in) && in > 0 && in <= maxTokenLifetime.Seconds() {
		out = now.Add(time.Duration(in) * time.Second)
	}
	if exp, ok := JWTExpiry(access, now); ok && (out.IsZero() || exp.Before(out)) {
		out = exp
	}
	return out
}

// JWTExpiry reads the exp claim of a JWT without verifying it (false when absent or implausible).
func JWTExpiry(token string, now time.Time) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return time.Time{}, false
	}
	exp, ok := claims["exp"].(float64)
	if !ok || math.IsInf(exp, 0) || math.IsNaN(exp) || exp <= 0 || exp > float64(now.Add(maxTokenLifetime).Unix()) {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

// Broker turns the caller's access token into the token given to the web search
// MCP (Studio ensure_studio_managed_environment): legacy mode uses it as is;
// token_exchange mode exchanges it, requires a future expiry covering MinValidity
// and reuses a still-valid exchanged token of the same user and subject token.
type Broker struct {
	Mode        string
	Exchanger   *Exchanger
	MinValidity time.Duration
	Now         func() time.Time

	mu    sync.Mutex
	cache map[string]Token // sha256(login, subject) → exchanged token
}

// NewBroker builds the broker of the configuration.
func NewBroker(cfg *config.Config) *Broker {
	b := &Broker{Mode: cfg.ManagedTokenMode, MinValidity: cfg.TokenMinValidity()}
	if b.Mode == config.TokenModeExchange {
		b.Exchanger = &Exchanger{Config: ExchangeConfig{
			Endpoint: cfg.TokenExchangeEndpoint, ClientID: cfg.TokenExchangeClientID, ClientSecret: cfg.TokenExchangeClientSecret,
			Audience: cfg.TokenExchangeAudience, Scope: cfg.TokenExchangeScope,
		}}
	}
	return b
}

func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

const maxCachedTokens = 1024

// Resolve returns the token to store for a run of login.
func (b *Broker) Resolve(ctx context.Context, login, subject string) (Token, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return Token{}, ErrNoToken
	}
	now := b.now()
	if b.Mode != config.TokenModeExchange {
		tok := Token{Value: subject}
		if exp, ok := JWTExpiry(subject, now); ok {
			if !exp.After(now) {
				return Token{}, ErrTokenExpired
			}
			tok.ExpiresAt = exp
		}
		return capLifetime(tok, now), nil
	}
	if b.Exchanger == nil {
		return Token{}, fmt.Errorf("%w: token exchange is not configured", ErrExchange)
	}
	sum := sha256.Sum256([]byte("work-token-exchange\x00" + login + "\x00" + subject))
	key := string(sum[:])
	horizon := now.Add(b.MinValidity)
	b.mu.Lock()
	cached, ok := b.cache[key]
	b.mu.Unlock()
	if ok && cached.ExpiresAt.After(horizon) {
		return capLifetime(cached, now), nil
	}
	tok, err := b.Exchanger.Exchange(ctx, subject)
	if err != nil {
		return Token{}, err
	}
	if tok.ExpiresAt.IsZero() || !tok.ExpiresAt.After(now) {
		return Token{}, fmt.Errorf("%w: the exchange returned no valid future expiration", ErrExchange)
	}
	if !tok.ExpiresAt.After(horizon) {
		return Token{}, ErrInsufficientValidity
	}
	b.mu.Lock()
	if b.cache == nil {
		b.cache = map[string]Token{}
	}
	if len(b.cache) >= maxCachedTokens {
		for k, v := range b.cache { // drop tokens no longer reusable
			if !v.ExpiresAt.After(horizon) {
				delete(b.cache, k)
			}
		}
		for k := range b.cache { // still full: drop arbitrary entries
			if len(b.cache) < maxCachedTokens/2 {
				break
			}
			delete(b.cache, k)
		}
	}
	b.cache[key] = tok
	b.mu.Unlock()
	return capLifetime(tok, now), nil
}

// capLifetime bounds a token's stored lifetime (unknown expiry: MaxStoredLifetime).
func capLifetime(t Token, now time.Time) Token {
	if limit := now.Add(MaxStoredLifetime); t.ExpiresAt.IsZero() || t.ExpiresAt.After(limit) {
		t.ExpiresAt = limit
	}
	return t
}
