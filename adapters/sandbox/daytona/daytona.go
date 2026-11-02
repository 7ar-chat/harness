// Package daytona is a sandbox.Backend on Daytona (https://www.daytona.io), through the official Go SDK
// (github.com/daytonaio/daytona/libs/sdk-go). SANDBOX_BACKEND=daytona selects it (DAYTONA_API_KEY, optional
// DAYTONA_API_URL, DAYTONA_TARGET, DAYTONA_SNAPSHOT); importing the package registers it.
//
// Mapping. A harness sandbox is a Daytona sandbox started from Options.Snapshot (the account default when empty); the
// snapshot must satisfy docs/sandbox-adapters.md section 3 (bash, GNU coreutils, setsid, base64, python3...). Metadata
// becomes labels. Commands run as the snapshot's user in /workspace, which Create makes and chowns to that user (with
// sudo -n when the user cannot create it). Create sets an auto-stop interval from the idle lifetime, Renew moves it,
// and Connect restarts a sandbox Daytona stopped or archived meanwhile (its files survive), so "stopped" is never
// reported as gone. Gone means a 404, destroyed/destroying/build_failed, or an unrecoverable error state.
//
// Exec. Daytona's one-shot command API returns the combined output only when the command ends. To stream, Exec uses a
// Daytona session per call: it starts the command asynchronously in the session, follows the session log WebSocket
// (stdout and stderr separate, in order within each) and polls the command for its exit code. The command runs as
// `setsid -w bash -c ...` in its own process group whose id is written to a file under /tmp; a timeout or a cancelled
// context kills the whole group with that id. Env values and the command travel base64 or single-quoted inside the
// script, never re-parsed. If the log stream cannot be opened and nothing was delivered, the output is fetched after
// the command ends and delivered at once, on OnStdout only when Daytona gives it combined. Every Exec costs several
// API calls (session create, start, polls, delete); that is the price of streaming.
//
// Files. The toolbox file API has no ranged read: ReadFile downloads the file and cuts it to maxBytes, so the
// full size is exact but a huge file is read whole. WriteFile makes the parents with mkdir -p and uploads (the bytes go
// through a temporary file inside the SDK). MkdirAll is mkdir -p. List walks directory by directory.
//
// Egress. Daytona can express an allow-list (CIDRs and domains) or block-all, not a deny-list, so: with
// Options.EgressAllowlist, CIDR entries become the sandbox's network allow list and the others its domain allow list
// (deny by default, at most 10 CIDRs); without one, Daytona's own network policy for the account tier applies and
// Options.EgressDenyCIDRs cannot be enforced (a warning is logged once per backend). Daytona's allow-lists were not
// verified against a live account in this adapter's tests. Credential brokering (sandbox.CredentialBroker) is not
// implemented.
//
// Limits: a Create that fails after Daytona accepted it (for example, the sandbox never starts in time) can leave a
// sandbox the adapter has no id for; Daytona's own auto-delete reaps it. Renew does not move the auto-delete interval,
// which counts from the moment a sandbox stops.
package daytona

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
)

// Options configures the Daytona backend.
type Options struct {
	APIKey string
	// URL is the Daytona API ("" means https://app.daytona.io/api).
	URL string
	// Target is the region or runner target ("" means the account default).
	Target string
	// Snapshot is the snapshot sandboxes start from ("" means the account default).
	Snapshot string
	// Images maps a sandbox profile to the snapshot it starts from; a profile without an entry uses Snapshot.
	Images map[string]string
	// TimeoutSeconds is the idle lifetime (auto-stop) of a sandbox; 0 means 1800.
	TimeoutSeconds int
	// AutoDeleteMinutes deletes a stopped sandbox after that long; 0 means one day, negative never.
	AutoDeleteMinutes int
	// CreateTimeout bounds Create including the start of the sandbox; 0 means 3 minutes.
	CreateTimeout time.Duration
	// EgressAllowlist, when set, makes egress deny-by-default with these CIDRs and domains allowed.
	EgressAllowlist []string
	// EgressDenyCIDRs are denied in unrestricted mode; Daytona cannot express a deny-list (see the package doc).
	EgressDenyCIDRs []string
}

// Backend is the sandbox.Backend for Daytona.
type Backend struct {
	opts Options
	api  api

	warnOnce       sync.Once
	restrictedOnce sync.Once
	// drainGrace is how long Exec waits for the log stream after the command's exit code is known.
	drainGrace time.Duration
	pollEvery  time.Duration
}

