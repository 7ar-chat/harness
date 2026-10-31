package inproc

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/filestore"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/hosts/client"
)

// maxDownload is the largest file DownloadFile returns.
const maxDownload = 100 << 20

// Option configures New.
type Option func(*impl)

// WithModels sets the model ids Models reports and the default model used when
// SendOptions.Model is empty (the engine does not expose its configuration).
func WithModels(models []string, def string) Option {
	return func(c *impl) { c.models, c.def = models, def }
}

// WithModelInfo sets where Models gets the description of each model it lists
// (context window, prices, capabilities); without it the list carries none.
func WithModelInfo(lookup func(id string) (client.ModelInfo, bool)) Option {
	return func(c *impl) { c.info = lookup }
}

// WithAuthor sets who the client's messages and steers are recorded as coming
// from, when that is not the thread owner the client acts for (a project
// member working on a shared thread, or a supervisor).
func WithAuthor(a harness.Author) Option {
	return func(c *impl) { c.author = a }
}

var _ client.Extras = (*impl)(nil)

type impl struct {
	author harness.Author
	h      *harness.Harness
	user   string
	models []string
	def    string
	info   func(id string) (client.ModelInfo, bool)
	// projectDir is where Commands looks for the project's .har directory from.
	projectDir string
}

// New returns a client.Client for user over a harness running in this process.
// It is the one place, with hosts/web's server side and the commands, that
// imports the facade; front ends use the Client.
func New(h *harness.Harness, user string, opts ...Option) client.Client {
	c := &impl{h: h, user: user}
	for _, o := range opts {
		o(c)
	}
	return c
}

// mapErr turns an engine error into the API error.
func mapErr(err error) error {
	var api *client.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &api):
		return err
	case errors.Is(err, store.ErrNotFound):
		return client.ErrNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrRunActive), errors.Is(err, store.ErrNoPendingQuestion):
		return client.ErrConflict
	case errors.Is(err, harness.ErrInvalidRequest):
		return client.Wrap(client.ErrInvalidRequest, err.Error(), err)
	case errors.Is(err, queue.ErrSteersClosed):
		return client.ErrRunNotActive
	case errors.Is(err, queue.ErrTooManySteers):
		return client.ErrTooManySteers
	case errors.Is(err, sandbox.ErrNotFound):
		return client.ErrNotFound
	}
	if !errors.Is(err, context.Canceled) {
		slog.Error("client: request failed", "error", err)
	}
	return client.Wrap(&client.Error{Status: 500, Code: "internal"}, "An internal error occurred.", err)
}

func (c *impl) Models(context.Context) (client.ModelList, error) {
	m := c.models
	if m == nil {
		m = []string{}
	}
	list := client.ModelList{Models: m, Default: c.def}
	if c.info != nil {
		for _, id := range append([]string{c.def}, m...) {
			if i, ok := c.info(id); ok && id != "" {
				if list.Info == nil {
					list.Info = map[string]client.ModelInfo{}
				}
				list.Info[id] = i
			}
		}
	}
	return list, nil
}

func toThread(t *store.Thread) client.Thread {
	d := client.Thread{ID: t.ID, Title: t.Title, Pinned: t.PinnedAt != nil, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		ActiveRun: t.ActiveRun, PendingQuestion: t.PendingQuestion}
	if t.PendingApproval != nil {
		d.PendingApproval = t.PendingApproval.Payload()
	}
	return d
}

func toAuthor(m *store.Message) *client.Author {
	if m.AuthorLogin == "" && m.AuthorKind == "" {
		return nil
	}
	return &client.Author{Login: m.AuthorLogin, Kind: m.AuthorKind, OnBehalfOf: m.OnBehalfOf}
}

func toRun(r *store.Run) *client.Run {
	return &client.Run{ID: r.ID, ThreadID: r.ThreadID, Status: r.Status, Model: r.Model, Error: r.Error, ErrorCode: r.ErrorCode,
		LastSeq: r.LastSeq, CreatedAt: r.CreatedAt, Sandbox: r.Sandbox}
}

func limitOf(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, 200)
}

func (c *impl) CreateThread(ctx context.Context, title string) (*client.Thread, error) {
	th, err := c.h.NewThread(ctx, c.user, title)
	if err != nil {
		return nil, mapErr(err)
	}
	d := toThread(th)
	return &d, nil
}

func (c *impl) ListThreads(ctx context.Context, limit int) ([]client.Thread, error) {
	ths, err := c.h.Store.ListThreads(ctx, c.user, limitOf(limit, 50), nil)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]client.Thread, 0, len(ths))
	for _, t := range ths {
		out = append(out, toThread(t))
	}
	return out, nil
}

