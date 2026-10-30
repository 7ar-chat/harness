package store

import (
	"context"
	"errors"
	"net/url"
	"time"
)

// Project roles (Member.Role), from the most to the least powerful.
const (
	// RoleOwner manages members, settings and the repository link.
	RoleOwner = "owner"
	// RoleMember reads and works: creates threads, sends, steers, approves.
	RoleMember = "member"
	// RoleViewer only reads.
	RoleViewer = "viewer"
)

// Thread kinds (ThreadMeta.Kind).
const (
	ThreadChat       = "chat"
	ThreadSupervisor = "supervisor"
)

var (
	// ErrForbidden means the user is known but may not do this.
	ErrForbidden = errors.New("forbidden")
	// ErrLastOwner means the change would leave a project without owner.
	ErrLastOwner = errors.New("a project needs at least one owner")
)

// ValidRole reports whether r is a project role.
func ValidRole(r string) bool { return r == RoleOwner || r == RoleMember || r == RoleViewer }

// RoleAtLeast reports whether have is at least as powerful as want.
func RoleAtLeast(have, want string) bool {
	rank := map[string]int{RoleViewer: 1, RoleMember: 2, RoleOwner: 3}
	return rank[have] > 0 && rank[have] >= rank[want]
}

// User is a person known from the identity provider. Login is the stable key
// used everywhere (threads, memory, membership): the OIDC subject.
type User struct {
	Login     string    `json:"login"`
	Email     string    `json:"email,omitempty"`
	Name      string    `json:"name,omitempty"`
	AvatarURL string    `json:"avatarUrl,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
	// DisplayName is the name the user chose; empty means Name (from the identity provider: first and last name).
	DisplayName string `json:"displayName,omitempty"`
	// AvatarVersion identifies the uploaded avatar ("" when none); AvatarURL is then superseded.
	AvatarVersion string `json:"avatarVersion,omitempty"`
}

// Label is the name shown for the user: their display name, else the provider's name, else the login.
func (u User) Label() string {
	switch {
	case u.DisplayName != "":
		return u.DisplayName
	case u.Name != "":
		return u.Name
	}
	return u.Login
}

// Avatar is the avatar URL to show: the uploaded one, else the provider's.
func (u User) Avatar() string {
	if u.AvatarVersion != "" {
		return "/api/users/" + url.PathEscape(u.Login) + "/avatar?v=" + u.AvatarVersion
	}
	return u.AvatarURL
}

// Connector kinds.
const (
	ConnectorGitHub   = "github"
	ConnectorTelegram = "telegram"
)

// Connector links a user to an outside account (GitHub, Telegram). Data is
// opaque to the store (the host encrypts it: tokens live there).
type Connector struct {
	Login     string    `json:"login"`
	Kind      string    `json:"kind"`
	Account   string    `json:"account"` // the account's id at the provider (GitHub login, Telegram chat id)
	Label     string    `json:"label"`   // what to show (@name)
	Data      []byte    `json:"data"`
	CreatedAt time.Time `json:"createdAt"`
}

// Project is a repository the team works on.
type Project struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	RepoURL       string `json:"repoUrl,omitempty"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// GitHubInstallation is the id of the GitHub App installation that grants
	// access to the repository (0 when not connected).
	GitHubInstallation int64     `json:"githubInstallation,omitempty"`
	CreatedBy          string    `json:"createdBy"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// ProjectPatch is a partial project update.
type ProjectPatch struct {
	Name               *string
	Description        *string
	RepoURL            *string
	DefaultBranch      *string
	GitHubInstallation *int64
}

// Member is a user's role in a project.
type Member struct {
	ProjectID string    `json:"projectId"`
	Login     string    `json:"login"`
	Role      string    `json:"role"`
	AddedAt   time.Time `json:"addedAt"`
}

// Invite is a membership waiting for its email to log in.
type Invite struct {
	ProjectID string    `json:"projectId"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	InvitedBy string    `json:"invitedBy"`
	CreatedAt time.Time `json:"createdAt"`
}

// ThreadMeta ties a thread to a project and records who created it. Owner is
// the login the thread is stored under (store calls for the thread use it);
// CreatedBy is the user who started it.
type ThreadMeta struct {
	ThreadID  string    `json:"threadId"`
	ProjectID string    `json:"projectId,omitempty"`
	Owner     string    `json:"owner"`
	CreatedBy string    `json:"createdBy"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"createdAt"`
}

// AuditEntry records an action taken on a project or thread by a user.
type AuditEntry struct {
	ID         int64          `json:"id"`
	ProjectID  string         `json:"projectId,omitempty"`
	ThreadID   string         `json:"threadId,omitempty"`
	Actor      string         `json:"actor"`
	OnBehalfOf string         `json:"onBehalfOf,omitempty"`
	Action     string         `json:"action"`
	Detail     map[string]any `json:"detail,omitempty"`
	At         time.Time      `json:"at"`
}

