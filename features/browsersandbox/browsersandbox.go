// Package browsersandbox runs the agent's browser in a sandbox of its own, on a sandbox provider that may be another one
// than the code sandboxes use (BROWSER_BACKEND=daytona|e2b|opensandbox next to SANDBOX_BACKEND). A conversation that
// uses the browser gets one sandbox from the image sandbox-image/browser-sandbox: Steel's Chromium, which a person can
// watch (Steel's session viewer), and the Playwright MCP server attached to that same Chromium. The sandbox is kept for the
// conversation, so the page, the cookies and a half-filled form are still there in the next message, and is deleted
// after BROWSER_IDLE_MINUTES without use.
//
// The package knows nothing about MCP: Servers returns the MCP server to connect to, and package mcpfeature does the
// connecting.
package browsersandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
)

// Ports the image serves (sandbox-image/browser-sandbox).
const (
	MCPPort  = 8931 // the Playwright MCP server, at /mcp
	ViewPort = 3000 // Steel: the live view of the session at ViewPath, its REST API and CDP
)

// ViewPath is the page of Steel's live session viewer.
const ViewPath = "/v1/sessions/debug"

// Profile is the sandbox profile of browser sandboxes: the backend maps it to BROWSER_IMAGE.
const Profile = "browser"

// DocsDir is where the user's documents are put in a browser sandbox (the browser's file chooser reads them there).
const DocsDir = "/docs"

// Server is where the conversation's browser can be reached over MCP.
type Server struct {
	URL     string            // the MCP endpoint
	Headers map[string]string // the provider's access headers, sent with every request
}

// Library is the user's document library, as far as the browser needs it: the files the file chooser may read.
type Library interface {
	// Signature changes when the user's documents change.
	Signature(login string) (string, error)
	// CopyTo puts the user's documents in dir of the sandbox and returns the directory they are in.
	CopyTo(ctx context.Context, login string, sb sandbox.Sandbox) error
}

// Manager keeps one browser sandbox per conversation.
type Manager struct {
	Backend sandbox.Backend
	// Docs, when set, is copied into the sandbox so the browser can attach the user's documents to a form.
	Docs Library
	// Idle is how long an unused sandbox is kept (default 20 minutes).
	Idle time.Duration
	// StartTimeout bounds the start of the browser in a new sandbox (default 2 minutes).
	StartTimeout time.Duration

	mu    sync.Mutex
	boxes map[string]*box
	once  sync.Once
	stop  chan struct{}
}

type box struct {
	mu       sync.Mutex // held while the sandbox is being created or checked
	inst     sandbox.Instance
	server   Server
	viewURL  string
	docsSig  string
	lastUsed time.Time
}

// New returns a manager creating its sandboxes with backend.
func New(backend sandbox.Backend) *Manager {
	return &Manager{Backend: backend, boxes: map[string]*box{}, stop: make(chan struct{})}
}

func (m *Manager) idle() time.Duration {
	if m.Idle > 0 {
		return m.Idle
	}
	return 20 * time.Minute
}

// Servers returns the browser's MCP server for the conversation, starting its sandbox when it has none. It is the
// function the MCP provider calls at the start of every run.
func (m *Manager) Servers(ctx context.Context, threadID, login string) ([]Server, error) {
	if m == nil || m.Backend == nil {
		return nil, nil
	}
	m.once.Do(func() { go m.reap() })
	m.mu.Lock()
	b := m.boxes[threadID]
	if b == nil {
		b = &box{}
		m.boxes[threadID] = b
	}
	m.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inst != nil && !b.inst.Healthy(ctx) {
		slog.Info("browser sandbox is gone; starting another", "thread", threadID, "sandbox", b.inst.ID())
		m.kill(b.inst.ID())
		b.inst = nil
	}
	if b.inst == nil {
		if err := m.start(ctx, b, threadID, login); err != nil {
			m.mu.Lock()
			delete(m.boxes, threadID) // the next run tries again
			m.mu.Unlock()
			return nil, err
		}
	} else {
		_ = b.inst.Renew(ctx, m.idle()+10*time.Minute)
		m.syncDocs(ctx, b, login)
	}
	b.lastUsed = time.Now()
	return []Server{b.server}, nil
}

