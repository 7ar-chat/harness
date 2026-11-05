package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/7ar-chat/harness/core/sandbox"
)

// instance is one E2B sandbox, driven through its envd agent.
type instance struct {
	b     *E2B
	id    string
	token string // envd access token, "" when the sandbox is not secured
	envd  string // base URL of the envd agent
	// traffic is the access token of the sandbox's ports ("" when the sandbox allows public traffic); domain is the
	// E2B domain its ports are served under.
	traffic string
	domain  string
	root  bool   // run commands as root (the creation bootstrap only)
}

var _ sandbox.Instance = (*instance)(nil)

func (s *instance) ID() string { return s.id }

// envdUser is the user commands and files run as (E2B's default sandbox user).
const envdUser = "user"

// envdError is a non-2xx answer of the envd agent or of E2B's edge proxy in front of it.
type envdError struct {
	Status  int
	Code    string // Connect error code: "not_found", "invalid_argument", ...
	Message string
}

func (e *envdError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("e2b sandbox: HTTP %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("e2b sandbox: HTTP %d: %s", e.Status, e.Message)
}

// readEnvdError builds the error of a non-2xx response; the body is consumed.
func readEnvdError(resp *http.Response) *envdError {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &envdError{Status: resp.StatusCode}
	var body struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) == nil {
		e.Message = body.Message
		if c, ok := body.Code.(string); ok {
			e.Code = c
		}
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(data))
		if len(e.Message) > 200 {
			e.Message = e.Message[:200]
		}
	}
	return e
}

// request builds a request to the envd agent with its routing and authentication headers.
func (s *instance) request(ctx context.Context, method, p string, q url.Values, body io.Reader) (*http.Request, error) {
	u := s.envd + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("E2b-Sandbox-Id", s.id)
	req.Header.Set("E2b-Sandbox-Port", strconv.Itoa(envdPort))
	user := envdUser
	if s.root {
		user = "root"
	}
	req.SetBasicAuth(user, "")
	if s.token != "" {
		req.Header.Set("X-Access-Token", s.token)
	}
	return req, nil
}

// opErr classifies a failed operation. An answer of E2B's proxy that the sandbox does not exist (502) is confirmed
// with the control plane before the sandbox is declared gone; a missing file (file ops) is sandbox.ErrNotFound;
// anything else that is not the caller's fault is transient (sandbox.ErrUnavailable).
func (s *instance) opErr(ctx context.Context, err error, file bool) error {
	if err == nil || errors.Is(err, sandbox.ErrNotFound) || sandbox.IsGone(err) {
		return err
	}
	var ee *envdError
	if !errors.As(err, &ee) {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w: %w", sandbox.ErrUnavailable, err)
	}
	switch {
	case file && (ee.Code == "not_found" || (ee.Status == http.StatusNotFound && ee.Code == "")):
		return sandbox.ErrNotFound
	case ee.Status == http.StatusBadGateway, ee.Status == http.StatusServiceUnavailable, ee.Status == http.StatusGatewayTimeout:
		gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if s.Gone(gctx) {
			return fmt.Errorf("%w: %w", sandbox.ErrGone, err)
		}
		return fmt.Errorf("%w: %w", sandbox.ErrUnavailable, err)
	}
	return err
}

// rpc calls a unary Connect RPC with a JSON body and decodes the JSON answer into out.
func (s *instance) rpc(ctx context.Context, method string, in, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := s.request(ctx, http.MethodPost, method, nil, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := s.b.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return readEnvdError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *instance) Healthy(ctx context.Context) bool {
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := s.request(hctx, http.MethodGet, "/health", nil, nil)
	if err != nil {
		return false
	}
	resp, err := s.b.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode/100 == 2
}

// Gone is true only when the control plane says the sandbox does not exist; a paused or unreachable one is not gone.
func (s *instance) Gone(ctx context.Context) bool {
	err := s.b.api(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(s.id), nil, nil)
	return errors.Is(err, errNotFound)
}

func (s *instance) Renew(ctx context.Context, d time.Duration) error {
	secs := max(int(d.Seconds()), 60)
	err := s.b.api(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(s.id)+"/timeout", map[string]any{"timeout": secs}, nil)
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("%w: renew", sandbox.ErrGone)
	}
	return err
}

