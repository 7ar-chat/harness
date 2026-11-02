package bench

// ---- go-refactor-notify: an API change that ripples through three packages ----

var goRefactorNotify = Task{
	Name: "go-refactor-notify",
	Prompt: "Refactor package notify from a global function to an injectable interface, and update every caller:\n" +
		"- Remove notify.Send(to, msg string) and the global notify.Out.\n" +
		"- Add type Message struct{ To, Subject, Body string } and interface Sender { Send(ctx context.Context, m Message) error }.\n" +
		"- Add notify.NewWriter(w io.Writer) *Writer, a Sender that writes \"<To>: <Subject>: <Body>\\n\" to w (safe for concurrent use) and returns ctx.Err() without writing when the context is done.\n" +
		"- billing: billing.New(s notify.Sender) *Biller with (*Biller).Charge(ctx, customer string, cents int) error. Amounts <= 0 return an error and send nothing. Otherwise send Message{To: customer, Subject: \"Payment received\", Body: \"charged N cents\"}; a send failure is returned wrapped (errors.Is works).\n" +
		"- alerts: alerts.New(s notify.Sender, thresholdMs int) *Monitor with (*Monitor).Check(ctx, service string, latencyMs int) error. Latency above the threshold sends Message{To: \"oncall\", Subject: \"Latency alert\", Body: \"<service> took <N>ms (limit <T>ms)\"}; otherwise nothing is sent.\n" +
		"- cmd/app must keep building and use the new API. Update the existing tests to the new API." + noAsking,
	Needs: []string{"go"},
	Files: map[string]string{
		"go.mod": "module refnotify\n\ngo 1.21\n",
		"notify/notify.go": `// Package notify delivers notifications.
package notify

import (
	"fmt"
	"io"
	"os"
)

// Out receives every notification.
var Out io.Writer = os.Stdout

// Send delivers msg to the recipient.
func Send(to, msg string) error {
	_, err := fmt.Fprintf(Out, "%s: %s\n", to, msg)
	return err
}
`,
		"billing/billing.go": `// Package billing charges customers.
package billing

import (
	"errors"
	"fmt"

	"refnotify/notify"
)

// Charge bills a customer and tells them.
func Charge(customer string, cents int) error {
	if cents <= 0 {
		return errors.New("billing: invalid amount")
	}
	return notify.Send(customer, fmt.Sprintf("charged %d cents", cents))
}
`,
		"alerts/alerts.go": `// Package alerts pages the on-call engineer.
package alerts

import (
	"fmt"

	"refnotify/notify"
)

// Check pages on-call when latencyMs is above thresholdMs.
func Check(service string, latencyMs, thresholdMs int) error {
	if latencyMs <= thresholdMs {
		return nil
	}
	return notify.Send("oncall", fmt.Sprintf("%s took %dms (limit %dms)", service, latencyMs, thresholdMs))
}
`,
		"cmd/app/main.go": `package main

import (
	"fmt"
	"os"

	"refnotify/alerts"
	"refnotify/billing"
)

func main() {
	if err := billing.Charge("ann", 1250); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := alerts.Check("api", 900, 500); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`,
		"billing/billing_test.go": `package billing

import (
	"bytes"
	"strings"
	"testing"

	"refnotify/notify"
)

func TestChargeNotifies(t *testing.T) {
	var buf bytes.Buffer
	notify.Out = &buf
	if err := Charge("ann", 100); err != nil || !strings.Contains(buf.String(), "ann: charged 100 cents") {
		t.Fatalf("%v %q", err, buf.String())
	}
}
`,
		"alerts/alerts_test.go": `package alerts

import (
	"bytes"
	"testing"

	"refnotify/notify"
)

func TestQuietBelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	notify.Out = &buf
	if err := Check("api", 100, 500); err != nil || buf.Len() != 0 {
		t.Fatalf("%v %q", err, buf.String())
	}
}
`,
	},
	Hidden: map[string]string{
		"notify/notify_test.go": `package notify

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

func TestWriterFormatsAndHonoursContext(t *testing.T) {
	var buf bytes.Buffer
	var s Sender = NewWriter(&buf)
	if err := s.Send(context.Background(), Message{To: "ann", Subject: "Hi", Body: "there"}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "ann: Hi: there\n" {
		t.Fatalf("%q", buf.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	buf.Reset()
	if err := s.Send(ctx, Message{To: "x"}); err == nil || buf.Len() != 0 {
		t.Fatalf("cancelled send: %v %q", err, buf.String())
	}
}

func TestWriterIsSafeForConcurrentUse(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = w.Send(context.Background(), Message{To: "a", Subject: "b", Body: "c"})
			}
		}()
	}
	wg.Wait()
	if buf.Len() != 1000*len("a: b: c\n") {
		t.Fatalf("len %d", buf.Len())
	}
}
`,
		"billing/billing_test.go": `package billing

import (
	"context"
	"errors"
	"testing"

	"refnotify/notify"
)

type fake struct {
	sent []notify.Message
	err  error
}

func (f *fake) Send(ctx context.Context, m notify.Message) error {
	f.sent = append(f.sent, m)
	return f.err
}

func TestChargeNotifies(t *testing.T) {
	f := &fake{}
	if err := New(f).Charge(context.Background(), "ann", 100); err != nil {
		t.Fatal(err)
	}
	want := notify.Message{To: "ann", Subject: "Payment received", Body: "charged 100 cents"}
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("%+v", f.sent)
	}
}

func TestInvalidAmountSendsNothing(t *testing.T) {
	f := &fake{}
	for _, c := range []int{0, -5} {
		if err := New(f).Charge(context.Background(), "ann", c); err == nil {
			t.Errorf("%d: no error", c)
		}
	}
	if len(f.sent) != 0 {
		t.Fatalf("%+v", f.sent)
	}
}

func TestSendFailureIsWrapped(t *testing.T) {
	boom := errors.New("boom")
	err := New(&fake{err: boom}).Charge(context.Background(), "ann", 5)
	if !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
}
`,
		"alerts/alerts_test.go": `package alerts

import (
	"context"
	"testing"

	"refnotify/notify"
)

type fake struct{ sent []notify.Message }

func (f *fake) Send(ctx context.Context, m notify.Message) error {
	f.sent = append(f.sent, m)
	return nil
}

func TestPagesOnCallAboveTheThreshold(t *testing.T) {
	f := &fake{}
	m := New(f, 500)
	if err := m.Check(context.Background(), "api", 500); err != nil || len(f.sent) != 0 {
		t.Fatalf("at the threshold: %v %+v", err, f.sent)
	}
	if err := m.Check(context.Background(), "api", 900); err != nil {
		t.Fatal(err)
	}
	want := notify.Message{To: "oncall", Subject: "Latency alert", Body: "api took 900ms (limit 500ms)"}
	if len(f.sent) != 1 || f.sent[0] != want {
		t.Fatalf("%+v", f.sent)
	}
}
`,
	},
	Verify: "go build ./... && go vet ./... && go test -count=1 ./... && ! grep -rnE 'notify\\.Out|notify\\.Send\\(|^var Out' --include=*.go .",
	Solution: map[string]string{
		"notify/notify.go": `// Package notify delivers notifications.
package notify

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Message is one notification.
type Message struct{ To, Subject, Body string }

// Sender delivers messages.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Writer is a Sender that writes one line per message.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a Writer on w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Send writes the message unless ctx is done.
func (w *Writer) Send(ctx context.Context, m Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := fmt.Fprintf(w.w, "%s: %s: %s\n", m.To, m.Subject, m.Body)
	return err
}
`,
		"billing/billing.go": `// Package billing charges customers.
package billing

import (
	"context"
	"errors"
	"fmt"

	"refnotify/notify"
)

// Biller charges customers and tells them.
type Biller struct{ s notify.Sender }

// New returns a Biller that notifies through s.
func New(s notify.Sender) *Biller { return &Biller{s: s} }

// Charge bills a customer and tells them.
func (b *Biller) Charge(ctx context.Context, customer string, cents int) error {
	if cents <= 0 {
		return errors.New("billing: invalid amount")
	}
	err := b.s.Send(ctx, notify.Message{To: customer, Subject: "Payment received", Body: fmt.Sprintf("charged %d cents", cents)})
	if err != nil {
		return fmt.Errorf("billing: notify %s: %w", customer, err)
	}
	return nil
}
`,
		"alerts/alerts.go": `// Package alerts pages the on-call engineer.
package alerts

import (
	"context"
	"fmt"

	"refnotify/notify"
)

// Monitor pages on-call when latency is too high.
type Monitor struct {
	s           notify.Sender
	thresholdMs int
}

// New returns a Monitor.
func New(s notify.Sender, thresholdMs int) *Monitor { return &Monitor{s: s, thresholdMs: thresholdMs} }

// Check pages on-call when latencyMs is above the threshold.
func (m *Monitor) Check(ctx context.Context, service string, latencyMs int) error {
	if latencyMs <= m.thresholdMs {
		return nil
	}
	return m.s.Send(ctx, notify.Message{To: "oncall", Subject: "Latency alert",
		Body: fmt.Sprintf("%s took %dms (limit %dms)", service, latencyMs, m.thresholdMs)})
}
`,
		"cmd/app/main.go": `package main

import (
	"context"
	"fmt"
	"os"

	"refnotify/alerts"
	"refnotify/billing"
	"refnotify/notify"
)

func main() {
	ctx := context.Background()
	out := notify.NewWriter(os.Stdout)
	if err := billing.New(out).Charge(ctx, "ann", 1250); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := alerts.New(out, 500).Check(ctx, "api", 900); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`,
		"billing/billing_test.go": `package billing

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"refnotify/notify"
)

func TestChargeNotifies(t *testing.T) {
	var buf bytes.Buffer
	if err := New(notify.NewWriter(&buf)).Charge(context.Background(), "ann", 100); err != nil || !strings.Contains(buf.String(), "ann: Payment received: charged 100 cents") {
		t.Fatalf("%v %q", err, buf.String())
	}
}
`,
		"alerts/alerts_test.go": `package alerts

import (
	"bytes"
	"context"
	"testing"

	"refnotify/notify"
)

func TestQuietBelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	if err := New(notify.NewWriter(&buf), 500).Check(context.Background(), "api", 100); err != nil || buf.Len() != 0 {
		t.Fatalf("%v %q", err, buf.String())
	}
}
`,
	},
}