// TeamStore persists users, projects, memberships and the thread-to-project
// mapping. It is separate from Store: threads stay stored under their owner's
// login and the host decides, from here, who may act on them.
type TeamStore interface {
	// UpsertUser records the user at login and claims the invites sent to the
	// email (the memberships are created).
	UpsertUser(ctx context.Context, u User) (*User, error)
	GetUser(ctx context.Context, login string) (*User, error)
	// GetUsers returns the known users among logins, by login.
	GetUsers(ctx context.Context, logins []string) (map[string]*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	// SetDisplayName sets the name the user chose ("" goes back to the provider's).
	SetDisplayName(ctx context.Context, login, name string) error
	// SetAvatar stores the user's avatar image and returns its version; empty data removes it.
	SetAvatar(ctx context.Context, login, mime string, data []byte) (string, error)
	// GetAvatar returns the uploaded avatar, ErrNotFound when none.
	GetAvatar(ctx context.Context, login string) (mime string, data []byte, err error)

	// Connectors: SetConnector replaces the user's connector of that kind.
	SetConnector(ctx context.Context, c Connector) error
	GetConnector(ctx context.Context, login, kind string) (*Connector, error)
	ListConnectors(ctx context.Context, login string) ([]Connector, error)
	DeleteConnector(ctx context.Context, login, kind string) error
	// FindConnector finds the connector of a provider account (who owns Telegram chat 123).
	FindConnector(ctx context.Context, kind, account string) (*Connector, error)

	// ListUsers returns every known user (small teams: the supervisor watcher walks them).
	ListUsers(ctx context.Context) ([]User, error)
	// UserPrefs returns the user's preferences (an empty map when none were set).
	UserPrefs(ctx context.Context, login string) (map[string]any, error)
	// SetUserPrefs replaces the user's preferences.
	SetUserPrefs(ctx context.Context, login string, prefs map[string]any) error

	// SeenThreads returns when login last saw each thread (by thread id); threads never seen are absent.
	SeenThreads(ctx context.Context, login string) (map[string]time.Time, error)
	// SetThreadSeen records that login has seen the thread up to at. The time never moves backwards, so
	// replays and several devices converge on the latest.
	SetThreadSeen(ctx context.Context, login, threadID string, at time.Time) error

	// CreateProject stores p (a fresh id when empty) and makes owner its owner.
	CreateProject(ctx context.Context, p Project, owner string) (*Project, error)
	GetProject(ctx context.Context, id string) (*Project, error)
	UpdateProject(ctx context.Context, id string, patch ProjectPatch) (*Project, error)
	// DeleteProject removes the project, its memberships, invites and thread links.
	DeleteProject(ctx context.Context, id string) error
	// ListProjects returns the projects login belongs to, with its role.
	ListProjects(ctx context.Context, login string) ([]ProjectRole, error)

	// MemberRole is login's role in the project, ErrNotFound when not a member.
	MemberRole(ctx context.Context, projectID, login string) (string, error)
	ListMembers(ctx context.Context, projectID string) ([]Member, error)
	// SetMember adds login or changes its role (ErrLastOwner when it would demote the last owner).
	SetMember(ctx context.Context, projectID, login, role string) error
	// RemoveMember removes login (ErrLastOwner for the last owner).
	RemoveMember(ctx context.Context, projectID, login string) error
	// CreateInvite invites an email; when a user with that email exists it is added at once and nil is returned.
	CreateInvite(ctx context.Context, in Invite) (*Invite, error)
	ListInvites(ctx context.Context, projectID string) ([]Invite, error)
	DeleteInvite(ctx context.Context, projectID, email string) error

	// SetThreadMeta links a thread (idempotent).
	SetThreadMeta(ctx context.Context, m ThreadMeta) error
	// GetThreadMeta returns the link of a thread, ErrNotFound for a thread
	// created before the team layer (then only its owner has access).
	GetThreadMeta(ctx context.Context, threadID string) (*ThreadMeta, error)
	ListProjectThreads(ctx context.Context, projectID string) ([]ThreadMeta, error)
	// SupervisorThread returns the user's supervisor thread link, ErrNotFound when none.
	SupervisorThread(ctx context.Context, login string) (*ThreadMeta, error)
	DeleteThreadMeta(ctx context.Context, threadID string) error

	Audit(ctx context.Context, e AuditEntry) error
	ListAudit(ctx context.Context, projectID string, limit int) ([]AuditEntry, error)
}

// ProjectRole is a project with the caller's role in it.
type ProjectRole struct {
	Project
	Role string `json:"role"`
}
