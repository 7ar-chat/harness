package split_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/hosts/httpapi"
	"github.com/7ar-chat/harness/redisinfra"
)

// The web tier and the workers share one Postgres database (own per test), one miniredis and one fake LLM; the
// workers are real harness-worker processes, the web tier is a harness in this process that never calls Run.

const (
	schema   = "work"
	user     = "alice"
	deadline = 30 * time.Second
)

var (
	buildOnce sync.Once
	buildDir  string
	workerBin string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// workerBinary builds cmd/harness-worker once per test binary and skips the test when it cannot be built.
func workerBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			buildErr = fmt.Errorf("the go toolchain is not available: %w", err)
			return
		}
		if buildDir, err = os.MkdirTemp("", "harness-worker-bin"); err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(buildDir, "harness-worker")
		cmd := exec.Command(goBin, "build", "-o", out, "github.com/7ar-chat/harness/app/cmd/harness-worker")
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build harness-worker: %v\n%s", err, b)
			return
		}
		workerBin = out
	})
	if buildErr != nil {
		t.Skipf("cannot build the harness-worker binary: %v", buildErr)
	}
	return workerBin
}

// eventually polls cond until it holds or the deadline passes (it waits for a condition, never for time).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// rig is the shared infrastructure of one test.
type rig struct {
	t    *testing.T
	pg   *pgxpool.Pool // search_path = work
	dsn  *url.URL      // the test database
	mr   *miniredis.Miniredis
	llm  *fakeLLM
	web  *harness.Harness
	srv  *httptest.Server
	http *http.Client
}