// ---- go-conventions: the repository's AGENTS.md decides how the change must be made ----

var goConventions = Task{
	Name:   "go-conventions",
	Prompt: "Add a GET /greet endpoint to the app package: /greet?name=Ann answers 200 with the body \"Hello, Ann\"; a missing or blank name answers 400. Follow this repository's conventions." + noAsking,
	Needs:  []string{"go", "make", "bash"},
	Files: map[string]string{
		"go.mod": "module hello\n\ngo 1.21\n",
		"AGENTS.md": `# Repository conventions

- Finish with: make check  (gofmt, go vet, go test and scripts/conventions.sh). It must pass.
- Errors: every error value a package returns is a sentinel declared in that package's errs.go and named ErrXxx.
  Outside errs.go there is no errors.New and no fmt.Errorf without %w.
- HTTP: handlers live in handlers.go. A route exists only if it is a row of the Routes table in routes.go, and
  every route is documented as a table row in docs/ROUTES.md: | METHOD | /path | description |
- Error responses write the sentinel's message: http.Error(w, ErrXxx.Error(), status).
- Tests are table tests in handlers_test.go.
`,
		"Makefile": "check:\n\t@test -z \"$$(gofmt -l .)\" || (echo 'gofmt needed:'; gofmt -l .; exit 1)\n\tgo vet ./...\n\tgo test ./...\n\tbash scripts/conventions.sh\n",
		"scripts/conventions.sh": `#!/bin/bash
# Checks the rules of AGENTS.md that gofmt/vet cannot.
fail=0
bad=$(grep -n 'errors\.New(' app/*.go | grep -v '^app/errs.go' | grep -v '_test.go')
if [ -n "$bad" ]; then echo "errors.New outside errs.go:"; echo "$bad"; fail=1; fi
bad=$(grep -n 'fmt\.Errorf(' app/*.go | grep -v '%w' | grep -v '_test.go')
if [ -n "$bad" ]; then echo "fmt.Errorf without %w:"; echo "$bad"; fail=1; fi
bad=$(grep 'errors\.New(' app/errs.go | grep -vE '^(var )?[[:space:]]*Err[A-Z][A-Za-z0-9]* += errors\.New')
if [ -n "$bad" ]; then echo "sentinels must be named ErrXxx:"; echo "$bad"; fail=1; fi
for p in $(grep -o '"/[^"]*"' app/routes.go | tr -d '"'); do
  if ! grep -qF "| $p |" docs/ROUTES.md; then echo "route $p is not documented in docs/ROUTES.md"; fail=1; fi
done
if grep -n 'HandleFunc\|http\.Handle(' app/*.go | grep -v '^app/routes.go' | grep -v '_test.go'; then echo "routes must be registered in routes.go"; fail=1; fi
exit $fail
`,
		"docs/ROUTES.md": "# Routes\n\n| Method | Path | Description |\n|---|---|---|\n| GET | /ping | liveness probe |\n",
		"app/errs.go": `package app

import "errors"

// ErrMethod is returned for a request method a route does not accept.
var ErrMethod = errors.New("method not allowed")
`,
		"app/handlers.go": `package app

import (
	"fmt"
	"net/http"
)

// Ping answers the liveness probe.
func Ping(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "pong")
}
`,
		"app/routes.go": `package app

import "net/http"

// Route is one HTTP endpoint.
type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
}

// Routes is every endpoint of the app.
var Routes = []Route{
	{"GET", "/ping", Ping},
}

// NewMux builds the handler for Routes.
func NewMux() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range Routes {
		rt := rt
		mux.HandleFunc(rt.Path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != rt.Method {
				http.Error(w, ErrMethod.Error(), http.StatusMethodNotAllowed)
				return
			}
			rt.Handler(w, r)
		})
	}
	return mux
}
`,
		"app/handlers_test.go": `package app

import (
	"net/http/httptest"
	"testing"
)

func TestPing(t *testing.T) {
	for _, c := range []struct {
		method string
		code   int
	}{{"GET", 200}, {"POST", 405}} {
		rec := httptest.NewRecorder()
		NewMux().ServeHTTP(rec, httptest.NewRequest(c.method, "/ping", nil))
		if rec.Code != c.code {
			t.Errorf("%s /ping = %d, want %d", c.method, rec.Code, c.code)
		}
	}
}
`,
	},
	Hidden: map[string]string{
		"app/greet_hidden_test.go": `package app

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGreetBehaviour(t *testing.T) {
	for _, c := range []struct {
		method, url string
		code        int
		body        string
	}{
		{"GET", "/greet?name=Ann", 200, "Hello, Ann"},
		{"GET", "/greet?name=Jo+Bo", 200, "Hello, Jo Bo"},
		{"GET", "/greet", 400, ""},
		{"GET", "/greet?name=%20%20", 400, ""},
		{"POST", "/greet?name=Ann", 405, ""},
		{"GET", "/ping", 200, "pong"},
	} {
		rec := httptest.NewRecorder()
		NewMux().ServeHTTP(rec, httptest.NewRequest(c.method, c.url, nil))
		if rec.Code != c.code {
			t.Errorf("%s %s = %d, want %d", c.method, c.url, rec.Code, c.code)
		}
		if c.body != "" && strings.TrimSpace(rec.Body.String()) != c.body {
			t.Errorf("%s %s body %q, want %q", c.method, c.url, rec.Body.String(), c.body)
		}
		if c.code == 400 && strings.TrimSpace(rec.Body.String()) == "" {
			t.Errorf("%s %s: a 400 needs an explanation", c.method, c.url)
		}
	}
}
`,
	},
	Verify: "make check",
	Solution: map[string]string{
		"app/errs.go": `package app

import "errors"

// ErrMethod is returned for a request method a route does not accept.
var ErrMethod = errors.New("method not allowed")

// ErrNoName is returned when a greeting has no name.
var ErrNoName = errors.New("name is required")
`,
		"app/handlers.go": `package app

import (
	"fmt"
	"net/http"
	"strings"
)

// Ping answers the liveness probe.
func Ping(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "pong")
}

// Greet says hello to the name query parameter.
func Greet(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, ErrNoName.Error(), http.StatusBadRequest)
		return
	}
	fmt.Fprintf(w, "Hello, %s", name)
}
`,
		"app/routes.go": `package app

import "net/http"

// Route is one HTTP endpoint.
type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
}

// Routes is every endpoint of the app.
var Routes = []Route{
	{"GET", "/ping", Ping},
	{"GET", "/greet", Greet},
}

// NewMux builds the handler for Routes.
func NewMux() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range Routes {
		rt := rt
		mux.HandleFunc(rt.Path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != rt.Method {
				http.Error(w, ErrMethod.Error(), http.StatusMethodNotAllowed)
				return
			}
			rt.Handler(w, r)
		})
	}
	return mux
}
`,
		"docs/ROUTES.md": "# Routes\n\n| Method | Path | Description |\n|---|---|---|\n| GET | /ping | liveness probe |\n| GET | /greet | greets the name query parameter |\n",
		"app/handlers_test.go": `package app

import (
	"net/http/httptest"
	"testing"
)

func TestPing(t *testing.T) {
	for _, c := range []struct {
		method, url string
		code        int
	}{{"GET", "/ping", 200}, {"POST", "/ping", 405}, {"GET", "/greet?name=Ann", 200}, {"GET", "/greet", 400}} {
		rec := httptest.NewRecorder()
		NewMux().ServeHTTP(rec, httptest.NewRequest(c.method, c.url, nil))
		if rec.Code != c.code {
			t.Errorf("%s %s = %d, want %d", c.method, c.url, rec.Code, c.code)
		}
	}
}
`,
	},
}

