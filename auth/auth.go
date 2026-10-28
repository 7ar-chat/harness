// Package auth decodes the gateway identity header (X-Userinfo) and carries
// it through request and run contexts.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// HeaderName is the header set by the gateway.
const HeaderName = "X-Userinfo"

// User is the authenticated caller.
type User struct {
	Login     string `json:"login"`
	Email     string `json:"email,omitempty"`
	FirstName string `json:"firstname,omitempty"`
	LastName  string `json:"lastname,omitempty"`
	// Raw is the original header value, forwarded to the LLM gateway and MCP servers.
	Raw string `json:"-"`
}

// ErrUnauthenticated is returned when the header is missing or invalid.
var ErrUnauthenticated = errors.New("authentication credentials were not provided")

// Decode parses an X-Userinfo value: base64 (std, fallback URL-safe) JSON with a
// required login taken from "preferred_username", falling back to "login".
func Decode(header string) (*User, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, ErrUnauthenticated
	}
	decoded, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(header)
		if err != nil {
			decoded, err = base64.RawURLEncoding.DecodeString(header)
			if err != nil {
				return nil, ErrUnauthenticated
			}
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(decoded, &raw); err != nil {
		return nil, ErrUnauthenticated
	}
	// Same claim precedence as studio_django_sdk: OIDC standard claim first,
	// then the legacy Studio key; blank values count as absent.
	u := &User{
		Login:     claim(raw, "preferred_username", "login"),
		Email:     claim(raw, "email", "mail"),
		FirstName: claim(raw, "given_name", "firstname"),
		LastName:  claim(raw, "family_name", "lastname"),
		Raw:       header,
	}
	if u.Login == "" {
		return nil, ErrUnauthenticated
	}
	return u, nil
}

// claim returns raw[primary], or raw[fallback] when the primary is missing or blank.
func claim(raw map[string]any, primary, fallback string) string {
	if v := str(raw[primary]); v != "" {
		return v
	}
	return str(raw[fallback])
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

type ctxKey struct{}

// WithUserinfo stores the raw X-Userinfo value in ctx (used by outbound HTTP clients).
func WithUserinfo(ctx context.Context, raw string) context.Context {
	return context.WithValue(ctx, ctxKey{}, raw)
}

// UserinfoFromContext returns the raw X-Userinfo value stored by WithUserinfo.
func UserinfoFromContext(ctx context.Context) string {
	s, _ := ctx.Value(ctxKey{}).(string)
	return s
}