// ViewURL is the address of the live view of the conversation's browser (Steel's session viewer), "" when it has no browser. The
// address carries the provider's access, so it is only shown to people who may open the conversation.
func (m *Manager) ViewURL(ctx context.Context, threadID string) (string, map[string]string) {
	if m == nil {
		return "", nil
	}
	m.mu.Lock()
	b := m.boxes[threadID]
	m.mu.Unlock()
	if b == nil {
		return "", nil
	}
	b.mu.Lock()
	inst := b.inst
	b.mu.Unlock()
	ports := sandbox.PortsOf(inst)
	if inst == nil || ports == nil {
		return "", nil
	}
	ep, err := ports.Endpoint(ctx, ViewPort)
	if err != nil {
		return "", nil
	}
	return strings.TrimRight(ep.URL, "/") + ViewPath, ep.Headers
}

func (m *Manager) start(ctx context.Context, b *box, threadID, login string) error {
	timeout := m.StartTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	inst, err := m.Backend.Create(cctx, sandbox.CreateOptions{Profile: Profile, Metadata: map[string]string{"harness": "browser", "thread": threadID}})
	if err != nil {
		return fmt.Errorf("browser sandbox could not be created: %w", err)
	}
	fail := func(err error) error {
		m.kill(inst.ID())
		b.inst, b.server = nil, Server{}
		return err
	}
	ports := sandbox.PortsOf(inst)
	if ports == nil {
		return fail(errors.New("this sandbox provider cannot expose the browser's ports: choose another BROWSER_BACKEND"))
	}
	b.inst, b.server, b.docsSig = inst, Server{}, ""
	m.syncDocs(cctx, b, login)
	// The image's own start script; output goes to a file, so the command returns at once.
	if res, err := inst.Exec(cctx, sandbox.ExecRequest{Timeout: 30 * time.Second,
		Command: `setsid sh -c 'browser-up >/tmp/browser.log 2>&1 &' </dev/null >/dev/null 2>&1; echo started`}); err != nil || res.ExitCode != 0 {
		return fail(fmt.Errorf("the browser did not start in its sandbox (is %q the browser-sandbox image?): %v", Profile, err))
	}
	for {
		res, err := inst.Exec(cctx, sandbox.ExecRequest{Command: "test -f /tmp/browser.ready", Timeout: 10 * time.Second})
		if err == nil && res.ExitCode == 0 {
			break
		}
		select {
		case <-cctx.Done():
			return fail(fmt.Errorf("the browser was not ready after %s", timeout))
		case <-time.After(2 * time.Second):
		}
	}
	ep, err := ports.Endpoint(cctx, MCPPort)
	if err != nil {
		return fail(fmt.Errorf("the browser's address: %w", err))
	}
	b.server = Server{URL: strings.TrimRight(ep.URL, "/") + "/mcp", Headers: ep.Headers}
	slog.Info("browser sandbox started", "thread", threadID, "sandbox", inst.ID())
	return nil
}

// syncDocs puts the user's documents in the sandbox when they changed since the last time.
func (m *Manager) syncDocs(ctx context.Context, b *box, login string) {
	if m.Docs == nil || b.inst == nil {
		return
	}
	sig, err := m.Docs.Signature(login)
	if err != nil || sig == b.docsSig {
		return
	}
	if err := m.Docs.CopyTo(ctx, login, b.inst); err != nil {
		slog.Warn("copying the documents to the browser sandbox failed", "error", err)
		return
	}
	b.docsSig = sig
}

func (m *Manager) kill(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Backend.Kill(ctx, id); err != nil {
		slog.Warn("deleting a browser sandbox failed", "sandbox", id, "error", err)
	}
}

// reap deletes the sandboxes nobody used for Idle.
func (m *Manager) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.Sweep(time.Now())
		}
	}
}

// Sweep deletes the sandboxes unused since before now-Idle and returns how many.
func (m *Manager) Sweep(now time.Time) int {
	m.mu.Lock()
	var old []*box
	for id, b := range m.boxes {
		if b.mu.TryLock() {
			if b.inst != nil && now.Sub(b.lastUsed) > m.idle() {
				old = append(old, b)
				delete(m.boxes, id)
			}
			b.mu.Unlock()
		}
	}
	m.mu.Unlock()
	for _, b := range old {
		b.mu.Lock()
		if b.inst != nil {
			m.kill(b.inst.ID())
			b.inst = nil
		}
		b.mu.Unlock()
	}
	return len(old)
}

// Close deletes every browser sandbox and stops the sweeping.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	m.Sweep(time.Now().Add(1000 * time.Hour))
}