// ---- go-config-format: a config file format change with its data file and callers ----

var goConfigFormat = Task{
	Name: "go-config-format",
	Prompt: "Move the config format from flat keys to sections, and migrate the shipped file and its readers:\n" +
		"- conf.Load(path) must understand [section] headers: a key under [db] is read as \"db.host\"; keys before any header keep their bare name. The same key in two sections is fine; the same full key twice is an error containing \"duplicate\".\n" +
		"- Values may contain ${VAR}, replaced from the environment; an unset variable is an error naming the variable. Add conf.LoadWith(path, lookup func(string) (string, bool)) that does the same with a custom lookup (Load uses os.LookupEnv).\n" +
		"- Every parse error must say which line (\"line N\"): bad section header, a line without '=', an empty key, a duplicate.\n" +
		"- Rewrite config/default.conf in the new format with [db] (host, port, name) and [server] (listen), same values, and update package app to read the new keys." + noAsking,
	Needs: []string{"go"},
	Files: map[string]string{
		"go.mod":              "module confapp\n\ngo 1.21\n",
		"config/default.conf": "# default settings\ndb_host = localhost\ndb_port = 5432\ndb_name = app\nlisten = :8080\n",
		"conf/conf.go": `// Package conf reads key = value configuration files.
package conf

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config maps keys to values.
type Config map[string]string

// Load reads path: one key = value per line, # starts a comment line.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := Config{}
	for i, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", i+1)
		}
		c[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return c, nil
}

// Get returns the value of key ("" when absent).
func (c Config) Get(key string) string { return c[key] }

// Int returns the value of key as an integer.
func (c Config) Int(key string) (int, error) { return strconv.Atoi(c[key]) }
`,
		"app/app.go": `// Package app builds runtime settings from a Config.
package app

import (
	"fmt"

	"confapp/conf"
)

// DSN is the database connection string.
func DSN(c conf.Config) string {
	return fmt.Sprintf("postgres://%s:%s/%s", c.Get("db_host"), c.Get("db_port"), c.Get("db_name"))
}

// Listen is the address the server binds.
func Listen(c conf.Config) string { return c.Get("listen") }
`,
		"conf/conf_test.go": `package conf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFlat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.conf")
	_ = os.WriteFile(p, []byte("# c\na = 1\nb=two\n"), 0o644)
	c, err := Load(p)
	if err != nil || c.Get("a") != "1" || c.Get("b") != "two" {
		t.Fatalf("%v %v", c, err)
	}
}
`,
		"app/app_test.go": `package app

import (
	"testing"

	"confapp/conf"
)

func TestDefaults(t *testing.T) {
	c, err := conf.Load("../config/default.conf")
	if err != nil || DSN(c) != "postgres://localhost:5432/app" || Listen(c) != ":8080" {
		t.Fatalf("%v %v", c, err)
	}
}
`,
	},
	Hidden: map[string]string{
		"conf/conf_test.go": `package conf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSectionsAndBareKeys(t *testing.T) {
	c, err := Load(write(t, "top = 1\n# note\n[db]\nhost = h\nport=5432\n\n[server]\nhost = s\n[ empty ]\n"))
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"top": "1", "db.host": "h", "db.port": "5432", "server.host": "s"} {
		if c.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, c.Get(k), want)
		}
	}
	if n, err := c.Int("db.port"); err != nil || n != 5432 {
		t.Errorf("Int = %d, %v", n, err)
	}
	if c.Get("host") != "" {
		t.Error("a sectioned key must not be readable bare")
	}
}

func TestEnvironmentExpansion(t *testing.T) {
	look := func(k string) (string, bool) {
		v, ok := map[string]string{"HOME_DIR": "/home/a", "PORT": "9"}[k]
		return v, ok
	}
	c, err := LoadWith(write(t, "[p]\ndir = ${HOME_DIR}/data\nmix = ${PORT}-${PORT}\nplain = $5\n"), look)
	if err != nil {
		t.Fatal(err)
	}
	if c.Get("p.dir") != "/home/a/data" || c.Get("p.mix") != "9-9" || c.Get("p.plain") != "$5" {
		t.Fatalf("%v", c)
	}
	_, err = LoadWith(write(t, "a = ${NOPE}\n"), look)
	if err == nil || !strings.Contains(err.Error(), "NOPE") || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("undefined variable: %v", err)
	}
}

func TestLoadUsesTheProcessEnvironment(t *testing.T) {
	t.Setenv("CONF_TEST_VAR", "yes")
	c, err := Load(write(t, "a = ${CONF_TEST_VAR}\n"))
	if err != nil || c.Get("a") != "yes" {
		t.Fatalf("%v %v", c, err)
	}
}

func TestErrorsNameTheLine(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"a = 1\n[db\nb = 2\n", "line 2"},
		{"a = 1\n\njust text\n", "line 3"},
		{"= 4\n", "line 1"},
		{"[s]\nk = 1\nk = 2\n", "line 3"},
		{"k = 1\nk = 2\n", "line 2"},
	} {
		_, err := Load(write(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want it to mention %s", c.body, err, c.want)
		}
	}
	_, err := Load(write(t, "[s]\nk = 1\nk = 2\n"))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := Load(write(t, "[a]\nk = 1\n[b]\nk = 2\n")); err != nil {
		t.Errorf("the same key in two sections must be fine: %v", err)
	}
}
`,
		"app/app_test.go": `package app

import (
	"os"
	"strings"
	"testing"

	"confapp/conf"
)

func TestShippedDefaultsUseTheNewFormat(t *testing.T) {
	c, err := conf.Load("../config/default.conf")
	if err != nil {
		t.Fatal(err)
	}
	if DSN(c) != "postgres://localhost:5432/app" || Listen(c) != ":8080" {
		t.Fatalf("%q %q", DSN(c), Listen(c))
	}
	raw, _ := os.ReadFile("../config/default.conf")
	if !strings.Contains(string(raw), "[db]") || !strings.Contains(string(raw), "[server]") {
		t.Fatalf("default.conf is not in the sectioned format:\n%s", raw)
	}
	if c.Get("db_host") != "" || c.Get("listen") != "" {
		t.Fatal("the old flat keys are still present")
	}
}
`,
	},
	Verify: "go vet ./... && go test -count=1 ./...",
	Solution: map[string]string{
		"config/default.conf": "# default settings\n[db]\nhost = localhost\nport = 5432\nname = app\n\n[server]\nlisten = :8080\n",
		"conf/conf.go": `// Package conf reads key = value configuration files with [sections].
package conf

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config maps full keys ("db.host") to values.
type Config map[string]string

// Load reads path, expanding ${VAR} from the environment.
func Load(path string) (Config, error) { return LoadWith(path, os.LookupEnv) }

// LoadWith reads path, expanding ${VAR} with lookup.
func LoadWith(path string, lookup func(string) (string, bool)) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := Config{}
	section := ""
	for i, raw := range strings.Split(string(b), "\n") {
		n := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: bad section header %q", n, line)
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			return nil, fmt.Errorf("line %d: empty key", n)
		}
		if section != "" {
			k = section + "." + k
		}
		if _, dup := c[k]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", n, k)
		}
		v, err := expand(v, lookup)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		c[k] = v
	}
	return c, nil
}

func expand(v string, lookup func(string) (string, bool)) (string, error) {
	var out strings.Builder
	for {
		i := strings.Index(v, "${")
		if i < 0 {
			out.WriteString(v)
			return out.String(), nil
		}
		j := strings.Index(v[i:], "}")
		if j < 0 {
			return "", fmt.Errorf("unterminated ${ in %q", v)
		}
		name := v[i+2 : i+j]
		val, ok := lookup(name)
		if !ok {
			return "", fmt.Errorf("undefined variable %s", name)
		}
		out.WriteString(v[:i])
		out.WriteString(val)
		v = v[i+j+1:]
	}
}

// Get returns the value of key ("" when absent).
func (c Config) Get(key string) string { return c[key] }

// Int returns the value of key as an integer.
func (c Config) Int(key string) (int, error) { return strconv.Atoi(c[key]) }
`,
		"app/app.go": `// Package app builds runtime settings from a Config.
package app

import (
	"fmt"

	"confapp/conf"
)

// DSN is the database connection string.
func DSN(c conf.Config) string {
	return fmt.Sprintf("postgres://%s:%s/%s", c.Get("db.host"), c.Get("db.port"), c.Get("db.name"))
}

// Listen is the address the server binds.
func Listen(c conf.Config) string { return c.Get("server.listen") }
`,
	},
}