func newRig(t *testing.T) *rig {
	t.Helper()
	base := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL is not set: the web/worker split needs Postgres")
	}
	workerBinary(t)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	r := &rig{t: t, http: &http.Client{}}
	r.dsn = createDatabase(t, base)

	pool, err := pgstore.NewPool(ctx, r.dsnWithSchema(), 4, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	r.pg = pool
	r.mr = miniredis.RunT(t)
	r.llm = newFakeLLM(t)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_BASE_URL", r.llm.BaseURL())
	t.Setenv("LLM_API_KEY", "x")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_DEFAULT_MODEL", "gpt-test")
	t.Setenv("LLM_STEP_RETRIES", "0")
	t.Setenv("AGENT_PERMISSIONS", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: r.mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := pgstore.NewPG(pool)
	infra := redisinfra.Infra(rdb, st)
	r.web, err = harness.New(harness.Options{Config: cfg, Store: st, Infra: &infra, RuntimeID: "web"})
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(r.web, httpapi.Options{
		Authenticate: func(req *http.Request) (string, error) { return req.Header.Get("X-User"), nil },
		Models:       cfg.LLMModels, DefaultModel: "gpt-test",
	})
	r.srv = httptest.NewServer(api)
	t.Cleanup(r.srv.Close)
	return r
}

// createDatabase creates a throwaway database on the server of base and drops it at cleanup.
func createDatabase(t *testing.T, base string) *url.URL {
	t.Helper()
	ctx := context.Background()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to %s: %v", u.Host, err)
	}
	defer admin.Close(ctx)
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	name := "e2e_" + hex.EncodeToString(rnd[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			t.Errorf("drop %s: %v", name, err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	db := *u
	db.Path = "/" + name
	return &db
}

func (r *rig) dsnWithSchema() string {
	u := *r.dsn
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// runtimeOf is the runtime id that owns the run in the shared store.
func (r *rig) runtimeOf(runID string) string {
	r.t.Helper()
	var id string
	err := r.pg.QueryRow(context.Background(), `SELECT coalesce(runtime_id, '') FROM work_runs WHERE id = $1`, runID).Scan(&id)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// ageHeartbeat makes the run look abandoned for 10 minutes in Postgres and in Redis.
func (r *rig) ageHeartbeat(runID string) {
	r.t.Helper()
	tag, err := r.pg.Exec(context.Background(), `UPDATE work_runs SET heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, runID)
	if err != nil || tag.RowsAffected() != 1 {
		r.t.Fatalf("aging the heartbeat of %s: %v (%d rows)", runID, err, tag.RowsAffected())
	}
	r.mr.FastForward(2 * time.Minute)
}

// --- the web tier, driven over HTTP ---

func (r *rig) do(method, path, body string) (int, []byte) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("X-User", user)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (r *rig) call(method, path, body string, want int) map[string]any {
	r.t.Helper()
	code, b := r.do(method, path, body)
	if code != want {
		r.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, b)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func (r *rig) newThread() string {
	r.t.Helper()
	return r.call("POST", "/api/threads", `{}`, 201)["id"].(string)
}

func (r *rig) send(thread, text string) string {
	r.t.Helper()
	return r.call("POST", "/api/threads/"+thread+"/runs", `{"message":`+strconv.Quote(text)+`}`, 202)["id"].(string)
}

func (r *rig) runStatus(runID string) string {
	r.t.Helper()
	return r.call("GET", "/api/runs/"+runID, "", 200)["status"].(string)
}

// waitStatus waits until the run has the status (the row is written just after the last event).
func (r *rig) waitStatus(runID, want string) {
	r.t.Helper()
	var got string
	eventually(r.t, "run "+runID+" to be "+want, func() bool { got = r.runStatus(runID); return got == want })
}

type message struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"toolCallId"`
	ToolCalls  []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"toolCalls"`
}

func (r *rig) messages(thread string) []message {
	r.t.Helper()
	code, b := r.do("GET", "/api/threads/"+thread, "")
	if code != 200 {
		r.t.Fatalf("GET thread: %d %s", code, b)
	}
	var th struct {
		Messages []message `json:"messages"`
	}
	if err := json.Unmarshal(b, &th); err != nil {
		r.t.Fatal(err)
	}
	return th.Messages
}

// sseEvent is one event of a run stream.
type sseEvent struct {
	Seq  int64
	Type string
	Raw  string
}

func (e sseEvent) field(name string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(e.Raw), &m)
	s, _ := m[name].(string)
	return s
}

// stream reads GET /api/runs/{id}/events from seq `after` until the server ends the stream.
func (r *rig) stream(runID string, after int64) []sseEvent {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", r.srv.URL+"/api/runs/"+runID+"/events?after="+strconv.FormatInt(after, 10), nil)
	req.Header.Set("X-User", user)
	resp, err := r.http.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		r.t.Fatalf("events: status %d", resp.StatusCode)
	}
	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.Seq, _ = strconv.ParseInt(line[4:], 10, 64)
		case strings.HasPrefix(line, "data: "):
			cur.Raw = line[6:]
			cur.Type = cur.field("type")
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	if ctx.Err() != nil {
		r.t.Fatalf("the event stream of %s did not end within %v (%d events)", runID, deadline, len(out))
	}
	return out
}

func eventTypes(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

// streamedText concatenates the text deltas of a stream.
func streamedText(evs []sseEvent) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Type == "TEXT_MESSAGE_CONTENT" {
			b.WriteString(e.field("delta"))
		}
	}
	return b.String()
}

// --- worker processes ---

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// worker is a running harness-worker process.
type worker struct {
	t         *testing.T
	id        string
	admin     string // host:port of /readyz
	workspace string
	cmd       *exec.Cmd
	stderr    *lockedBuffer
	exited    chan struct{}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startWorker starts `harness-worker -id id -concurrency 1` on the rig's Postgres, Redis and fake LLM and returns
// once /readyz answers 200.
func (r *rig) startWorker(id string) *worker {
	r.t.Helper()
	t := r.t
	w := &worker{t: t, id: id, workspace: t.TempDir(), stderr: &lockedBuffer{}, exited: make(chan struct{})}
	w.admin = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	rhost, rport, _ := net.SplitHostPort(r.mr.Addr())
	pw, _ := r.dsn.User.Password()
	w.cmd = exec.Command(workerBin, "-id", id, "-concurrency", "1")
	w.cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"LLM_PROVIDER=openai", "LLM_BASE_URL=" + r.llm.BaseURL(), "LLM_API_KEY=x",
		"LLM_MODELS=gpt-test", "LLM_DEFAULT_MODEL=gpt-test", "LLM_STEP_RETRIES=0",
		"POSTGRES_HOST=" + r.dsn.Hostname(), "POSTGRES_PORT=" + r.dsn.Port(),
		"POSTGRES_DB=" + strings.TrimPrefix(r.dsn.Path, "/"), "POSTGRES_USER=" + r.dsn.User.Username(),
		"POSTGRES_PASSWORD=" + pw, "POSTGRES_SSLMODE=disable", "POSTGRES_SCHEMA=" + schema,
		"REDIS_HOST=" + rhost, "REDIS_PORT=" + rport,
		"SANDBOX_BACKEND=local", "LOCAL_WORKSPACE=" + w.workspace, "LOCAL_STATE_DIR=" + t.TempDir(),
		"AGENT_PERMISSIONS=auto-edit",
		"RUNTIME_SHUTDOWN_GRACE_SECONDS=1",
		"HARNESS_WORKER_ADDR=" + w.admin,
	}
	w.cmd.Stderr = w.stderr
	w.cmd.Stdout = w.stderr
	if err := w.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.cmd.Wait(); close(w.exited) }()
	t.Cleanup(func() {
		w.Kill()
		if t.Failed() {
			t.Logf("stderr of %s:\n%s", id, w.stderr.String())
		}
	})
	eventually(t, "worker "+id+" to be ready\n"+"(see the stderr of the worker)", func() bool {
		select {
		case <-w.exited:
			t.Fatalf("worker %s exited before it was ready:\n%s", id, w.stderr.String())
		default:
		}
		return w.Ready()
	})
	return w
}

// Ready reports whether GET /readyz answers 200.
func (w *worker) Ready() bool {
	resp, err := http.Get("http://" + w.admin + "/readyz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// Kill sends SIGKILL (no grace, no cleanup) and waits for the process to be gone.
func (w *worker) Kill() {
	_ = w.cmd.Process.Kill()
	<-w.exited
}