// ---- commands ----

type processConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Envs map[string]string `json:"envs,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
}

// run starts a process and streams its output; it returns the exit code. A cancelled ctx or an exceeded timeout kills
// the process tree and returns the context's error.
func (s *instance) run(ctx context.Context, pc processConfig, timeout time.Duration, onOut, onErr func(string)) (int, error) {
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	body, err := json.Marshal(map[string]any{"process": pc, "stdin": false})
	if err != nil {
		return 0, err
	}
	req, err := s.request(runCtx, http.MethodPost, "/process.Process/Start", nil, bytes.NewReader(connectFrame(0, body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := s.b.hc.Do(req)
	if err != nil {
		return 0, s.ctxErr(ctx, runCtx, timeout, 0, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, s.opErr(ctx, readEnvdError(resp), false)
	}

	var (
		pid        uint32
		outS, errS utf8Carry
		exit       = -1
		ended      bool
	)
	emit := func(f func(string), c *utf8Carry, b []byte) {
		if f != nil {
			if t := c.feed(b); t != "" {
				f(t)
			}
		}
	}
	for {
		flags, msg, rerr := readFrame(resp.Body)
		if rerr != nil {
			if ended {
				break
			}
			return 0, s.ctxErr(ctx, runCtx, timeout, pid, rerr)
		}
		if flags&flagEndStream != 0 {
			var end struct {
				Error *struct{ Code, Message string } `json:"error"`
			}
			if json.Unmarshal(msg, &end) == nil && end.Error != nil {
				return 0, s.opErr(ctx, &envdError{Status: http.StatusOK, Code: end.Error.Code, Message: end.Error.Message}, false)
			}
			break
		}
		var ev struct {
			Event struct {
				Start *struct{ Pid uint32 } `json:"start"`
				Data  *struct {
					Stdout []byte `json:"stdout"`
					Stderr []byte `json:"stderr"`
				} `json:"data"`
				End *struct {
					ExitCode int  `json:"exitCode"`
					Exited   bool `json:"exited"`
				} `json:"end"`
			} `json:"event"`
		}
		if err := json.Unmarshal(msg, &ev); err != nil {
			return 0, fmt.Errorf("e2b sandbox: unreadable process event: %w", err)
		}
		switch {
		case ev.Event.Start != nil:
			pid = ev.Event.Start.Pid
		case ev.Event.Data != nil:
			if len(ev.Event.Data.Stdout) > 0 {
				emit(onOut, &outS, ev.Event.Data.Stdout)
			}
			if len(ev.Event.Data.Stderr) > 0 {
				emit(onErr, &errS, ev.Event.Data.Stderr)
			}
		case ev.Event.End != nil:
			exit, ended = ev.Event.End.ExitCode, true
		}
	}
	if onOut != nil {
		if t := outS.flush(); t != "" {
			onOut(t)
		}
	}
	if onErr != nil {
		if t := errS.flush(); t != "" {
			onErr(t)
		}
	}
	if !ended {
		return 0, fmt.Errorf("%w: the command's result is unknown (the stream ended without an exit status)", sandbox.ErrUnavailable)
	}
	return exit, nil
}

// ctxErr turns a transport failure into the right error: the caller's cancellation, the command's timeout (both kill
// the process tree), or a transient provider error.
func (s *instance) ctxErr(ctx, runCtx context.Context, timeout time.Duration, pid uint32, err error) error {
	switch {
	case ctx.Err() != nil:
		s.killTree(pid)
		return ctx.Err()
	case runCtx.Err() != nil:
		s.killTree(pid)
		return fmt.Errorf("command timed out after %s: %w", timeout, context.DeadlineExceeded)
	}
	return s.opErr(ctx, err, false)
}

// killTree kills a process and its descendants, then the process itself (best effort, in the background).
func (s *instance) killTree(pid uint32) {
	if pid == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		script := `kill_tree() { for c in $(pgrep -P "$1" 2>/dev/null); do kill_tree "$c"; done; kill -KILL "$1" 2>/dev/null; }; kill_tree "$1"`
		_, _ = s.run(ctx, processConfig{Cmd: "/bin/bash", Args: []string{"-c", script, "kill_tree", strconv.FormatUint(uint64(pid), 10)}}, 0, nil, nil)
		_ = s.rpc(ctx, "/process.Process/SendSignal", map[string]any{"process": map[string]any{"pid": pid}, "signal": "SIGNAL_SIGKILL"}, nil)
	}()
}

func (s *instance) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	code, err := s.run(ctx, processConfig{Cmd: "/bin/bash", Args: []string{"-c", req.Command}, Envs: req.Env, Cwd: req.Cwd}, req.Timeout, req.OnStdout, req.OnStderr)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	return sandbox.ExecResult{ExitCode: code}, nil
}

// bootstrap makes /workspace exist and belong to the command user: the templates ship no such directory, and the
// default user cannot create one under /. It runs once, as root, when the sandbox is created.
func (s *instance) bootstrap(ctx context.Context) error {
	rs := *s
	rs.root = true
	script := `mkdir -p "$1" && chown "$2:$2" "$1" && chmod 755 "$1"`
	var stderr strings.Builder
	code, err := rs.run(ctx, processConfig{Cmd: "/bin/bash", Args: []string{"-c", script, "bootstrap", sandbox.Workspace, envdUser}}, time.Minute, nil, func(t string) { stderr.WriteString(t) })
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ---- files ----

func (s *instance) MkdirAll(ctx context.Context, p string) error {
	var stderr strings.Builder
	code, err := s.run(ctx, processConfig{Cmd: "/bin/mkdir", Args: []string{"-p", "--", p}}, time.Minute, nil, func(t string) { stderr.WriteString(t) })
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("mkdir %s: %s", p, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s *instance) WriteFile(ctx context.Context, p string, data []byte) error {
	return s.WriteFileFrom(ctx, p, bytes.NewReader(data))
}

// WriteFileFrom streams r into the file (sandbox.WriteFrom uses it for large uploads).
func (s *instance) WriteFileFrom(ctx context.Context, p string, r io.Reader) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, path.Base(p)))
		h.Set("Content-Type", "application/octet-stream")
		part, err := mw.CreatePart(h)
		if err == nil {
			_, err = io.Copy(part, r)
		}
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	req, err := s.request(ctx, http.MethodPost, "/files", url.Values{"path": {p}, "username": {envdUser}}, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := s.b.hc.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return s.opErr(ctx, err, false)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return s.opErr(ctx, readEnvdError(resp), false)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

func (s *instance) ReadFile(ctx context.Context, p string, maxBytes int64) ([]byte, int64, error) {
	info, err := s.Stat(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	if info.Type == "directory" {
		return nil, 0, fmt.Errorf("%s is a directory", p)
	}
	req, err := s.request(ctx, http.MethodGet, "/files", url.Values{"path": {p}, "username": {envdUser}}, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.b.hc.Do(req)
	if err != nil {
		return nil, 0, s.opErr(ctx, err, true)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, 0, s.opErr(ctx, readEnvdError(resp), true)
	}
	var rd io.Reader = resp.Body
	if maxBytes > 0 {
		rd = io.LimitReader(resp.Body, maxBytes)
	}
	data, err := io.ReadAll(rd)
	if err != nil {
		return nil, 0, s.opErr(ctx, err, true)
	}
	return data, max(info.Size, int64(len(data))), nil
}

// entry is the EntryInfo of E2B's filesystem service; 64-bit integers are JSON strings.
type entry struct {
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Path         string          `json:"path"`
	Size         json.RawMessage `json:"size"`
	ModifiedTime time.Time       `json:"modifiedTime"`
}

func (e entry) info() sandbox.FileInfo {
	t := "file"
	if e.Type == "FILE_TYPE_DIRECTORY" {
		t = "directory"
	}
	size, _ := strconv.ParseInt(strings.Trim(string(e.Size), `"`), 10, 64)
	return sandbox.FileInfo{Path: e.Path, Type: t, Size: size, ModifiedAt: e.ModifiedTime}
}