// ---- py-misleading-readme: the README describes behaviour the tests do not want ----

var pyMisleadingReadme = Task{
	Name:   "py-misleading-readme",
	Prompt: "The TTL cache in ttlcache loses entries too early: test_overwriting_restarts_the_clock fails. Fix the bug. Run the tests with: python3 -m unittest discover -s tests -t .",
	Needs:  []string{"python3"},
	Files: map[string]string{
		"README.md": `# ttlcache

A tiny in-memory cache with expiry.

## Design

- **Sliding expiration.** Every successful ` + "`get()`" + ` refreshes the entry's TTL, so hot keys never expire.
  If entries expire too early, the place to look is ` + "`Cache.get`" + `: it forgets to touch the timestamp.
- The ttl is given in milliseconds.
- ` + "`set()`" + ` records the time the key was first stored.
`,
		"ttlcache/__init__.py": "from .cache import Cache\n",
		"ttlcache/cache.py": `import time


class Cache:
    """Maps keys to values that expire ttl seconds after they were stored."""

    def __init__(self, ttl, clock=time.monotonic):
        self.ttl = ttl
        self._clock = clock
        self._data = {}
        self._stamp = {}

    def set(self, key, value):
        self._data[key] = value
        self._stamp.setdefault(key, self._clock())

    def get(self, key, default=None):
        if key not in self._data:
            return default
        if self._clock() - self._stamp[key] >= self.ttl:
            self.delete(key)
            return default
        return self._data[key]

    def delete(self, key):
        self._data.pop(key, None)
        self._stamp.pop(key, None)

    def __len__(self):
        return len(self._data)

    def purge(self):
        """Drop every expired entry and return how many were dropped."""
        now = self._clock()
        dead = [k for k, t in self._stamp.items() if now - t >= self.ttl]
        for k in dead:
            self.delete(k)
        return len(dead)
`,
		"tests/__init__.py": "",
		"tests/test_cache.py": `import unittest

from ttlcache import Cache


class Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now


class CacheTest(unittest.TestCase):
    def test_overwriting_restarts_the_clock(self):
        clock = Clock()
        c = Cache(10, clock)
        c.set("a", 1)
        clock.now = 8
        c.set("a", 2)
        clock.now = 15
        self.assertEqual(c.get("a"), 2)
`,
	},
	Hidden: map[string]string{
		"tests/test_cache.py": `import unittest

from ttlcache import Cache


class Clock:
    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now


class CacheTest(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.c = Cache(10, self.clock)

    def test_overwriting_restarts_the_clock(self):
        self.c.set("a", 1)
        self.clock.now = 8
        self.c.set("a", 2)
        self.clock.now = 15
        self.assertEqual(self.c.get("a"), 2)
        self.clock.now = 18
        self.assertIsNone(self.c.get("a"))

    def test_ttl_is_in_seconds_and_expires_at_the_boundary(self):
        self.c.set("a", 1)
        self.clock.now = 9.99
        self.assertEqual(self.c.get("a"), 1)
        self.clock.now = 10
        self.assertIsNone(self.c.get("a"))

    def test_reading_does_not_extend_the_life(self):
        self.c.set("a", 1)
        self.clock.now = 6
        self.assertEqual(self.c.get("a"), 1)
        self.clock.now = 11
        self.assertIsNone(self.c.get("a"))

    def test_purge_counts_only_expired_entries(self):
        self.c.set("old", 1)
        self.clock.now = 7
        self.c.set("new", 2)
        self.clock.now = 12
        self.assertEqual(self.c.purge(), 1)
        self.assertEqual(len(self.c), 1)
        self.assertEqual(self.c.get("new"), 2)

    def test_default_and_delete(self):
        self.assertEqual(self.c.get("x", "d"), "d")
        self.c.set("x", 1)
        self.c.delete("x")
        self.c.delete("x")
        self.assertEqual(len(self.c), 0)
`,
	},
	Verify: "python3 -m unittest discover -s tests -t .",
	Solution: map[string]string{
		"ttlcache/cache.py": `import time


class Cache:
    """Maps keys to values that expire ttl seconds after they were last stored."""

    def __init__(self, ttl, clock=time.monotonic):
        self.ttl = ttl
        self._clock = clock
        self._data = {}
        self._stamp = {}

    def set(self, key, value):
        self._data[key] = value
        self._stamp[key] = self._clock()

    def get(self, key, default=None):
        if key not in self._data:
            return default
        if self._clock() - self._stamp[key] >= self.ttl:
            self.delete(key)
            return default
        return self._data[key]

    def delete(self, key):
        self._data.pop(key, None)
        self._stamp.pop(key, None)

    def __len__(self):
        return len(self._data)

    def purge(self):
        """Drop every expired entry and return how many were dropped."""
        now = self._clock()
        dead = [k for k, t in self._stamp.items() if now - t >= self.ttl]
        for k in dead:
            self.delete(k)
        return len(dead)
`,
	},
}