func (c *impl) GetThread(ctx context.Context, id string) (*client.Thread, error) {
	th, err := c.h.Store.GetThread(ctx, c.user, id)
	if err != nil {
		return nil, mapErr(err)
	}
	msgs, err := c.h.Store.ListMessages(ctx, th.ID)
	if err != nil {
		return nil, mapErr(err)
	}
	d := toThread(th)
	d.Messages = make([]client.Message, 0, len(msgs))
	for _, m := range msgs {
		d.Messages = append(d.Messages, client.Message{ID: m.ID, RunID: m.RunID, Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls,
			ToolCallID: m.ToolCallID, Error: m.Error, Metadata: m.Metadata, CreatedAt: m.CreatedAt, Author: toAuthor(m)})
	}
	return &d, nil
}

func (c *impl) RenameThread(ctx context.Context, id string, p client.ThreadPatch) (*client.Thread, error) {
	if p.Title == nil && p.Pinned == nil {
		return nil, client.Errorf(client.ErrInvalidRequest, "title or pinned is required.")
	}
	if p.Title != nil {
		t := strings.TrimSpace(*p.Title)
		if t == "" || len([]rune(t)) > 200 {
			return nil, client.Errorf(client.ErrInvalidRequest, "title must be 1 to 200 characters.")
		}
		p.Title = &t
	}
	th, err := c.h.Store.UpdateThread(ctx, c.user, id, store.ThreadPatch{Title: p.Title, Pinned: p.Pinned})
	if err != nil {
		return nil, mapErr(err)
	}
	d := toThread(th)
	return &d, nil
}

func (c *impl) DeleteThread(ctx context.Context, id string) error {
	_, err := c.h.Store.DeleteThread(ctx, c.user, id)
	return mapErr(err)
}

func (c *impl) modelOr(m string) string {
	if m == "" {
		return c.def
	}
	return m
}

// owns reports client.ErrNotFound unless the thread belongs to the user.
func (c *impl) owns(ctx context.Context, threadID string) error {
	_, err := c.h.Store.GetThread(ctx, c.user, threadID)
	return mapErr(err)
}