var _ sandbox.Backend = (*Backend)(nil)

// New builds the backend. It does not call Daytona.
func New(o Options) (*Backend, error) {
	if o.APIKey == "" {
		return nil, errors.New("daytona: an API key is required")
	}
	a, err := newSDKAPI(o)
	if err != nil {
		return nil, err
	}
	return newBackend(o, a), nil
}

func newBackend(o Options, a api) *Backend {
	if o.TimeoutSeconds <= 0 {
		o.TimeoutSeconds = 1800
	}
	if o.AutoDeleteMinutes == 0 {
		o.AutoDeleteMinutes = 24 * 60
	}
	if o.CreateTimeout <= 0 {
		o.CreateTimeout = 3 * time.Minute
	}
	return &Backend{opts: o, api: a, drainGrace: 3 * time.Second, pollEvery: 150 * time.Millisecond}
}

// snapshotFor is the snapshot of a profile: its entry of Options.Images, else Options.Snapshot ("" is the account default).
func (b *Backend) snapshotFor(profile string) string {
	if s := backends.ImageFor(b.opts.Images, profile); s != "" {
		return s
	}
	return b.opts.Snapshot
}

// maxAllowCIDRs is Daytona's cap on a sandbox's network allow list.
const maxAllowCIDRs = 10

// createParams builds the creation parameters: labels, idle lifetime and the egress policy.
func (b *Backend) createParams(co sandbox.CreateOptions) (createParams, error) {
	p := createParams{
		Snapshot:        b.snapshotFor(co.Profile),
		AutoStopMinutes: minutes(time.Duration(b.opts.TimeoutSeconds) * time.Second),
	}
	if len(co.Metadata) > 0 {
		p.Labels = make(map[string]string, len(co.Metadata))
		for k, v := range co.Metadata {
			p.Labels[k] = v
		}
	}
	if b.opts.AutoDeleteMinutes > 0 {
		m := b.opts.AutoDeleteMinutes
		p.AutoDeleteMin = &m
	}
	var cidrs, domains []string
	for _, e := range b.opts.EgressAllowlist {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if pf, err := netip.ParsePrefix(e); err == nil {
			cidrs = append(cidrs, pf.String())
		} else if a, err := netip.ParseAddr(e); err == nil {
			cidrs = append(cidrs, netip.PrefixFrom(a, a.BitLen()).String())
		} else {
			domains = append(domains, e)
		}
	}
	if len(cidrs) > maxAllowCIDRs {
		return p, fmt.Errorf("daytona allows at most %d CIDRs in a network allow list, got %d", maxAllowCIDRs, len(cidrs))
	}
	if len(cidrs) > 0 && len(domains) > 0 {
		return p, errors.New("daytona takes either CIDRs or domains in a sandbox's egress allow list, not both: split SANDBOX_EGRESS_ALLOWLIST")
	}
	p.NetworkAllowList = strings.Join(cidrs, ",")
	p.DomainAllowList = strings.Join(domains, ",")
	if len(cidrs)+len(domains) == 0 {
		b.warnOnce.Do(func() {
			slog.Warn("daytona: no egress allow-list; Daytona cannot deny private ranges, the account's network policy applies",
				"deny_cidrs_ignored", len(b.opts.EgressDenyCIDRs))
		})
	}
	return p, nil
}

// minutes rounds d up to whole minutes, at least 1 (0 would disable Daytona's auto-stop).
func minutes(d time.Duration) int {
	m := int((d + time.Minute - 1) / time.Minute)
	if m < 1 {
		m = 1
	}
	return m
}

// workspaceScript makes /workspace and hands it to the user commands run as.
const workspaceScript = `set -e
d=` + sandbox.Workspace + `
[ -d "$d" ] || mkdir -p "$d" 2>/dev/null || sudo -n mkdir -p "$d"
[ "$(stat -c %u "$d")" = "$(id -u)" ] || sudo -n chown "$(id -u):$(id -g)" "$d"
[ -w "$d" ]`

