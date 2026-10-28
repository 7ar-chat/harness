package websearch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/websearch"
)

var now0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return now0 }

func TestLegacyModeUsesTheUserTokenWithItsExpiry(t *testing.T) {
	// legacy: the user's own token is stored as is, with its JWT exp (opaque tokens: capped lifetime); expired or missing tokens are refused.
	// Arrange
	b := &websearch.Broker{Mode: config.TokenModeLegacy, Now: clock}
	exp := now0.Add(10 * time.Minute)
	valid := jwt("alice", exp)

	// Act
	tok, err := b.Resolve(context.Background(), "alice", valid)
	opaque, opaqueErr := b.Resolve(context.Background(), "alice", "opaque-token")
	_, expiredErr := b.Resolve(context.Background(), "alice", jwt("alice", now0.Add(-time.Second)))
	_, noneErr := b.Resolve(context.Background(), "alice", "  ")

	// Assert
	if err != nil || tok.Value != valid || !tok.ExpiresAt.Equal(time.Unix(exp.Unix(), 0)) {
		t.Fatalf("tok %+v err %v", tok.ExpiresAt, err)
	}
	if opaqueErr != nil || opaque.Value != "opaque-token" || !opaque.ExpiresAt.Equal(now0.Add(websearch.MaxStoredLifetime)) {
		t.Fatalf("opaque %v %v", opaque.ExpiresAt, opaqueErr)
	}
	if !errors.Is(expiredErr, websearch.ErrTokenExpired) || !errors.Is(noneErr, websearch.ErrNoToken) {
		t.Fatalf("expired %v none %v", expiredErr, noneErr)
	}
	if strings.Contains(tok.String(), valid) {
		t.Fatal("Token.String leaks the token")
	}
}

type exchangeServer struct {
	mu    sync.Mutex
	calls int
	forms []map[string]string
	users []string
	// respond builds the response of a call for a subject token.
	respond func(subject string) (int, string)
}

