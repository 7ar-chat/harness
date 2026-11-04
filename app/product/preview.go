package product

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// Preview runs a project's app in the sandbox its thread works in and shows it to the people of the project.
//
//	browser ──► /preview/<thread>.<mac>/…  (this handler)  ──► the sandbox's port ──► the project's dev server
//
// The agent starts the app with preview_start, which runs the "run" command of .har/project.yaml as a detached process
// of the sandbox, so files the agent edits are served by the dev server's own reload. The address is a bearer link:
// the mac is an HMAC of the thread id, so only someone the UI gave it to (a member of the project) can form it. The
// app is code the agent wrote, so it never gets the visitor's identity: the UI shows it in a sandboxed frame (an opaque
// origin: it cannot call the product's API with the visitor's cookies), the handler answers with a sandboxing CSP of its
// own, and cookies are neither forwarded to it nor accepted from it.
type Preview struct {
	// Key signs the addresses. Product.New sets it to Options.SecretKey when empty; without any key a random one is
	// used, and the addresses stop working when the process restarts.
	Key []byte

	Team store.TeamStore
	mgr  sandbox.Manager

	keyOnce sync.Once
	cache   sync.Map // thread id -> previewTarget
}

type previewTarget struct {
	manifest Manifest
	endpoint sandbox.Endpoint
	at       time.Time
}

const previewCacheTTL = 20 * time.Second

func (pv *Preview) key() []byte {
	pv.keyOnce.Do(func() {
		if len(pv.Key) == 0 {
			pv.Key = make([]byte, 32)
			_, _ = rand.Read(pv.Key)
		}
	})
	return pv.Key
}

