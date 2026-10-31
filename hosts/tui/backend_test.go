package tui

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/client"
)

// recClient is a client.Client that records what the backend asks of it.
// Methods the tests do not exercise panic through the nil embedded interface.
type recClient struct {
	client.Client
	sends    []recSend
	resumes  []client.ResumeRequest
	patches  []client.ThreadPatch
	deleted  []string
	steered  []string
	canceled []string
	runErr   error // returned by Steer and Cancel
	list     client.ModelList
	listErr  error
	sendErr  error
	eventsCh chan client.Event
	evErr    error
}

type recSend struct {
	thread, text string
	opts         client.SendOptions
}

func (c *recClient) Send(_ context.Context, thread, text string, o client.SendOptions) (*client.Run, error) {
	c.sends = append(c.sends, recSend{thread, text, o})
	if c.sendErr != nil {
		return nil, c.sendErr
	}
	return &client.Run{ID: "run-1"}, nil
}
func (c *recClient) Resume(_ context.Context, thread string, r client.ResumeRequest) (*client.Run, error) {
	c.resumes = append(c.resumes, r)
	if thread == "bad" {
		return nil, errors.New("no such thread")
	}
	return &client.Run{ID: "run-2", ThreadID: thread}, nil
}
func (c *recClient) Steer(_ context.Context, run, text string) error {
	c.steered = append(c.steered, run+":"+text)
	return c.runErr
}
func (c *recClient) Cancel(_ context.Context, run string) error {
	c.canceled = append(c.canceled, run)
	return c.runErr
}
func (c *recClient) Events(context.Context, string, int64) (<-chan client.Event, error) {
	return c.eventsCh, c.evErr
}
func (c *recClient) Models(context.Context) (client.ModelList, error) { return c.list, c.listErr }
func (c *recClient) Sessions(_ context.Context, limit int) ([]client.Session, error) {
	return []client.Session{{ID: "s", Messages: limit}}, nil
}
func (c *recClient) CreateThread(context.Context, string) (*client.Thread, error) {
	return &client.Thread{ID: "new"}, nil
}
func (c *recClient) GetThread(_ context.Context, id string) (*client.Thread, error) {
	if id == "gone" {
		return nil, client.ErrNotFound
	}
	return &client.Thread{ID: id}, nil
}
func (c *recClient) RenameThread(_ context.Context, id string, p client.ThreadPatch) (*client.Thread, error) {
	c.patches = append(c.patches, p)
	return &client.Thread{ID: id}, nil
}
func (c *recClient) DeleteThread(_ context.Context, id string) error {
	c.deleted = append(c.deleted, id)
	return nil
}

// extClient adds the optional Extras on top of recClient.
type extClient struct {
	recClient
	forkErr error
}

func (c *extClient) Compact(context.Context, string) (*client.CompactResult, error) {
	return &client.CompactResult{Compacted: true}, nil
}
func (c *extClient) Undo(_ context.Context, th string) (*client.UndoResult, error) {
	return &client.UndoResult{Prompt: "undo:" + th}, nil
}
func (c *extClient) Redo(_ context.Context, th string) (*client.UndoResult, error) {
	return &client.UndoResult{Prompt: "redo:" + th}, nil
}
func (c *extClient) Fork(_ context.Context, th, msg string) (*client.Thread, error) {
	if c.forkErr != nil {
		return nil, c.forkErr
	}
	return &client.Thread{ID: th + "/fork@" + msg}, nil
}
func (c *extClient) Commands(context.Context) ([]client.Command, error) {
	return []client.Command{{Name: "review"}}, nil
}
func (c *extClient) ExpandCommand(_ context.Context, name, args string) (string, error) {
	return name + " " + args, nil
}

func TestClientBackendSendsTheRunOptionsAndAttachmentsOnlyOnce(t *testing.T) {
	// Arrange
	c := &recClient{}
	b := &ClientBackend{C: c, ThreadID: "t1", ModelID: "m", Interactive: true}
	b.SetEffort("high")
	b.SetMode(client.ModePlan)
	b.Attach(client.Attachment{Name: "a.png"}, client.Attachment{Name: "b.png"})

	// Act
	id1, err1 := b.Send(context.Background(), "first")
	_, err2 := b.Send(context.Background(), "second")

	// Assert
	if err1 != nil || err2 != nil || id1 != "run-1" {
		t.Fatalf("id %q errs %v %v", id1, err1, err2)
	}
	first, second := c.sends[0], c.sends[1]
	if first.thread != "t1" || first.opts.Model != "m" || !first.opts.Interactive || first.opts.ReasoningEffort != "high" || first.opts.Mode != client.ModePlan {
		t.Fatalf("first send %+v", first)
	}
	if len(first.opts.Attachments) != 2 {
		t.Fatalf("attachments of the first send: %+v", first.opts.Attachments)
	}
	if len(second.opts.Attachments) != 0 {
		t.Fatalf("attachments repeated on the next send: %+v", second.opts.Attachments)
	}
	if b.Effort() != "high" || b.Mode() != client.ModePlan {
		t.Fatalf("effort %q mode %q", b.Effort(), b.Mode())
	}
}