func (s *instance) Stat(ctx context.Context, p string) (sandbox.FileInfo, error) {
	var out struct {
		Entry entry `json:"entry"`
	}
	if err := s.rpc(ctx, "/filesystem.Filesystem/Stat", map[string]any{"path": p}, &out); err != nil {
		return sandbox.FileInfo{}, s.opErr(ctx, err, true)
	}
	fi := out.Entry.info()
	if fi.Path == "" {
		fi.Path = p
	}
	return fi, nil
}

func (s *instance) List(ctx context.Context, p string, depth int) ([]sandbox.FileInfo, error) {
	var out struct {
		Entries []entry `json:"entries"`
	}
	if err := s.rpc(ctx, "/filesystem.Filesystem/ListDir", map[string]any{"path": p, "depth": max(depth, 1)}, &out); err != nil {
		return nil, s.opErr(ctx, err, true)
	}
	infos := make([]sandbox.FileInfo, 0, len(out.Entries))
	for _, e := range out.Entries {
		if path.Clean(e.Path) == path.Clean(p) {
			continue
		}
		infos = append(infos, e.info())
	}
	return infos, nil
}

// ---- Connect streaming frames and UTF-8 handling ----

const flagEndStream = 0x02

// connectFrame is one envelope of a Connect streaming message: flags, big-endian length, payload.
func connectFrame(flags byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flags
	out[1], out[2], out[3], out[4] = byte(len(payload)>>24), byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	copy(out[5:], payload)
	return out
}