func (s *exchangeServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, secret, _ := r.BasicAuth()
		s.mu.Lock()
		s.calls++
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		s.forms = append(s.forms, form)
		s.users = append(s.users, id+":"+secret)
		s.mu.Unlock()
		code, body := s.respond(r.PostForm.Get("subject_token"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func exchangeBroker(url string, minValidity time.Duration) *websearch.Broker {
	return &websearch.Broker{Mode: config.TokenModeExchange, MinValidity: minValidity, Now: clock, Exchanger: &websearch.Exchanger{
		Config: websearch.ExchangeConfig{Endpoint: url, ClientID: "work", ClientSecret: "s3cret", Audience: "websearch-mcp", Scope: "openid websearch"},
		Now:    clock,
	}}
}

func TestTokenExchangeSendsRFC8693RequestAndKeepsTheEarliestExpiry(t *testing.T) {
	// token_exchange: RFC 8693 form with client basic auth, audience and scope; expiry = min(expires_in, JWT exp).
	// Arrange
	exchanged := jwt("alice", now0.Add(30*time.Minute))
	srv := &exchangeServer{respond: func(string) (int, string) {
		return 200, `{"access_token":"` + exchanged + `","expires_in":3600,"token_type":"Bearer"}`
	}}
	url := srv.start(t).URL
	b := exchangeBroker(url, 15*time.Minute)

	// Act
	tok, err := b.Resolve(context.Background(), "alice", "subject-token")

	// Assert
	if err != nil || tok.Value != exchanged || !tok.ExpiresAt.Equal(now0.Add(30*time.Minute)) {
		t.Fatalf("tok exp %v err %v", tok.ExpiresAt, err)
	}
	f := srv.forms[0]
	if f["grant_type"] != "urn:ietf:params:oauth:grant-type:token-exchange" || f["subject_token"] != "subject-token" ||
		f["subject_token_type"] != "urn:ietf:params:oauth:token-type:access_token" ||
		f["requested_token_type"] != "urn:ietf:params:oauth:token-type:access_token" ||
		f["audience"] != "websearch-mcp" || f["scope"] != "openid websearch" || srv.users[0] != "work:s3cret" {
		t.Fatalf("form %v auth %v", f, srv.users)
	}
}

func TestTokenExchangeEnforcesTheMinimumValidity(t *testing.T) {
	// Like Studio: an exchanged token must have a future expiry beyond now + min validity.
	cases := []struct {
		name string
		body string
		want error
	}{
		{"too short", `{"access_token":"opaque","expires_in":600}`, websearch.ErrInsufficientValidity},
		{"no expiry", `{"access_token":"opaque"}`, websearch.ErrExchange},
		{"no access token", `{"expires_in":3600}`, websearch.ErrExchange},
		{"not json", `<html>`, websearch.ErrExchange},
		{"long enough", `{"access_token":"opaque","expires_in":1800}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			srv := &exchangeServer{respond: func(string) (int, string) { return 200, tc.body }}
			b := exchangeBroker(srv.start(t).URL, 15*time.Minute)

			// Act
			_, err := b.Resolve(context.Background(), "alice", "subject")

			// Assert
			if (tc.want == nil) != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err %v want %v", err, tc.want)
			}
		})
	}
}

func TestTokenExchangeRejectionNeverLeaksTokens(t *testing.T) {
	// A rejected exchange reports the HTTP status only (no subject token, no client secret).
	// Arrange
	srv := &exchangeServer{respond: func(string) (int, string) { return 400, `{"error":"invalid_grant"}` }}
	b := exchangeBroker(srv.start(t).URL, time.Minute)

	// Act
	_, err := b.Resolve(context.Background(), "alice", "subject-secret")

	// Assert
	if !errors.Is(err, websearch.ErrExchange) || !strings.Contains(err.Error(), "400") ||
		strings.Contains(err.Error(), "subject-secret") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err %v", err)
	}
}

func TestTokenExchangeReusesTokensPerUserAndSubjectOnly(t *testing.T) {
	// A still-valid exchanged token is reused for the same user and subject token, never for another user.
	// Arrange
	srv := &exchangeServer{respond: func(subject string) (int, string) {
		return 200, `{"access_token":"exchanged-for-` + subject + `","expires_in":3600}`
	}}
	b := exchangeBroker(srv.start(t).URL, 15*time.Minute)
	ctx := context.Background()

	// Act
	a1, _ := b.Resolve(ctx, "alice", "alice-subject")
	a2, _ := b.Resolve(ctx, "alice", "alice-subject")
	bob, _ := b.Resolve(ctx, "bob", "alice-subject")
	a3, _ := b.Resolve(ctx, "alice", "alice-new-subject")

	// Assert
	if srv.calls != 3 || a1.Value != a2.Value || bob.Value != "exchanged-for-alice-subject" || a3.Value != "exchanged-for-alice-new-subject" {
		t.Fatalf("calls %d values %q %q %q %q", srv.calls, a1.Value, a2.Value, bob.Value, a3.Value)
	}
}

func TestNewBrokerFollowsTheConfiguration(t *testing.T) {
	// The min validity defaults to max(900 s, LLM_TIMEOUT_SECONDS) like Studio's max(900, stream timeout).
	// Arrange
	cfg := &config.Config{LLMTimeout: 300}
	cfg.ManagedTokenMode = config.TokenModeExchange
	cfg.TokenExchangeEndpoint = "http://idp/token"
	long := &config.Config{LLMTimeout: 1200}
	set := &config.Config{LLMTimeout: 300}
	set.ManagedTokenMinValiditySeconds = 60

	// Act
	b := websearch.NewBroker(cfg)

	// Assert
	if b.Exchanger == nil || b.Exchanger.Config.Endpoint != "http://idp/token" || b.MinValidity != 900*time.Second ||
		long.TokenMinValidity() != 1200*time.Second || set.TokenMinValidity() != time.Minute {
		t.Fatalf("broker %+v", b)
	}
}
