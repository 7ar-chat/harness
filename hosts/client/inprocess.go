package client

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"sort"
	"strings"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/engine/queue"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/filestore"
)

// maxDownload is the largest file DownloadFile returns.
const maxDownload = 100 << 20

// InProcessOption configures InProcess.
type InProcessOption func(*inProcess)

// WithModels sets the model ids Models reports and the default model used when
// SendOptions.Model is empty (the engine does not expose its configuration).
func WithModels(models []string, def string) InProcessOption {
	return func(c *inProcess) { c.models, c.def = models, def }
}

type inProcess struct {
	h      *harness.Harness
	user   string
	models []string
	def    string
	// projectDir is where Commands looks for the project's .har directory from.
	projectDir string
}

// InProcess returns a Client for user over a harness running in this process.
// It is the one place, with hosts/web's server side and the commands, that
// imports the facade; front ends use the Client.
func InProcess(h *harness.Harness, user string, opts ...InProcessOption) Client {
	c := &inProcess{h: h, user: user}
	for _, o := range opts {
		o(c)
	}
	return c
}

// mapErr turns an engine error into the API error.
func mapErr(err error) error {
	var api *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &api):
		return err
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrRunActive):
		return ErrConflict
	case errors.Is(err, harness.ErrInvalidRequest):
		return &Error{Status: ErrInvalidRequest.Status, Code: ErrInvalidRequest.Code, Message: err.Error(), cause: err}
	case errors.Is(err, queue.ErrSteersClosed):
		return ErrRunNotActive
	case errors.Is(err, queue.ErrTooManySteers):
		return ErrTooManySteers
	case errors.Is(err, sandbox.ErrNotFound):
		return ErrNotFound
	}
	if !errors.Is(err, context.Canceled) {
		slog.Error("client: request failed", "error", err)
	}
	return &Error{Status: 500, Code: "internal", Message: "An internal error occurred.", cause: err}
}

func (c *inProcess) Models(context.Context) (ModelList, error) {
	m := c.models
	if m == nil {
		m = []string{}
	}
	return ModelList{Models: m, Default: c.def}, nil
}

func toThread(t *store.Thread) Thread {
	d := Thread{ID: t.ID, Title: t.Title, Pinned: t.PinnedAt != nil, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		ActiveRun: t.ActiveRun, PendingQuestion: t.PendingQuestion}
	if t.PendingApproval != nil {
		d.PendingApproval = t.PendingApproval.Payload()
	}
	return d
}

func toRun(r *store.Run) *Run {
	return &Run{ID: r.ID, ThreadID: r.ThreadID, Status: r.Status, Model: r.Model, Error: r.Error, ErrorCode: r.ErrorCode,
		LastSeq: r.LastSeq, CreatedAt: r.CreatedAt, Sandbox: r.Sandbox}
}

func limitOf(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, 200)
}

func (c *inProcess) CreateThread(ctx context.Context, title string) (*Thread, error) {
	th, err := c.h.NewThread(ctx, c.user, title)
	if err != nil {
		return nil, mapErr(err)
	}
	d := toThread(th)
	return &d, nil
}

func (c *inProcess) ListThreads(ctx context.Context, limit int) ([]Thread, error) {
	ths, err := c.h.Store.ListThreads(ctx, c.user, limitOf(limit, 50), nil)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]Thread, 0, len(ths))
	for _, t := range ths {
		out = append(out, toThread(t))
	}
	return out, nil
}

func (c *inProcess) GetThread(ctx context.Context, id string) (*Thread, error) {
	th, err := c.h.Store.GetThread(ctx, c.user, id)
	if err != nil {
		return nil, mapErr(err)
	}
	msgs, err := c.h.Store.ListMessages(ctx, th.ID)
	if err != nil {
		return nil, mapErr(err)
	}
	d := toThread(th)
	d.Messages = make([]Message, 0, len(msgs))
	for _, m := range msgs {
		d.Messages = append(d.Messages, Message{ID: m.ID, RunID: m.RunID, Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls,
			ToolCallID: m.ToolCallID, Error: m.Error, Metadata: m.Metadata, CreatedAt: m.CreatedAt})
	}
	return &d, nil
}