func TestClientBackendModeDefaultsToBuild(t *testing.T) {
	b := &ClientBackend{}

	if got := b.Mode(); got != client.ModeBuild {
		t.Fatalf("mode %q", got)
	}
}

func TestClientBackendSendFailureReturnsTheError(t *testing.T) {
	c := &recClient{sendErr: errors.New("queue full")}
	b := &ClientBackend{C: c}

	id, err := b.Send(context.Background(), "hi")

	if id != "" || err == nil || err.Error() != "queue full" {
		t.Fatalf("id %q err %v", id, err)
	}
}

func TestClientBackendEventsReportAnUnstreamableRunAsARunError(t *testing.T) {
	c := &recClient{evErr: client.ErrNotFound}
	b := &ClientBackend{C: c}

	ch := b.Events(context.Background(), "gone")
	e, ok := <-ch
	_, open := <-ch

	if !ok || e.Type != agui.RunError || open {
		t.Fatalf("event %+v ok %v, channel still open %v", e, ok, open)
	}
	var d struct{ Code, Message string }
	if err := json.Unmarshal(e.Data, &d); err != nil || d.Code != "stream_failed" || d.Message == "" {
		t.Fatalf("data %s (%v)", e.Data, err)
	}
}

func TestClientBackendEventsPassTheStreamThrough(t *testing.T) {
	src := make(chan client.Event, 1)
	src <- client.Event{Seq: 7, Type: agui.RunStarted}
	close(src)
	b := &ClientBackend{C: &recClient{eventsCh: src}}

	e := <-b.Events(context.Background(), "r")

	if e.Seq != 7 || e.Type != agui.RunStarted {
		t.Fatalf("event %+v", e)
	}
}

func TestClientBackendResumeCarriesTheDecisionAndTheRunSettings(t *testing.T) {
	c := &recClient{}
	b := &ClientBackend{C: c, ThreadID: "t1", ModelID: "m", Interactive: true, ReasoningEffort: "low"}

	id, err := b.Resume(context.Background(), ResumeRequest{InterruptID: "i1", Reason: client.ReasonToolApproval, Decision: DecisionApprove, Always: true, Text: "ok"})

	want := client.ResumeRequest{InterruptID: "i1", Reason: client.ReasonToolApproval, Decision: DecisionApprove, Always: true, Text: "ok", Model: "m", Interactive: true, ReasoningEffort: "low"}
	if err != nil || id != "run-2" || !reflect.DeepEqual(c.resumes[0], want) {
		t.Fatalf("id %q err %v sent %+v", id, err, c.resumes[0])
	}
}

func TestClientBackendResumeFailureReturnsTheError(t *testing.T) {
	b := &ClientBackend{C: &recClient{}, ThreadID: "bad"}

	id, err := b.Resume(context.Background(), ResumeRequest{})

	if id != "" || err == nil {
		t.Fatalf("id %q err %v", id, err)
	}
}

func TestClientBackendModelsListTheDefaultFirstWithoutDuplicates(t *testing.T) {
	c := &recClient{list: client.ModelList{Default: "b", Models: []string{"a", "b", "", "c"}}}
	b := &ClientBackend{C: c, Discover: func(context.Context) ([]string, error) { return []string{"c", "d"}, errors.New("partial") }}

	got, err := b.Models(context.Background())

	if err != nil || !reflect.DeepEqual(got, []string{"b", "a", "c", "d"}) {
		t.Fatalf("models %v err %v", got, err)
	}
}

func TestClientBackendModelsFailWhenOnlyDiscoveryWasTriedAndFailed(t *testing.T) {
	boom := errors.New("provider down")
	b := &ClientBackend{C: &recClient{}, Discover: func(context.Context) ([]string, error) { return nil, boom }}

	got, err := b.Models(context.Background())

	if !errors.Is(err, boom) || got != nil {
		t.Fatalf("models %v err %v", got, err)
	}
}

func TestClientBackendModelsPropagateTheServerError(t *testing.T) {
	b := &ClientBackend{C: &recClient{listErr: client.ErrNotSupported}}

	if _, err := b.Models(context.Background()); !errors.Is(err, client.ErrNotSupported) {
		t.Fatalf("err %v", err)
	}
}

func TestClientBackendSetModelChangesTheModelOfTheNextRun(t *testing.T) {
	c := &recClient{}
	b := &ClientBackend{C: c, ModelID: "old"}

	b.SetModel("new")
	b.Send(context.Background(), "x")

	if b.Model() != "new" || c.sends[0].opts.Model != "new" {
		t.Fatalf("model %q sent %q", b.Model(), c.sends[0].opts.Model)
	}
}