// Create starts a sandbox and prepares /workspace.
func (b *Backend) Create(ctx context.Context, co sandbox.CreateOptions) (sandbox.Instance, error) {
	p, err := b.createParams(co)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, b.opts.CreateTimeout)
	defer cancel()
	bx, err := b.api.Create(cctx, p)
	if err != nil && restrictedNetwork(err) && (p.NetworkAllowList != "" || p.DomainAllowList != "") {
		// Tiers 1 and 2: the organisation's network policy wins and refuses a sandbox-level list. That policy is itself
		// restrictive (Daytona's essential-services list), so create the sandbox under it, and say so.
		b.restrictedOnce.Do(func() {
			slog.Warn("daytona: the account's network tier refuses a per-sandbox egress allow-list; sandboxes run under Daytona's "+
				"organisation policy instead (tier 3 or 4 honours SANDBOX_EGRESS_ALLOWLIST)", "allowlist_entries", len(b.opts.EgressAllowlist))
		})
		p.NetworkAllowList, p.DomainAllowList = "", ""
		bx, err = b.api.Create(cctx, p)
	}
	if err != nil {
		return nil, fmt.Errorf("create daytona sandbox: %w", wrap(err))
	}
	inst := b.instance(bx)
	exit, out, err := bx.Run(cctx, workspaceScript, "/", nil)
	if err == nil && exit != 0 {
		err = fmt.Errorf("prepare %s: exit %d: %s", sandbox.Workspace, exit, strings.TrimSpace(out))
	}
	if err != nil {
		_ = b.Kill(context.WithoutCancel(ctx), bx.ID())
		return nil, fmt.Errorf("create daytona sandbox: %w", wrap(err))
	}
	return inst, nil
}

// Connect reattaches to a sandbox. ErrNoSandbox only when Daytona says it is gone for good.
func (b *Backend) Connect(ctx context.Context, id string) (sandbox.Instance, error) {
	bx, err := b.api.Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s: %w", sandbox.ErrNoSandbox, id, err)
		}
		return nil, fmt.Errorf("connect daytona sandbox: %w", wrap(unavailable(err)))
	}
	inst := b.instance(bx)
	switch lifecycle(bx.Info()) {
	case stateGone:
		return nil, fmt.Errorf("%w: %s is %s", sandbox.ErrNoSandbox, id, bx.Info().State)
	case stateStopped:
		if err := bx.Start(ctx); err != nil {
			if isNotFound(err) {
				return nil, fmt.Errorf("%w: %s: %w", sandbox.ErrNoSandbox, id, err)
			}
			return nil, fmt.Errorf("start daytona sandbox: %w", wrap(unavailable(err)))
		}
	}
	return inst, nil
}

// unavailable makes sure a non-classified error (for instance an authentication failure during a blip) is still
// transient for Connect, whose only definitive answer is "not found".
func unavailable(err error) error {
	if errors.Is(err, sandbox.ErrQuota) || errors.Is(err, sandbox.ErrUnavailable) || errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w: %w", sandbox.ErrUnavailable, err)
}

// Kill deletes the sandbox; an unknown or already deleted one is not an error.
func (b *Backend) Kill(ctx context.Context, id string) error {
	bx, err := b.api.Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("kill daytona sandbox: %w", wrap(err))
	}
	if lifecycle(bx.Info()) == stateGone && bx.Info().State != "error" {
		return nil
	}
	if err := bx.Delete(ctx); err != nil && !isNotFound(err) {
		return fmt.Errorf("kill daytona sandbox: %w", wrap(err))
	}
	return nil
}

type lifeState int

const (
	stateUp lifeState = iota
	stateStopped
	stateGone
)

// lifecycle classifies a Daytona state. Only destroyed, destroying, build_failed and an unrecoverable error are
// final; stopped and archived come back with a start.
func lifecycle(i boxInfo) lifeState {
	switch i.State {
	case "destroyed", "destroying", "build_failed":
		return stateGone
	case "error":
		if !i.Recoverable {
			return stateGone
		}
		return stateUp
	case "stopped", "archived", "stopping", "archiving":
		return stateStopped
	}
	return stateUp
}

func (b *Backend) instance(bx box) *instance { return &instance{b: b, box: bx} }

// instance is a Daytona sandbox. Safe for concurrent use.
type instance struct {
	b   *Backend
	box box

	mu      sync.Mutex // the SDK sandbox object is refreshed in place
	cleanup sync.WaitGroup
}

var _ sandbox.Instance = (*instance)(nil)

func (i *instance) ID() string { return i.box.ID() }

