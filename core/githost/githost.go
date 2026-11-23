// Package githost describes the code hosts a deployment may work with besides GitHub.com: GitLab, Gitea and
// Forgejo, and plain git over HTTPS or SSH. It holds what every layer needs to agree on (how a repository is
// named, what a registered host is, how a credential is looked up) and nothing that talks to a network.
//
// A repository of GitHub.com keeps the id "owner/name". A repository of any other host is "host/path" (for
// example "gitlab.example.com/group/sub/project"): the first segment holds a dot or a colon, which a GitHub owner
// cannot, so the two never collide.
package githost

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Kind is the sort of server behind a host.
type Kind string

// Host kinds. Forgejo speaks the Gitea API.
const (
	KindGitHub  Kind = "github"
	KindGitLab  Kind = "gitlab"
	KindGitea   Kind = "gitea"
	KindGeneric Kind = "git"
)

// Valid reports whether k is a kind a host can be registered with.
func (k Kind) Valid() bool {
	switch k {
	case KindGitLab, KindGitea, KindGeneric:
		return true
	}
	return false
}

// HasAPI reports whether the kind has a pull request API the forge can drive.
func (k Kind) HasAPI() bool { return k == KindGitLab || k == KindGitea }

// GitHubHost is the host name of GitHub.com, whose repositories keep the short "owner/name" id.
const GitHubHost = "github.com"

// Errors of the resolver.
var (
	ErrUnknownHost  = errors.New("githost: this git host is not registered by an administrator")
	ErrNoCredential = errors.New("githost: no access token or SSH key for this git host")
)

// Host is a code host an administrator registered. Name is host or host:port, lower case, and is the key.
type Host struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// BaseURL is the web root ("https://gitlab.example.com", with a path prefix when the server is mounted under
	// one). It is also where the API lives.
	BaseURL string `json:"baseUrl"`
	// SSHHost and SSHPort tell where git over SSH connects (default: the host name, port 22).
	SSHHost string `json:"sshHost,omitempty"`
	SSHPort int    `json:"sshPort,omitempty"`
	// KnownHosts are the SSH host keys, in known_hosts format, that a connection must present. Without any, SSH
	// to this host is refused: a first connection is never trusted blindly.
	KnownHosts []string `json:"knownHosts,omitempty"`
	Note       string   `json:"note,omitempty"`
}

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// ValidName reports whether s is a host name (with an optional port) a host may be registered under.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > 253 || !hostRe.MatchString(s) {
		return false
	}
	h, _, _ := strings.Cut(s, ":")
	return !strings.Contains(h, "..")
}

// Normalize lower-cases a host name and drops the default HTTPS port.
func Normalize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.TrimSuffix(name, ":443")
}

// Validate checks a host before it is stored and fills the defaults.
func (h *Host) Validate() error {
	h.Name = Normalize(h.Name)
	if !ValidName(h.Name) || h.Name == GitHubHost {
		return errors.New("the host name must be a DNS name with an optional port (GitHub.com is served by the GitHub App)")
	}
	if !strings.ContainsAny(h.Name, ".:") {
		return errors.New("the host name needs a dot or a port, so it can never be read as a GitHub owner")
	}
	if !h.Kind.Valid() {
		return errors.New("the kind must be gitlab, gitea or git")
	}
	if h.BaseURL == "" {
		h.BaseURL = "https://" + h.Name
	}
	u, err := url.Parse(h.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the base URL must be http(s)://host with no credentials")
	}
	if Normalize(u.Host) != h.Name {
		return errors.New("the base URL must be on the host it is registered for")
	}
	h.BaseURL = strings.TrimRight(u.String(), "/")
	if h.SSHPort < 0 || h.SSHPort > 65535 {
		return errors.New("the SSH port is not valid")
	}
	h.SSHHost = strings.ToLower(strings.TrimSpace(h.SSHHost))
	if h.SSHHost != "" && !ValidName(h.SSHHost) {
		return errors.New("the SSH host is not valid")
	}
	var kh []string
	for _, l := range h.KnownHosts {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if len(strings.Fields(l)) < 3 || strings.ContainsAny(l, "\x00\r") {
			return errors.New("a host key line must be 'host key-type key' (the output of ssh-keyscan)")
		}
		kh = append(kh, l)
	}
	h.KnownHosts = kh
	return nil
}

// APIBase is the root of the REST API ("" for a host with none).
func (h Host) APIBase() string {
	switch h.Kind {
	case KindGitLab:
		return h.BaseURL + "/api/v4"
	case KindGitea:
		return h.BaseURL + "/api/v1"
	}
	return ""
}