func TestClientBackendThreadSwitchingRedirectsTheNextSend(t *testing.T) {
	c := &recClient{}
	b := &ClientBackend{C: c, ThreadID: "t1"}

	if err := b.NewThread(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Send(context.Background(), "a")
	th, err := b.OpenThread(context.Background(), "t9")
	b.Send(context.Background(), "b")
	_, gone := b.OpenThread(context.Background(), "gone")
	b.Send(context.Background(), "c")

	if err != nil || th.ID != "t9" || !errors.Is(gone, client.ErrNotFound) {
		t.Fatalf("open: %+v %v / %v", th, err, gone)
	}
	got := []string{c.sends[0].thread, c.sends[1].thread, c.sends[2].thread}
	if !reflect.DeepEqual(got, []string{"new", "t9", "t9"}) || b.CurrentThread() != "t9" {
		t.Fatalf("threads %v (failed open must keep the current one)", got)
	}
	ss, _ := b.Sessions(context.Background())
	if len(ss) != 1 || ss[0].Messages != 50 {
		t.Fatalf("sessions %+v", ss)
	}
}

func TestClientBackendThreadAdminSendsOnlyTheChangedField(t *testing.T) {
	c := &recClient{}
	b := &ClientBackend{C: c}

	b.RenameSession(context.Background(), "t1", "New title")
	b.PinSession(context.Background(), "t1", false)
	b.DeleteSession(context.Background(), "t2")

	title, pin := c.patches[0], c.patches[1]
	if title.Title == nil || *title.Title != "New title" || title.Pinned != nil {
		t.Fatalf("rename patch %+v", title)
	}
	if pin.Pinned == nil || *pin.Pinned || pin.Title != nil {
		t.Fatalf("pin patch %+v (unpinning must be sent as false, not omitted)", pin)
	}
	if !reflect.DeepEqual(c.deleted, []string{"t2"}) {
		t.Fatalf("deleted %v", c.deleted)
	}
}

func TestClientBackendWithoutExtrasReportsNotSupported(t *testing.T) {
	b := &ClientBackend{C: &recClient{}}
	ctx := context.Background()

	_, e1 := b.Compact(ctx)
	_, e2 := b.Undo(ctx)
	_, e3 := b.Redo(ctx)
	_, e4 := b.Fork(ctx, "m")
	_, e5 := b.Commands(ctx)
	_, e6 := b.ExpandCommand(ctx, "x", "")

	for i, err := range []error{e1, e2, e3, e4, e5, e6} {
		if !errors.Is(err, client.ErrNotSupported) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestClientBackendExtrasActOnTheCurrentThread(t *testing.T) {
	b := &ClientBackend{C: &extClient{}, ThreadID: "t1"}
	ctx := context.Background()

	c, _ := b.Compact(ctx)
	u, _ := b.Undo(ctx)
	r, _ := b.Redo(ctx)
	cmds, _ := b.Commands(ctx)
	exp, _ := b.ExpandCommand(ctx, "review", "main.go")

	if !c.Compacted || u.Prompt != "undo:t1" || r.Prompt != "redo:t1" || len(cmds) != 1 || exp != "review main.go" {
		t.Fatalf("%+v %+v %+v %+v %q", c, u, r, cmds, exp)
	}
}

func TestClientBackendForkSwitchesToTheCopyOnlyOnSuccess(t *testing.T) {
	c := &extClient{}
	b := &ClientBackend{C: c, ThreadID: "t1"}

	th, err := b.Fork(context.Background(), "m3")
	c.forkErr = errors.New("busy")
	_, err2 := b.Fork(context.Background(), "m4")

	if err != nil || th.ID != "t1/fork@m3" || err2 == nil {
		t.Fatalf("fork %+v %v / %v", th, err, err2)
	}
	if b.CurrentThread() != "t1/fork@m3" {
		t.Fatalf("current thread %q: a failed fork must not move it", b.CurrentThread())
	}
}

func TestClientBackendSteerAndCancelTargetTheRunAndSurfaceErrors(t *testing.T) {
	c := &recClient{}
	b := &ClientBackend{C: c}

	errSteer := b.Steer(context.Background(), "r1", "use go 1.26")
	errCancel := b.Cancel(context.Background(), "r1")
	c.runErr = client.ErrRunNotActive
	errLate := b.Steer(context.Background(), "r1", "too late")

	if errSteer != nil || errCancel != nil || !errors.Is(errLate, client.ErrRunNotActive) {
		t.Fatalf("errors %v %v %v", errSteer, errCancel, errLate)
	}
	if !reflect.DeepEqual(c.steered, []string{"r1:use go 1.26", "r1:too late"}) || !reflect.DeepEqual(c.canceled, []string{"r1"}) {
		t.Fatalf("steered %v canceled %v", c.steered, c.canceled)
	}
}