func (pv *Preview) mac(threadID string) string {
	m := hmac.New(sha256.New, pv.key())
	m.Write([]byte("preview\x00" + threadID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// Base is the path the preview of a thread is served under (with the trailing slash).
func (pv *Preview) Base(threadID string) string {
	return httpapi.PreviewPrefix + threadID + "." + pv.mac(threadID) + "/"
}

// threadOf checks the address and returns its thread.
func (pv *Preview) threadOf(id string) (string, bool) {
	i := strings.LastIndex(id, ".")
	if i <= 0 {
		return "", false
	}
	thread, got := id[:i], id[i+1:]
	if subtle.ConstantTimeCompare([]byte(got), []byte(pv.mac(thread))) != 1 {
		return "", false
	}
	return thread, true
}

// ---- the agent's tools ----

// Feature gives the agent of a project's thread the preview tools.
func (pv *Preview) Feature() harness.Feature {
	return harness.FeatureFunc("project-preview", func(b *harness.Builder) error {
		if b.Sandboxes == nil {
			return nil
		}
		pv.mgr = b.Sandboxes
		b.AddTools(previewTools{pv: pv})
		b.Section(func(ctx context.Context, in agent.Input) string {
			if !in.Sandbox || !pv.projectThread(ctx, in.Run.ThreadID) {
				return ""
			}
			return "# Preview\n\nTo show the user the app they are building, call preview_start: it installs the dependencies and starts the app " +
				"as described in " + ManifestPath + " (run, port, install), then the user sees it in the Preview tab of the conversation. " +
				"The app reloads by itself when you edit files, so call preview_start once and edit; call it again only after changing " +
				ManifestPath + " or the dependencies. Use preview_logs when the page is blank or an error is reported."
		})
		return nil
	})
}

func (pv *Preview) projectThread(ctx context.Context, threadID string) bool {
	if pv.Team == nil {
		return false
	}
	meta, err := pv.Team.GetThreadMeta(ctx, threadID)
	return err == nil && meta.ProjectID != ""
}

type previewTools struct{ pv *Preview }

func (previewTools) Name() string { return "preview" }

type noArgs struct{}

type previewResult struct {
	State   string `json:"state"`
	URL     string `json:"url,omitempty"`
	Port    int    `json:"port,omitempty"`
	Message string `json:"message,omitempty"`
	Log     string `json:"log,omitempty"`
}

func (p previewTools) Tools(ctx context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	pv := p.pv
	if rc.Sandbox == tools.SandboxOff || !pv.projectThread(ctx, rc.ThreadID) {
		return nil, func() {}, nil
	}
	var out []ai.BaseTool
	add := func(t ai.BaseTool, err error) {
		if err == nil {
			out = append(out, t)
		}
	}
	add(ai.InferTool("preview_start", "Start (or restart) the project's app in the sandbox so the user can see it in the Preview tab. Reads "+ManifestPath+
		" (install, run, port), installs the dependencies when they changed, starts the app and waits until it answers. Returns the state, the port and the end of the log.",
		func(ctx context.Context, _ noArgs) (any, error) { return pv.start(ctx, rc) }))
	add(ai.InferTool("preview_logs", "Show the end of the output of the previewed app (install and server). Use it when the preview is blank or broken.",
		func(ctx context.Context, _ noArgs) (any, error) { return pv.logs(ctx, rc) }))
	add(ai.InferTool("preview_stop", "Stop the previewed app.",
		func(ctx context.Context, _ noArgs) (any, error) { return pv.stop(ctx, rc) }))
	return out, func() {}, nil
}

const previewDir = sandbox.ScratchDir + "/preview"

// previewStartScript stops the previous app, installs when the dependency files changed and starts the app detached in
// its own session. It waits for the port, and fails early with the log when the process dies.
const previewStartScript = `cd "$REPO_DIR"
D="$PREVIEW_DIR"
mkdir -p "$D"
if [ -f "$D/pid" ]; then
  P=$(cat "$D/pid")
  kill -TERM -- "-$P" 2>/dev/null || { pkill -TERM -P "$P" 2>/dev/null; kill -TERM "$P" 2>/dev/null; } || true
  sleep 1
fi
STAMP=$(cat package.json package-lock.json requirements.txt pyproject.toml go.mod Gemfile 2>/dev/null | sha256sum | cut -d' ' -f1)
RUN_ALL="$RUN"
if [ -n "$INSTALL" ] && [ "$(cat "$D/installed" 2>/dev/null)" != "$STAMP" ]; then
  RUN_ALL="( $INSTALL ) && echo \"$STAMP\" > \"$D/installed\" && $RUN"
fi
export PORT PREVIEW_BASE
: > "$D/log"
if command -v setsid >/dev/null 2>&1; then
  setsid bash -c "$RUN_ALL" </dev/null >"$D/log" 2>&1 &
else
  nohup bash -c "$RUN_ALL" </dev/null >"$D/log" 2>&1 &
fi
echo $! > "$D/pid"
N=0
while [ $N -lt $WAIT ]; do
  if (exec 3<>/dev/tcp/127.0.0.1/$PORT) 2>/dev/null; then echo STATE=running; exit 0; fi
  if ! kill -0 "$(cat "$D/pid")" 2>/dev/null; then echo STATE=exited; exit 0; fi
  sleep 1; N=$((N+1))
done
echo STATE=starting
`

const previewStopScript = `D="$PREVIEW_DIR"
if [ -f "$D/pid" ]; then
  P=$(cat "$D/pid")
  kill -TERM -- "-$P" 2>/dev/null || { pkill -TERM -P "$P" 2>/dev/null; kill -TERM "$P" 2>/dev/null; } || true
  rm -f "$D/pid"
fi
`

func (pv *Preview) manifest(ctx context.Context, sb sandbox.Sandbox) (Manifest, error) {
	data, _, err := sb.ReadFile(ctx, RepoDir+"/"+ManifestPath, 64<<10)
	if err != nil {
		return Manifest{}, fmt.Errorf("%s does not exist: create it with the run command and the port of the app (and install, if it needs one)", ManifestPath)
	}
	return ParseManifest(data)
}

func (pv *Preview) start(ctx context.Context, rc tools.RunContext) (any, error) {
	if pv.mgr == nil {
		return nil, errors.New("no sandbox")
	}
	sb, err := pv.mgr.Acquire(ctx, rc.ThreadID, rc.Login)
	if err != nil {
		return nil, err
	}
	if sandbox.PortsOf(sb) == nil {
		return previewResult{State: "unavailable", Message: "This sandbox cannot show a preview: its ports are not reachable."}, nil
	}
	m, err := pv.manifest(ctx, sb)
	if err != nil {
		return previewResult{State: "error", Message: err.Error()}, nil
	}
	pv.cache.Delete(rc.ThreadID)
	base := pv.Base(rc.ThreadID)
	env := map[string]string{"REPO_DIR": RepoDir, "PREVIEW_DIR": previewDir, "RUN": m.Run, "INSTALL": m.Install,
		"PORT": fmt.Sprint(m.Port), "PREVIEW_BASE": base, "WAIT": "240"}
	if m.Base == "strip" {
		env["PREVIEW_BASE"] = "/"
	}
	var stdout strings.Builder
	_, err = sb.Exec(ctx, sandbox.ExecRequest{Command: previewStartScript, Env: env, Timeout: 5 * time.Minute,
		OnStdout: func(t string) { stdout.WriteString(t) }})
	if err != nil {
		return nil, err
	}
	state := "starting"
	for _, line := range strings.Split(stdout.String(), "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), "STATE="); ok {
			state = s
		}
	}
	res := previewResult{State: state, Port: m.Port}
	switch state {
	case "running":
		res.URL = base
		res.Message = "The app answers. The user sees it in the Preview tab; it reloads when you edit files."
	case "exited":
		res.Message = "The app stopped right after starting. Read the log, fix the cause and call preview_start again."
		res.Log = pv.tail(ctx, sb)
	default:
		res.URL = base
		res.Message = "The app does not answer on port " + fmt.Sprint(m.Port) + " yet (still installing, or listening on another port or address). " +
			"It must listen on 0.0.0.0:" + fmt.Sprint(m.Port) + ". Check preview_logs."
		res.Log = pv.tail(ctx, sb)
	}
	return res, nil
}

func (pv *Preview) tail(ctx context.Context, sb sandbox.Sandbox) string {
	var out strings.Builder
	_, _ = sb.Exec(ctx, sandbox.ExecRequest{Command: `tail -n 40 "` + previewDir + `/log" 2>/dev/null | cut -c1-400`, Timeout: 20 * time.Second,
		OnStdout: func(t string) { out.WriteString(t) }})
	s := strings.TrimSpace(out.String())
	if len(s) > 4000 {
		s = s[len(s)-4000:]
	}
	return s
}

func (pv *Preview) logs(ctx context.Context, rc tools.RunContext) (any, error) {
	sb, err := pv.mgr.Get(ctx, rc.ThreadID)
	if err != nil {
		return previewResult{State: "stopped", Message: "There is no sandbox yet."}, nil
	}
	return previewResult{State: "logs", Log: pv.tail(ctx, sb)}, nil
}

func (pv *Preview) stop(ctx context.Context, rc tools.RunContext) (any, error) {
	sb, err := pv.mgr.Get(ctx, rc.ThreadID)
	if err != nil {
		return previewResult{State: "stopped"}, nil
	}
	_, err = sb.Exec(ctx, sandbox.ExecRequest{Command: previewStopScript, Env: map[string]string{"PREVIEW_DIR": previewDir}, Timeout: 30 * time.Second})
	pv.cache.Delete(rc.ThreadID)
	if err != nil {
		return nil, err
	}
	return previewResult{State: "stopped"}, nil
}

// ---- the proxy ----

// target finds where the thread's app listens: its manifest and the sandbox's address of that port.
func (pv *Preview) target(ctx context.Context, threadID string) (previewTarget, error) {
	if v, ok := pv.cache.Load(threadID); ok {
		if t := v.(previewTarget); time.Since(t.at) < previewCacheTTL {
			return t, nil
		}
	}
	if pv.mgr == nil {
		return previewTarget{}, errors.New("no sandbox")
	}
	sb, err := pv.mgr.Get(ctx, threadID)
	if err != nil {
		return previewTarget{}, err
	}
	ports := sandbox.PortsOf(sb)
	if ports == nil {
		return previewTarget{}, errors.New("the sandbox does not expose ports")
	}
	m, err := pv.manifest(ctx, sb)
	if err != nil {
		return previewTarget{}, err
	}
	ep, err := ports.Endpoint(ctx, m.Port)
	if err != nil {
		return previewTarget{}, err
	}
	t := previewTarget{manifest: m, endpoint: ep, at: time.Now()}
	pv.cache.Store(threadID, t)
	return t, nil
}

// previewCSP sandboxes the app into an opaque origin: scripts run, but without the visitor's cookies or storage on this
// host, and it may only be framed by the product itself.
const previewCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads allow-pointer-lock; frame-ancestors 'self'"

// ServeHTTP serves /preview/<thread>.<mac>/…
func (pv *Preview) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, _ := strings.CutPrefix(r.URL.Path, httpapi.PreviewPrefix)
	id, sub, _ := strings.Cut(rest, "/")
	threadID, ok := pv.threadOf(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	// The server's own headers (CSP 'self', frame denial) describe the product, not the app.
	h.Del("Content-Security-Policy")
	h.Del("X-Frame-Options")
	h.Set("Referrer-Policy", "no-referrer")
	// The app's module scripts are fetched from an opaque origin, which needs CORS. No credentials are involved: the
	// address is the credential.
	h.Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", r.Header.Get("Access-Control-Request-Headers"))
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if sub == "" && r.URL.Path == strings.TrimSuffix(httpapi.PreviewPrefix+id, "/") {
		http.Redirect(w, r, httpapi.PreviewPrefix+id+"/", http.StatusTemporaryRedirect)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	t, err := pv.target(ctx, threadID)
	cancel()
	if err != nil {
		pv.notRunning(w, err)
		return
	}
	u, err := url.Parse(t.endpoint.URL)
	if err != nil {
		pv.notRunning(w, err)
		return
	}
	base := httpapi.PreviewPrefix + id + "/"
	strip := t.manifest.Base != "app"
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			p := r.URL.Path
			if strip {
				p = "/" + sub
			}
			pr.Out.URL.Path, pr.Out.URL.RawPath = p, ""
			pr.Out.Host = u.Host
			// The visitor's identity stays with the product.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Origin")
			for k, v := range t.endpoint.Headers {
				pr.Out.Header.Set(k, v)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			rh := resp.Header
			rh.Del("Set-Cookie")
			rh.Del("X-Frame-Options")
			rh.Del("Access-Control-Allow-Origin")
			rh.Del("Access-Control-Allow-Credentials")
			rh.Set("Content-Security-Policy", previewCSP)
			if loc := rh.Get("Location"); strip && strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
				rh.Set("Location", base+loc[1:])
			}
			rh.Set("Access-Control-Allow-Origin", "*")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			pv.cache.Delete(threadID)
			slog.Debug("preview: upstream failed", "thread", threadID, "error", err)
			pv.notRunning(w, err)
		},
	}
	rp.ServeHTTP(w, r)
}

func (pv *Preview) notRunning(w http.ResponseWriter, err error) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", previewCSP)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`+
		`<body style="font:15px system-ui,sans-serif;color:#555;display:grid;place-items:center;min-height:100vh;margin:0;text-align:center">`+
		`<div><p><b>The preview is not running.</b></p><p>Ask the assistant to start it, then reload.</p></div>`)
}

// ---- the API ----

type previewInfo struct {
	URL      string    `json:"url"`
	Running  bool      `json:"running"`
	Manifest *Manifest `json:"manifest,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

func (s *server) routePreview() {
	pv := s.opt.Preview
	if pv == nil {
		return
	}
	pv.Team = s.team
	s.Route("GET /threads/{id}/preview", s.threadPreview)
	s.Mux.Handle(httpapi.PreviewPrefix, pv)
}

// threadPreview tells the UI where the preview of the thread is and whether anything answers there.
func (s *server) threadPreview(w http.ResponseWriter, r *http.Request, c Caller) {
	pv := s.opt.Preview
	id := r.PathValue("id")
	info := previewInfo{URL: pv.Base(id)}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	t, err := pv.target(ctx, id)
	if err != nil {
		info.Reason = "not started"
		writeJSON(w, http.StatusOK, info)
		return
	}
	info.Manifest = &t.manifest
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint.URL+"/", nil)
	for k, v := range t.endpoint.Headers {
		req.Header.Set(k, v)
	}
	if resp, err := (&http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req); err == nil {
		_ = resp.Body.Close()
		info.Running = true
	} else {
		info.Reason = "not answering"
	}
	writeJSON(w, http.StatusOK, info)
}