func (c *impl) Send(ctx context.Context, threadID, text string, o client.SendOptions) (*client.Run, error) {
	if strings.TrimSpace(text) == "" {
		return nil, client.Errorf(client.ErrInvalidRequest, "message is required.")
	}
	if err := c.owns(ctx, threadID); err != nil {
		return nil, err
	}
	run, err := c.h.Send(ctx, c.user, threadID, text, harness.RunOptions{
		Model: c.modelOr(o.Model), ReasoningEffort: o.ReasoningEffort, Skills: o.Skills, Interactive: o.Interactive,
		Sandbox: harness.SandboxMode(o.Sandbox), Mode: harness.RunMode(o.Mode), Attachments: toAttachments(o.Attachments),
		Author: c.author.Login, AuthorKind: c.author.Kind, OnBehalfOf: c.author.OnBehalfOf,
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return toRun(run), nil
}

func toAttachments(list []client.Attachment) []harness.Attachment {
	var out []harness.Attachment
	for _, a := range list {
		out = append(out, harness.Attachment{Name: a.Name, MIME: a.MIME, Data: a.Data})
	}
	return out
}

func (c *impl) GetRun(ctx context.Context, runID string) (*client.Run, error) {
	run, err := c.h.Store.GetUserRun(ctx, c.user, runID)
	if err != nil {
		return nil, mapErr(err)
	}
	return toRun(run), nil
}

func (c *impl) Events(ctx context.Context, runID string, after int64) (<-chan client.Event, error) {
	run, err := c.h.Store.GetUserRun(ctx, c.user, runID)
	if err != nil {
		return nil, mapErr(err)
	}
	src := c.h.Events(ctx, run.ID, after)
	out := make(chan client.Event)
	go func() {
		defer close(out)
		for ev := range src {
			select {
			case out <- client.Event{Seq: ev.Seq, Type: ev.Type, Data: ev.Data}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (c *impl) Steer(ctx context.Context, runID, text string) error {
	if strings.TrimSpace(text) == "" {
		return client.Errorf(client.ErrInvalidRequest, "text is required.")
	}
	return mapErr(c.h.SteerAs(ctx, c.user, runID, text, c.author))
}

func (c *impl) Cancel(ctx context.Context, runID string) error {
	return mapErr(c.h.Cancel(ctx, c.user, runID))
}

func (c *impl) Resume(ctx context.Context, threadID string, r client.ResumeRequest) (*client.Run, error) {
	if r.InterruptID == "" {
		return nil, client.Errorf(client.ErrInvalidRequest, "interruptId is required.")
	}
	if r.Reason == "" {
		r.Reason = "ask_user"
		if r.Decision != "" {
			r.Reason = store.ReasonToolApproval
		}
	}
	if r.Reason == store.ReasonToolApproval && r.Decision != store.DecisionApprove && r.Decision != store.DecisionDeny && r.Decision != store.DecisionRespond {
		return nil, client.Errorf(client.ErrInvalidRequest, "decision must be approve, deny or respond.")
	}
	if err := c.owns(ctx, threadID); err != nil {
		return nil, err
	}
	run, err := c.h.Resume(ctx, c.user, threadID, harness.ResumeRequest{
		InterruptID: r.InterruptID, Reason: r.Reason, Decision: r.Decision, Always: r.Always, Text: r.Text,
		Model: c.modelOr(r.Model), Interactive: r.Interactive, ReasoningEffort: r.ReasoningEffort, Mode: harness.RunMode(r.Mode),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return toRun(run), nil
}

// confine resolves a path inside /workspace: "" is the workspace itself, a
// relative path is taken under it, and anything that escapes it is refused.
func confine(p string) (string, bool) {
	if strings.ContainsRune(p, 0) {
		return "", false
	}
	if p == "" {
		return sandbox.Workspace, true
	}
	if !strings.HasPrefix(p, "/") {
		p = sandbox.Workspace + "/" + p
	}
	// Clean on the rooted path resolves every "..", so the prefix test below is final.
	p = path.Clean(p)
	if p != sandbox.Workspace && !strings.HasPrefix(p, sandbox.Workspace+"/") {
		return "", false
	}
	return p, true
}

// sandboxOf returns the thread's sandbox; empty is true when the thread has none yet.
func (c *impl) sandboxOf(ctx context.Context, threadID string) (sb sandbox.Sandbox, empty bool, err error) {
	if err := c.owns(ctx, threadID); err != nil {
		return nil, false, err
	}
	if c.h.Sandboxes == nil || !c.h.Sandboxes.Enabled() {
		return nil, false, client.ErrNoSandbox
	}
	sb, err = c.h.Sandboxes.Get(ctx, threadID)
	if errors.Is(err, sandbox.ErrNoSandbox) || errors.Is(err, sandbox.ErrNotFound) {
		return nil, true, nil
	}
	return sb, false, mapErr(err)
}

func (c *impl) ListFiles(ctx context.Context, threadID, p string) ([]client.File, error) {
	dir, ok := confine(p)
	if !ok {
		return nil, client.Errorf(client.ErrInvalidPath, "The path must be inside /workspace.")
	}
	sb, empty, err := c.sandboxOf(ctx, threadID)
	if err != nil {
		return nil, err
	}
	out := []client.File{}
	if empty {
		return out, nil
	}
	infos, err := sb.List(ctx, dir, 1)
	if errors.Is(err, sandbox.ErrNotFound) {
		infos, err = nil, nil
	}
	if err != nil {
		return nil, mapErr(err)
	}
	for _, f := range infos {
		fp, ok := confine(f.Path)
		if !ok || fp == dir {
			continue
		}
		out = append(out, client.File{Name: path.Base(fp), Path: fp, Type: f.Type, Size: f.Size, ModifiedAt: f.ModifiedAt})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "directory") != (out[j].Type == "directory") {
			return out[i].Type == "directory"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (c *impl) DownloadFile(ctx context.Context, threadID, p string) (*client.Download, error) {
	fp, ok := confine(p)
	if !ok || fp == sandbox.Workspace {
		return nil, client.Errorf(client.ErrInvalidPath, "The path must be a file inside /workspace.")
	}
	sb, empty, err := c.sandboxOf(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, client.ErrNotFound
	}
	info, err := sb.Stat(ctx, fp)
	if err != nil {
		return nil, mapErr(err)
	}
	if info.Type == "directory" {
		return nil, client.Errorf(client.ErrInvalidPath, "A directory cannot be downloaded.")
	}
	if info.Size > maxDownload {
		return nil, client.ErrTooLarge
	}
	data, _, err := sb.ReadFile(ctx, fp, maxDownload)
	if err != nil {
		return nil, mapErr(err)
	}
	return &client.Download{Path: fp, Name: path.Base(fp), Data: data}, nil
}

func (c *impl) Sessions(ctx context.Context, limit int) ([]client.Session, error) {
	fs, ok := c.h.Store.(interface{ Sessions() filestore.Sessions })
	if !ok {
		return nil, client.ErrNotSupported
	}
	list, err := fs.Sessions().List(ctx, c.user, limitOf(limit, 50))
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]client.Session, 0, len(list))
	for _, i := range list {
		out = append(out, client.Session{ID: i.ID, Title: i.Title, Preview: i.Preview, Messages: i.Messages, LastStatus: i.LastStatus, Pinned: i.Pinned, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt})
	}
	return out, nil
}