// refresh reloads the state from Daytona.
func (i *instance) refresh(ctx context.Context) (boxInfo, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.box.Refresh(ctx); err != nil {
		return boxInfo{}, err
	}
	return i.box.Info(), nil
}

// Healthy: the sandbox is up (a stopped one is started) and runs `true`.
func (i *instance) Healthy(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	info, err := i.refresh(ctx)
	if err != nil {
		return false
	}
	switch lifecycle(info) {
	case stateGone:
		return false
	case stateStopped:
		if info.State == "stopping" || info.State == "archiving" || i.box.Start(ctx) != nil {
			return false
		}
	}
	exit, _, err := i.box.Run(ctx, "true", "/", nil)
	return err == nil && exit == 0 && ctx.Err() == nil
}

// Gone is true only for a 404 or a final state; any failure to find out is "not gone".
func (i *instance) Gone(ctx context.Context) bool {
	info, err := i.refresh(ctx)
	if err != nil {
		return isNotFound(err)
	}
	return lifecycle(info) == stateGone
}

// Renew moves the auto-stop interval to d from now.
func (i *instance) Renew(ctx context.Context, d time.Duration) error {
	if err := i.b.api.SetAutoStop(ctx, i.ID(), minutes(d)); err != nil {
		return i.opErr(ctx, err, false)
	}
	return nil
}

// opErr classifies an error of an operation on this sandbox: a 404 is the sandbox gone when Daytona confirms it
// (ErrGone), else a missing file when file is set; quota and transient failures get their sentinels.
func (i *instance) opErr(ctx context.Context, err error, file bool) error {
	switch {
	case err == nil:
		return nil
	case sandbox.IsGone(err):
		return err
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		return err
	case isNotFound(err):
		gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if i.Gone(gctx) {
			return fmt.Errorf("%w: %w", sandbox.ErrGone, err)
		}
		if file {
			return fmt.Errorf("%w: %w", sandbox.ErrNotFound, err)
		}
	}
	return wrap(err)
}

// ---- Exec ----

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func validEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for n, r := range k {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || n > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// execScript is what the Daytona session runs: a new session and process group (setsid -w keeps the exit code) whose
// leader records its pid, exports Env, enters Cwd and becomes `bash -c <command>`. The command travels base64.
func execScript(req sandbox.ExecRequest, pidfile string) (string, error) {
	var in strings.Builder
	fmt.Fprintf(&in, "echo $$ > %s\n", shq(pidfile))
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		if !validEnvKey(k) {
			return "", fmt.Errorf("invalid environment variable name %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&in, "export %s=%s\n", k, shq(req.Env[k]))
	}
	if req.Cwd != "" {
		fmt.Fprintf(&in, "cd %s || exit 126\n", shq(req.Cwd))
	}
	fmt.Fprintf(&in, "exec bash -c \"$(printf %%s %s | base64 -d)\"\n", shq(base64.StdEncoding.EncodeToString([]byte(req.Command))))
	return "setsid -w bash -c " + shq(in.String()), nil
}

// killScript kills the process group recorded in pidfile (waiting briefly for it to be written).
func killScript(pidfile string) string {
	f := shq(pidfile)
	return `for i in 1 2 3 4 5 6 7 8 9 10; do [ -s ` + f + ` ] && break; sleep 0.2; done
p=$(cat ` + f + ` 2>/dev/null)
if [ -n "$p" ]; then kill -KILL -- "-$p" 2>/dev/null; kill -KILL "$p" 2>/dev/null; fi
rm -f ` + f + `
exit 0`
}

func randID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sink serialises the callbacks and stops them for good once Exec returns.
type sink struct {
	mu        sync.Mutex
	closed    bool
	delivered int
	out, err  func(string)
}

func (s *sink) put(f func(string), text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || text == "" {
		return
	}
	s.delivered += len(text)
	if f != nil {
		f(text)
	}
}

func (s *sink) stdout(t string) { s.put(s.out, t) }
func (s *sink) stderr(t string) { s.put(s.err, t) }
func (s *sink) count() int      { s.mu.Lock(); defer s.mu.Unlock(); return s.delivered }
func (s *sink) close()          { s.mu.Lock(); s.closed = true; s.mu.Unlock() }

// Exec runs the command with bash and streams its output (see the package doc). A non-zero exit is a result. Timeout
// and a cancelled ctx kill the process group; Exec then returns an error wrapping context.DeadlineExceeded or ctx's.
func (i *instance) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	id := randID()
	session, pidfile := "hx-"+id, "/tmp/.hx-"+id+".pid"
	script, err := execScript(req, pidfile)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	parent := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	if err := i.box.CreateSession(ctx, session); err != nil {
		return sandbox.ExecResult{}, i.execErr(parent, ctx, req, "", pidfile, err)
	}
	defer i.cleanupAsync(session, pidfile)
	cmdID, err := i.box.StartCommand(ctx, session, script)
	if err != nil {
		return sandbox.ExecResult{}, i.execErr(parent, ctx, req, "", pidfile, err)
	}

	sk := &sink{out: req.OnStdout, err: req.OnStderr}
	defer sk.close()
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	outc, errc := make(chan string), make(chan string)
	streamDone := make(chan error, 1)
	go func() { streamDone <- i.box.StreamLogs(streamCtx, session, cmdID, outc, errc) }()
	drained := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for t := range outc {
				sk.stdout(t)
			}
		}()
		go func() {
			defer wg.Done()
			for t := range errc {
				sk.stderr(t)
			}
		}()
		wg.Wait()
		close(drained)
	}()

	exit, err := i.waitExit(ctx, session, cmdID)
	if err != nil {
		return sandbox.ExecResult{}, i.execErr(parent, ctx, req, cmdID, pidfile, err)
	}
	select {
	case <-drained:
	case <-time.After(i.b.drainGrace):
	}
	var streamErr error
	select {
	case streamErr = <-streamDone:
	default:
	}
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) && sk.count() == 0 {
		// The live log could not be followed: deliver what Daytona kept.
		if so, se, err := i.box.CommandLogs(ctx, session, cmdID); err == nil {
			sk.stdout(so)
			sk.stderr(se)
			streamErr = nil
		}
	}
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		return sandbox.ExecResult{}, fmt.Errorf("%w: output stream interrupted: %w", sandbox.ErrUnavailable, streamErr)
	}
	return sandbox.ExecResult{ExitCode: exit}, nil
}