func (c *inProcess) RenameThread(ctx context.Context, id string, p ThreadPatch) (*Thread, error) {
	if p.Title == nil && p.Pinned == nil {
		return nil, errorf(ErrInvalidRequest, "title or pinned is required.")
	}
	if p.Title != nil {
		t := strings.TrimSpace(*p.Title)
		if t == "" || len([]rune(t)) > 200 {
			return nil, errorf(ErrInvalidRequest, "title must be 1 to 200 characters.")
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

func (c *inProcess) DeleteThread(ctx context.Context, id string) error {
	_, err := c.h.Store.DeleteThread(ctx, c.user, id)
	return mapErr(err)
}

func (c *inProcess) modelOr(m string) string {
	if m == "" {
		return c.def
	}
	return m
}

// owns reports ErrNotFound unless the thread belongs to the user.
func (c *inProcess) owns(ctx context.Context, threadID string) error {
	_, err := c.h.Store.GetThread(ctx, c.user, threadID)
	return mapErr(err)
}

func (c *inProcess) Send(ctx context.Context, threadID, text string, o SendOptions) (*Run, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errorf(ErrInvalidRequest, "message is required.")
	}
	if err := c.owns(ctx, threadID); err != nil {
		return nil, err
	}
	run, err := c.h.Send(ctx, c.user, threadID, text, harness.RunOptions{
		Model: c.modelOr(o.Model), ReasoningEffort: o.ReasoningEffort, Skills: o.Skills, Interactive: o.Interactive,
		Sandbox: harness.SandboxMode(o.Sandbox), Mode: harness.RunMode(o.Mode), Attachments: toAttachments(o.Attachments),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return toRun(run), nil
}

func toAttachments(list []Attachment) []harness.Attachment {
	var out []harness.Attachment
	for _, a := range list {
		out = append(out, harness.Attachment{Name: a.Name, MIME: a.MIME, Data: a.Data})
	}
	return out
}

func (c *inProcess) GetRun(ctx context.Context, runID string) (*Run, error) {
	run, err := c.h.Store.GetUserRun(ctx, c.user, runID)
	if err != nil {
		return nil, mapErr(err)
	}
	return toRun(run), nil
}

func (c *inProcess) Events(ctx context.Context, runID string, after int64) (<-chan Event, error) {
	run, err := c.h.Store.GetUserRun(ctx, c.user, runID)
	if err != nil {
		return nil, mapErr(err)
	}
	src := c.h.Events(ctx, run.ID, after)
	out := make(chan Event)
	go func() {
		defer close(out)
		for ev := range src {
			select {
			case out <- Event{Seq: ev.Seq, Type: ev.Type, Data: ev.Data}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (c *inProcess) Steer(ctx context.Context, runID, text string) error {
	if strings.TrimSpace(text) == "" {
		return errorf(ErrInvalidRequest, "text is required.")
	}
	return mapErr(c.h.Steer(ctx, c.user, runID, text))
}

func (c *inProcess) Cancel(ctx context.Context, runID string) error {
	return mapErr(c.h.Cancel(ctx, c.user, runID))
}

func (c *inProcess) Resume(ctx context.Context, threadID string, r ResumeRequest) (*Run, error) {
	if r.InterruptID == "" {
		return nil, errorf(ErrInvalidRequest, "interruptId is required.")
	}
	if r.Reason == "" {
		r.Reason = "ask_user"
		if r.Decision != "" {
			r.Reason = store.ReasonToolApproval
		}
	}
	if r.Reason == store.ReasonToolApproval && r.Decision != store.DecisionApprove && r.Decision != store.DecisionDeny && r.Decision != store.DecisionRespond {
		return nil, errorf(ErrInvalidRequest, "decision must be approve, deny or respond.")
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
func (c *inProcess) sandboxOf(ctx context.Context, threadID string) (sb sandbox.Sandbox, empty bool, err error) {
	if err := c.owns(ctx, threadID); err != nil {
		return nil, false, err
	}
	if c.h.Sandboxes == nil || !c.h.Sandboxes.Enabled() {
		return nil, false, ErrNoSandbox
	}
	sb, err = c.h.Sandboxes.Get(ctx, threadID)
	if errors.Is(err, sandbox.ErrNoSandbox) || errors.Is(err, sandbox.ErrNotFound) {
		return nil, true, nil
	}
	return sb, false, mapErr(err)
}

func (c *inProcess) ListFiles(ctx context.Context, threadID, p string) ([]File, error) {
	dir, ok := confine(p)
	if !ok {
		return nil, errorf(ErrInvalidPath, "The path must be inside /workspace.")
	}
	sb, empty, err := c.sandboxOf(ctx, threadID)
	if err != nil {
		return nil, err
	}
	out := []File{}
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
		out = append(out, File{Name: path.Base(fp), Path: fp, Type: f.Type, Size: f.Size, ModifiedAt: f.ModifiedAt})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Type == "directory") != (out[j].Type == "directory") {
			return out[i].Type == "directory"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (c *inProcess) DownloadFile(ctx context.Context, threadID, p string) (*Download, error) {
	fp, ok := confine(p)
	if !ok || fp == sandbox.Workspace {
		return nil, errorf(ErrInvalidPath, "The path must be a file inside /workspace.")
	}
	sb, empty, err := c.sandboxOf(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, ErrNotFound
	}
	info, err := sb.Stat(ctx, fp)
	if err != nil {
		return nil, mapErr(err)
	}
	if info.Type == "directory" {
		return nil, errorf(ErrInvalidPath, "A directory cannot be downloaded.")
	}
	if info.Size > maxDownload {
		return nil, ErrTooLarge
	}
	data, _, err := sb.ReadFile(ctx, fp, maxDownload)
	if err != nil {
		return nil, mapErr(err)
	}
	return &Download{Path: fp, Name: path.Base(fp), Data: data}, nil
}

func (c *inProcess) Sessions(ctx context.Context, limit int) ([]Session, error) {
	fs, ok := c.h.Store.(interface{ Sessions() filestore.Sessions })
	if !ok {
		return nil, ErrNotSupported
	}
	list, err := fs.Sessions().List(ctx, c.user, limitOf(limit, 50))
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]Session, 0, len(list))
	for _, i := range list {
		out = append(out, Session{ID: i.ID, Title: i.Title, Preview: i.Preview, Messages: i.Messages, LastStatus: i.LastStatus, Pinned: i.Pinned, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt})
	}
	return out, nil
}
