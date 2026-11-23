package githost

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Access is how a repository is reached: its URL and the environment (credentials included) of the commands that
// clone, fetch and push. Secrets are the literals to mask in what the commands print.
type Access struct {
	URL     string
	Env     map[string]string
	Secrets []string
}

// HostAccess resolves how login reaches a repository of a registered host ("host/path"): over SSH when the user has
// a key and the administrator registered the host's SSH host keys, else over HTTPS with the user's token (or the
// host's shared token), else anonymously over HTTPS. A host that is not registered is refused: this is the egress
// guard of the checkout. The environment carries GH_TOKEN and GIT_USER, or GIT_SSH_KEY and GIT_KNOWN_HOSTS, for
// the gitx function of the clone and push scripts.
func HostAccess(ctx context.Context, res Resolver, repo, login string) (Access, error) {
	host, path, ok := SplitRepo(repo)
	if !ok || host == "" {
		return Access{}, fmt.Errorf("invalid repository %q", repo)
	}
	if res == nil {
		return Access{}, errors.New("this server has no code hosts besides GitHub")
	}
	h, err := res.Host(ctx, host)
	if err != nil {
		return Access{}, fmt.Errorf("%s is not a registered git host (an administrator adds it under Admin, Git hosts)", host)
	}
	cred, err := res.Credential(ctx, login, host)
	if err != nil {
		cred = Credential{}
	}
	a := Access{Env: map[string]string{}}
	switch {
	case len(cred.SSHKey) > 0 && len(h.KnownHosts) == 0 && cred.Token == "":
		return Access{}, fmt.Errorf("%s has no registered SSH host key: an administrator adds it under Admin, Git hosts", host)
	case len(cred.SSHKey) > 0 && len(h.KnownHosts) > 0:
		a.URL = h.SSHURL(path)
		a.Env["GIT_SSH_KEY"] = string(cred.SSHKey)
		a.Env["GIT_KNOWN_HOSTS"] = strings.Join(h.KnownHosts, "\n")
		a.Secrets = []string{string(cred.SSHKey)}
	case cred.Token != "":
		a.URL = h.CloneURL(path)
		a.Env["GH_TOKEN"] = cred.Token
		a.Env["GIT_USER"] = firstNonEmpty(cred.Username, DefaultGitUser(h.Kind))
		a.Secrets = []string{cred.Token}
	default:
		// No credential: a public repository can still be cloned over HTTPS; a push will say what is missing.
		a.URL = h.CloneURL(path)
	}
	return a, nil
}

// DefaultGitUser is the HTTPS user name that goes with a token when the user gave none.
func DefaultGitUser(k Kind) string {
	if k == KindGitLab {
		return "oauth2"
	}
	return "x-access-token"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
