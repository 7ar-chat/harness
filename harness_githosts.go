package harness

import (
	"net/http"

	"github.com/7ar-chat/harness/core/githost"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/features/factory/forge"
)

// HostIssueSource returns the issue provider of a registered code host (GitLab, Gitea) for the requirements import;
// a plain git host has none (nil, false). res supplies the token of the user in the context (githost.WithLogin).
func HostIssueSource(h githost.Host, res githost.Resolver, hc *http.Client) (requirements.IssueSource, bool) {
	return forge.IssueSourceFor(h, res, hc)
}

// RedactSecrets masks the secrets in text a person or a model will read: every literal in secrets and anything shaped
// like a credential (tokens, keys, authorization headers).
func RedactSecrets(s string, secrets ...string) string { return forge.Redact(s, secrets...) }