// CloneURL is the HTTPS URL of path ("group/project") on this host.
func (h Host) CloneURL(path string) string { return h.BaseURL + "/" + path + ".git" }

// SSHEndpoint is the host and port git over SSH connects to.
func (h Host) SSHEndpoint() (host string, port int) {
	host, port = h.SSHHost, h.SSHPort
	if host == "" {
		host, _, _ = strings.Cut(h.Name, ":")
	}
	if port == 0 {
		port = 22
	}
	return host, port
}

// SSHURL is the ssh:// URL of path on this host.
func (h Host) SSHURL(path string) string {
	host, port := h.SSHEndpoint()
	return "ssh://git@" + net.JoinHostPort(host, strconv.Itoa(port)) + "/" + path + ".git"
}

// Ref names a repository: Host "" is GitHub.com.
type Ref struct {
	Host string
	Path string
	// SSH tells the URL was an SSH one (git@host:path, ssh://...).
	SSH bool
}

var segRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseRepoURL reads a repository URL of any host: https://h/a/b[.git], ssh://git@h[:port]/a/b, git@h:a/b.git.
// Credentials in a URL are refused. The path keeps every segment (GitLab groups nest).
func ParseRepoURL(raw string) (Ref, bool) {
	raw = strings.TrimSpace(raw)
	var host, path string
	ssh := false
	switch {
	case raw == "":
		return Ref{}, false
	case !strings.Contains(raw, "://"):
		// scp-like: [user@]host:path
		rest := raw[strings.LastIndex(raw, "@")+1:]
		h, p, found := strings.Cut(rest, ":")
		if !found || h == "" || strings.Contains(h, "/") {
			return Ref{}, false
		}
		host, path, ssh = h, p, true
	default:
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return Ref{}, false
		}
		switch u.Scheme {
		case "https", "http":
			if u.User != nil {
				return Ref{}, false
			}
			host = u.Host
		case "ssh", "git+ssh":
			host, ssh = u.Host, true
			if p := u.Port(); p != "" && p != "22" {
				host = u.Hostname() + ":" + p
			} else {
				host = u.Hostname()
			}
		default:
			return Ref{}, false
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	host = Normalize(host)
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if !ValidName(host) || path == "" {
		return Ref{}, false
	}
	segs := strings.Split(path, "/")
	for _, s := range segs {
		if s == "." || s == ".." || !segRe.MatchString(s) {
			return Ref{}, false
		}
	}
	if host == GitHubHost {
		if len(segs) != 2 {
			return Ref{}, false
		}
		host = ""
	}
	return Ref{Host: host, Path: path, SSH: ssh}, true
}

// Repo is the repository id.
func (r Ref) Repo() string {
	if r.Host == "" {
		return r.Path
	}
	return r.Host + "/" + r.Path
}

// SplitRepo splits a repository id. host is "" for a GitHub.com id ("owner/name"). ok is false for an id that is
// neither shape.
func SplitRepo(repo string) (host, path string, ok bool) {
	first, rest, found := strings.Cut(repo, "/")
	if !found || first == "" || rest == "" {
		return "", "", false
	}
	if strings.ContainsAny(first, ".:") {
		first = strings.ToLower(first)
		if !ValidName(first) {
			return "", "", false
		}
		return first, rest, true
	}
	return "", repo, true
}

// IsQualified reports whether the repository id names a host other than GitHub.com.
func IsQualified(repo string) bool {
	h, _, ok := SplitRepo(repo)
	return ok && h != ""
}

// Credential is what a user presents to one host. Token is a personal access token for HTTPS and the API; SSHKey is
// a PEM private key for git over SSH. Never log or return it.
type Credential struct {
	// Username goes with the token for git over HTTPS ("oauth2" for GitLab, the account for Gitea).
	Username string
	Token    string
	SSHKey   []byte
}

// Empty reports whether there is nothing to present.
func (c Credential) Empty() bool { return c.Token == "" && len(c.SSHKey) == 0 }

// Resolver tells the registered hosts and the credentials users saved for them.
type Resolver interface {
	// Host returns the registered host, or ErrUnknownHost.
	Host(ctx context.Context, name string) (Host, error)
	// Credential returns what login saved for the host: the user's own, else the host's shared service token
	// when the administrator set one; ErrNoCredential when neither exists.
	Credential(ctx context.Context, login, host string) (Credential, error)
}

type loginKey struct{}

// WithLogin returns a context that carries the user on whose behalf forge calls are made.
func WithLogin(ctx context.Context, login string) context.Context {
	return context.WithValue(ctx, loginKey{}, login)
}

// LoginFrom returns the user set by WithLogin ("" when none).
func LoginFrom(ctx context.Context) string {
	s, _ := ctx.Value(loginKey{}).(string)
	return s
}