// waitExit polls the command until it has an exit code. A few consecutive transient failures are tolerated.
func (i *instance) waitExit(ctx context.Context, session, cmdID string) (int, error) {
	delay := i.b.pollEvery
	fails := 0
	for {
		exit, done, err := i.box.CommandExit(ctx, session, cmdID)
		switch {
		case err == nil && done:
			return exit, nil
		case err == nil:
			fails = 0
		case ctx.Err() != nil:
			return 0, ctx.Err()
		case isNotFound(err) || !isTransient(err) || fails >= 4:
			return 0, err
		default:
			fails++
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 4*i.b.pollEvery {
			delay += i.b.pollEvery / 2
		}
	}
}

// execErr turns a failure of Exec into the error it returns; a timeout or cancellation first kills the group.
func (i *instance) execErr(parent, ctx context.Context, req sandbox.ExecRequest, cmdID, pidfile string, err error) error {
	if ctx.Err() == nil {
		return i.opErr(ctx, err, false)
	}
	kctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 20*time.Second)
	defer cancel()
	if cmdID != "" {
		_, _, _ = i.box.Run(kctx, killScript(pidfile), "/", nil)
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	return fmt.Errorf("command timed out after %s: %w", req.Timeout, context.DeadlineExceeded)
}

// cleanupAsync deletes the session and the pid file without delaying the caller.
func (i *instance) cleanupAsync(session, pidfile string) {
	i.cleanup.Add(1)
	go func() {
		defer i.cleanup.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = i.box.DeleteSession(ctx, session)
		_, _, _ = i.box.Run(ctx, "rm -f "+shq(pidfile), "/", nil)
	}()
}

// ---- files ----

func absPath(p string) (string, error) {
	if !path.IsAbs(p) {
		return "", fmt.Errorf("path %q is not absolute", p)
	}
	return path.Clean(p), nil
}

// fileErr is opErr for the file API. Daytona's toolbox does not always answer 404 for a missing path, so a server or
// validation error is double-checked with `test -e`: a path that does not exist is ErrNotFound.
func (i *instance) fileErr(ctx context.Context, p string, err error) error {
	e := i.opErr(ctx, err, true)
	if e == nil || errors.Is(e, sandbox.ErrNotFound) || errors.Is(e, sandbox.ErrGone) || ctx.Err() != nil {
		return e
	}
	if st := statusOf(err); st == 400 || st == 500 {
		if exit, _, rerr := i.box.Run(ctx, "test -e "+shq(p), "/", nil); rerr == nil && exit != 0 {
			return fmt.Errorf("%w: %w", sandbox.ErrNotFound, err)
		}
	}
	return e
}

func (i *instance) ReadFile(ctx context.Context, p string, maxBytes int64) ([]byte, int64, error) {
	p, err := absPath(p)
	if err != nil {
		return nil, 0, err
	}
	data, err := i.box.Download(ctx, p)
	if err != nil {
		// Daytona's download fails on an empty file ("Failed to read file"): a regular file of size 0 is just empty.
		if fe, serr := i.box.FileInfo(ctx, p); serr == nil && !fe.IsDir && fe.Size == 0 {
			return []byte{}, 0, nil
		}
		return nil, 0, i.fileErr(ctx, p, err)
	}
	size := int64(len(data))
	if maxBytes > 0 && size > maxBytes {
		data = data[:maxBytes]
	}
	return data, size, nil
}

func (i *instance) WriteFile(ctx context.Context, p string, data []byte) error {
	p, err := absPath(p)
	if err != nil {
		return err
	}
	if err := i.MkdirAll(ctx, path.Dir(p)); err != nil {
		return err
	}
	if len(data) == 0 {
		// An empty upload carries no file part and Daytona rejects it ("no such file"): create or truncate in place.
		exit, out, err := i.box.Run(ctx, ": > "+shq(p), "/", nil)
		if err != nil {
			return i.opErr(ctx, err, false)
		}
		if exit != 0 {
			return fmt.Errorf("write %s: exit %d: %s", p, exit, strings.TrimSpace(out))
		}
		return nil
	}
	if err := i.box.Upload(ctx, p, data); err != nil {
		return i.opErr(ctx, err, false)
	}
	return nil
}

func (i *instance) MkdirAll(ctx context.Context, p string) error {
	p, err := absPath(p)
	if err != nil {
		return err
	}
	exit, out, err := i.box.Run(ctx, "mkdir -p -- "+shq(p), "/", nil)
	if err != nil {
		return i.opErr(ctx, err, false)
	}
	if exit != 0 {
		return fmt.Errorf("mkdir %s: exit %d: %s", p, exit, strings.TrimSpace(out))
	}
	return nil
}

func info(p string, f fileEntry) sandbox.FileInfo {
	t := "file"
	if f.IsDir {
		t = "directory"
	}
	return sandbox.FileInfo{Path: p, Type: t, Size: f.Size, ModifiedAt: f.MTime}
}

func (i *instance) Stat(ctx context.Context, p string) (sandbox.FileInfo, error) {
	p, err := absPath(p)
	if err != nil {
		return sandbox.FileInfo{}, err
	}
	f, err := i.box.FileInfo(ctx, p)
	if err != nil {
		return sandbox.FileInfo{}, i.fileErr(ctx, p, err)
	}
	return info(p, f), nil
}

// List returns the entries below p down to depth (1 = children), breadth first.
func (i *instance) List(ctx context.Context, p string, depth int) ([]sandbox.FileInfo, error) {
	p, err := absPath(p)
	if err != nil {
		return nil, err
	}
	if depth < 1 {
		depth = 1
	}
	var out []sandbox.FileInfo
	level := []string{p}
	for d := 1; d <= depth && len(level) > 0; d++ {
		var next []string
		for _, dir := range level {
			entries, err := i.box.ListFiles(ctx, dir)
			if err != nil {
				if dir != p && errors.Is(i.fileErr(ctx, dir, err), sandbox.ErrNotFound) {
					continue // removed while we walked
				}
				return nil, i.fileErr(ctx, dir, err)
			}
			for _, f := range entries {
				fp := path.Join(dir, f.Name)
				out = append(out, info(fp, f))
				if f.IsDir {
					next = append(next, fp)
				}
			}
		}
		level = next
	}
	return out, nil
}

// restrictedNetwork reports Daytona's refusal of a sandbox-level network setting on an account whose tier restricts
// the network ("Network access is restricted and cannot be overridden at the sandbox level").
func restrictedNetwork(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "cannot be overridden at the sandbox level") || strings.Contains(m, "network access is restricted")
}