// ---- node-csv: a Node task (skipped without node) ----

var nodeCSV = Task{
	Name: "node-csv",
	Prompt: "csv.js parses CSV with a naive split. Make it handle RFC 4180 input and add stringify:\n" +
		"- parse(text) returns an array of rows (arrays of strings). Quoted fields may contain commas, doubled quotes (\"\" is one quote) and line breaks. Rows end with LF or CRLF; a final line break does not add an empty row; completely empty lines are skipped; empty fields are kept (\"a,,b\" is three fields). An unterminated quote throws an Error whose message contains \"unterminated\".\n" +
		"- parseObjects(text) uses the first row as the header; a missing cell is the empty string.\n" +
		"- stringify(rows) writes rows joined by \"\\n\" with a final \"\\n\", quoting a field only when it contains a comma, a quote, CR or LF (quotes doubled), so parse(stringify(rows)) gives rows back. stringify([]) is the empty string.\n" +
		"Run the tests with: node --test" + noAsking,
	Needs: []string{"node"},
	Files: map[string]string{
		"package.json": "{\n  \"name\": \"csvlib\",\n  \"version\": \"1.0.0\",\n  \"scripts\": { \"test\": \"node --test\" }\n}\n",
		"csv.js": `'use strict';

// parse splits CSV text into rows of fields.
function parse(text) {
  return text.split('\n').filter((l) => l.length).map((l) => l.split(','));
}

// parseObjects turns the rows into objects keyed by the header row.
function parseObjects(text) {
  const [head, ...rows] = parse(text);
  return rows.map((r) => Object.fromEntries(head.map((h, i) => [h, r[i]])));
}

module.exports = { parse, parseObjects };
`,
		"test/csv.test.js": `'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { parse } = require('../csv.js');

test('plain rows', () => {
  assert.deepStrictEqual(parse('a,b\n1,2\n'), [['a', 'b'], ['1', '2']]);
});
`,
	},
	Hidden: map[string]string{
		"test/csv.test.js": `'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { parse, parseObjects, stringify } = require('../csv.js');

test('plain rows, LF and CRLF, trailing newline', () => {
  assert.deepStrictEqual(parse('a,b\n1,2\n'), [['a', 'b'], ['1', '2']]);
  assert.deepStrictEqual(parse('a,b\r\n1,2\r\n'), [['a', 'b'], ['1', '2']]);
  assert.deepStrictEqual(parse('a,b\n1,2'), [['a', 'b'], ['1', '2']]);
});

test('empty fields are kept and empty lines are skipped', () => {
  assert.deepStrictEqual(parse('a,,b\n\n,,\n'), [['a', '', 'b'], ['', '', '']]);
  assert.deepStrictEqual(parse('x,\n'), [['x', '']]);
});

test('quoted fields', () => {
  assert.deepStrictEqual(parse('"a,b",c\n'), [['a,b', 'c']]);
  assert.deepStrictEqual(parse('"say ""hi""",x\n'), [['say "hi"', 'x']]);
  assert.deepStrictEqual(parse('"line1\nline2",z\r\n"q",r\n'), [['line1\nline2', 'z'], ['q', 'r']]);
  assert.deepStrictEqual(parse('"",a\n'), [['', 'a']]);
});

test('an unterminated quote throws', () => {
  assert.throws(() => parse('a,"b\nc'), /unterminated/);
});

test('parseObjects', () => {
  assert.deepStrictEqual(parseObjects('name,note\nann,"hi, there"\nbob\n'), [
    { name: 'ann', note: 'hi, there' },
    { name: 'bob', note: '' },
  ]);
});

test('stringify quotes only when needed and round-trips', () => {
  const rows = [['a', 'b,c', 'say "x"'], ['multi\nline', '', 'plain']];
  const out = stringify(rows);
  assert.strictEqual(out, 'a,"b,c","say ""x"""\n"multi\nline",,plain\n');
  assert.deepStrictEqual(parse(out), rows);
  assert.strictEqual(stringify([]), '');
});
`,
	},
	Verify: "node --test",
	Solution: map[string]string{
		"csv.js": `'use strict';

// parse splits RFC 4180 CSV text into rows of fields.
function parse(text) {
  const rows = [];
  let row = [];
  let field = '';
  let quoted = false;
  let started = false; // the current row has any content
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (quoted) {
      if (ch === '"') {
        if (text[i + 1] === '"') {
          field += '"';
          i++;
        } else {
          quoted = false;
        }
      } else {
        field += ch;
      }
      continue;
    }
    if (ch === '"') {
      quoted = true;
      started = true;
    } else if (ch === ',') {
      row.push(field);
      field = '';
      started = true;
    } else if (ch === '\n' || ch === '\r') {
      if (ch === '\r' && text[i + 1] === '\n') i++;
      if (started) {
        row.push(field);
        rows.push(row);
      }
      row = [];
      field = '';
      started = false;
    } else {
      field += ch;
      started = true;
    }
  }
  if (quoted) throw new Error('csv: unterminated quoted field');
  if (started) {
    row.push(field);
    rows.push(row);
  }
  return rows;
}

// parseObjects turns the rows into objects keyed by the header row.
function parseObjects(text) {
  const [head, ...rows] = parse(text);
  if (!head) return [];
  return rows.map((r) => Object.fromEntries(head.map((h, i) => [h, r[i] === undefined ? '' : r[i]])));
}

function quote(f) {
  return /[",\r\n]/.test(f) ? '"' + f.replace(/"/g, '""') + '"' : f;
}

// stringify writes rows as CSV with LF line ends.
function stringify(rows) {
  return rows.map((r) => r.map(quote).join(',') + '\n').join('');
}

module.exports = { parse, parseObjects, stringify };
`,
	},
}