// maxFrame bounds one envelope, so a corrupt length cannot allocate gigabytes.
const maxFrame = 64 << 20

func readFrame(r io.Reader) (flags byte, payload []byte, err error) {
	var h [5]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := uint32(h[1])<<24 | uint32(h[2])<<16 | uint32(h[3])<<8 | uint32(h[4])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("e2b sandbox: a %d-byte stream frame exceeds the %d-byte limit", n, maxFrame)
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return h[0], payload, nil
}

// utf8Carry holds back the incomplete character at the end of an output chunk, so a multi-byte character split
// across two chunks reaches the callback whole.
type utf8Carry struct{ rest []byte }

func (c *utf8Carry) feed(b []byte) string {
	buf := append(c.rest, b...)
	cut := len(buf)
	for i := 1; i <= utf8.UTFMax-1 && i <= len(buf); i++ {
		if r, _ := utf8.DecodeRune(buf[len(buf)-i:]); r == utf8.RuneError && !utf8.FullRune(buf[len(buf)-i:]) {
			cut = len(buf) - i
			break
		}
	}
	c.rest = append([]byte(nil), buf[cut:]...)
	return string(buf[:cut])
}

func (c *utf8Carry) flush() string {
	s := string(c.rest)
	c.rest = nil
	return s
}

var _ sandbox.PortExposer = (*instance)(nil)

// Endpoint implements sandbox.PortExposer: a port of the sandbox is served at https://<port>-<id>.<domain>, and a
// sandbox that does not allow public traffic wants its traffic token in the e2b-traffic-access-token header.
func (s *instance) Endpoint(_ context.Context, port int) (sandbox.Endpoint, error) {
	if s.domain == "" {
		return sandbox.Endpoint{}, errors.New("e2b: the sandbox's domain is unknown")
	}
	ep := sandbox.Endpoint{URL: fmt.Sprintf("https://%d-%s.%s", port, s.id, s.domain)}
	if s.traffic != "" {
		ep.Headers = map[string]string{"e2b-traffic-access-token": s.traffic}
	}
	return ep, nil
}
