package sandbox

// Credential brokering: a backend whose egress layer can add authentication to the sandbox's outbound HTTPS
// requests implements CredentialBroker, so a secret (a Git host token) reaches the request on its way out and
// never exists inside the sandbox: not in its environment, its files, its command lines or its logs.

import "context"

// Credential is a named secret held outside the sandbox.
type Credential struct {
	Name  string
	Value string
}

// AuthKind says how a binding presents its credential.
type AuthKind string

const (
	// AuthBasic sends "Authorization: Basic <value>"; the value must already be base64("user:password").
	AuthBasic AuthKind = "basic"
	// AuthBearer sends "Authorization: Bearer <value>".
	AuthBearer AuthKind = "bearer"
)

// CredentialBinding attaches a credential to the HTTPS requests that match its host, methods and paths.
// Paths are globs ("/owner/repo.git*"); empty Methods means every method.
type CredentialBinding struct {
	Name       string
	Host       string
	Methods    []string
	Paths      []string
	Auth       AuthKind
	Credential string
}

// CredentialSet is the complete desired state: SetCredentials makes the sandbox's broker equal to it.
type CredentialSet struct {
	Credentials []Credential
	Bindings    []CredentialBinding
}

// CredentialBroker is implemented by sandboxes that can inject credentials outside the workload.
type CredentialBroker interface {
	// SetCredentials replaces the broker's credentials and bindings with set (idempotent). It is called again
	// to renew a short-lived token.
	SetCredentials(ctx context.Context, set CredentialSet) error
}

// BrokerOf returns the credential broker of sb, looking through the wrappers a Manager puts around a sandbox;
// nil when the backend has none.
func BrokerOf(sb Sandbox) CredentialBroker {
	for {
		switch v := sb.(type) {
		case CredentialBroker:
			return v
		case managed:
			if b, ok := v.Instance.(CredentialBroker); ok {
				return b
			}
			return nil
		case noticed:
			sb = v.Sandbox
		case recreated:
			sb = v.Sandbox
		default:
			return nil
		}
	}
}
