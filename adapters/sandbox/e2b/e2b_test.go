package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
)

// fake is one httptest server standing in for both the control plane and the envd agent of a sandbox.
type fake struct {
	t       *testing.T
	srv     *httptest.Server
	created map[string]any // body of the last POST /sandboxes
	apiKey  string
	start   func(w http.ResponseWriter, r *http.Request) // handler of /process.Process/Start
	routes  map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := f.routes[r.URL.Path]; ok { // a test's own route wins over the defaults below
			h(w, r)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			f.apiKey = r.Header.Get("X-API-Key")
			_ = json.NewDecoder(r.Body).Decode(&f.created)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sandboxID":"sb1","envdAccessToken":"tok","domain":"example.test"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sb1/connect":
			_, _ = w.Write([]byte(`{"sandboxID":"sb1","envdAccessToken":"tok2"}`))
		case strings.HasPrefix(r.URL.Path, "/sandboxes/missing"):
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/sandboxes/bad"):
			http.Error(w, `{"code":400,"message":"Invalid sandbox ID"}`, http.StatusBadRequest)
		case r.URL.Path == "/process.Process/Start" && f.start != nil:
			f.start(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) backend(o Options) *E2B {
	o.APIKey, o.APIURL, o.EnvdURL = "secret-key-123", f.srv.URL, f.srv.URL
	b, err := New(o)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func frame(v any) []byte {
	data, _ := json.Marshal(v)
	return connectFrame(0, data)
}

func data(kind, text string) []byte {
	return frame(map[string]any{"event": map[string]any{"data": map[string]any{kind: base64.StdEncoding.EncodeToString([]byte(text))}}})
}

func end(code int) []byte {
	return frame(map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": code, "exited": true}}})
}

func start(pid int) []byte {
	return frame(map[string]any{"event": map[string]any{"start": map[string]any{"pid": pid}}})
}

func stream(parts ...[]byte) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		for _, p := range parts {
			_, _ = w.Write(p)
		}
		_, _ = w.Write(connectFrame(flagEndStream, []byte("{}")))
	}
}

func TestCreateSendsTheKeyTemplateMetadataAndTheEgressPolicy(t *testing.T) {
	// Arrange
	f := newFake(t)
	f.start = stream(start(1), end(0))
	b := f.backend(Options{Template: "mine", TimeoutSeconds: 900, EgressAllowlist: []string{"pypi.org"}})

	// Act
	inst, err := b.Create(context.Background(), sandbox.CreateOptions{Metadata: map[string]string{"work-thread": "t1"}})

	// Assert
	if err != nil || inst.ID() != "sb1" {
		t.Fatalf("Create = %v, %v", inst, err)
	}
	if f.apiKey != "secret-key-123" || f.created["templateID"] != "mine" || f.created["timeout"] != float64(900) {
		t.Fatalf("request: key=%q body=%v", f.apiKey, f.created)
	}
	net, _ := f.created["network"].(map[string]any)
	if net["allowOut"].([]any)[0] != "pypi.org" || net["denyOut"].([]any)[0] != "0.0.0.0/0" {
		t.Fatalf("allowlist mode must deny everything but the list: %v", net)
	}
	if md, _ := f.created["metadata"].(map[string]any); md["work-thread"] != "t1" {
		t.Fatalf("metadata: %v", f.created["metadata"])
	}
}

func TestTheTemplateFollowsTheProfileAndFallsBackToTheDefault(t *testing.T) {
	for profile, want := range map[string]string{"work": "office-tpl", "coding": "default-tpl", "": "default-tpl"} {
		f := newFake(t)
		f.start = stream(start(1), end(0))
		b := f.backend(Options{Template: "default-tpl", Images: map[string]string{"work": "office-tpl"}})

		_, err := b.Create(context.Background(), sandbox.CreateOptions{Profile: profile})

		if err != nil || f.created["templateID"] != want {
			t.Errorf("profile %q: template %v (want %q), err %v", profile, f.created["templateID"], want, err)
		}
	}
}

func TestUnrestrictedEgressStillDeniesPrivateRanges(t *testing.T) {
	n := networkConfig(Options{})

	deny, _ := n["denyOut"].([]string)
	if len(deny) == 0 || deny[0] != "10.0.0.0/8" || n["allowOut"] != nil {
		t.Fatalf("unrestricted mode: %v", n)
	}
}

func TestTheHarnessDefaultDenyListIsReducedToWhatE2BAccepts(t *testing.T) {
	// the harness default (config SANDBOX_EGRESS_DENY_CIDRS) names IPv6 ranges and 0.0.0.0/8, which E2B rejects
	cfg := []string{"172.16.0.0/12", "10.0.0.0/8", "0.0.0.0/8", "fc00::/7", "fe80::/10", "::ffff:0:0/96", "169.254.169.254", "garbage"}

	n := networkConfig(Options{EgressDenyCIDRs: cfg})

	got, _ := n["denyOut"].([]string)
	if strings.Join(got, ",") != "172.16.0.0/12,10.0.0.0/8,169.254.169.254" {
		t.Fatalf("denyOut = %v", got)
	}
	if only := networkConfig(Options{EgressDenyCIDRs: []string{"fc00::/7"}}); len(only["denyOut"].([]string)) != len(DefaultEgressDenyCIDRs) {
		t.Fatalf("a list E2B cannot express at all falls back to the package default: %v", only)
	}
}

func TestConnectToAnIdThatDoesNotExistIsDefinitive(t *testing.T) {
	f := newFake(t)
	b := f.backend(Options{})

	for _, id := range []string{"missing", "bad"} {
		if _, err := b.Connect(context.Background(), id); !errors.Is(err, sandbox.ErrNoSandbox) {
			t.Errorf("Connect(%q) = %v, want ErrNoSandbox", id, err)
		}
	}
}

func TestAServerErrorOnConnectIsTransientNotAbsence(t *testing.T) {
	f := newFake(t)
	f.routes["/sandboxes/sb1/connect"] = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusBadGateway) }
	b := f.backend(Options{})

	_, err := b.Connect(context.Background(), "sb1")

	if errors.Is(err, sandbox.ErrNoSandbox) || !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("a 502 must not read as a missing sandbox: %v", err)
	}
}

func TestExecKeepsStdoutAndStderrApartAndReturnsTheExitCode(t *testing.T) {
	// Arrange
	f := newFake(t)
	f.start = stream(start(7), data("stdout", "out1 "), data("stderr", "err1"), data("stdout", "out2"), end(3))
	inst := f.backend(Options{}).instance(sandboxInfo{SandboxID: "sb1", EnvdAccessToken: "tok"})
	var out, errOut strings.Builder

	// Act
	res, err := inst.Exec(context.Background(), sandbox.ExecRequest{Command: "x", OnStdout: func(s string) { out.WriteString(s) }, OnStderr: func(s string) { errOut.WriteString(s) }})

	// Assert
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("Exec = %+v, %v (a non-zero exit is a result)", res, err)
	}
	if out.String() != "out1 out2" || errOut.String() != "err1" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestExecSendsTheCommandToBashWithCwdAndEnv(t *testing.T) {
	f := newFake(t)
	var got map[string]any
	var auth, token string
	f.start = func(w http.ResponseWriter, r *http.Request) {
		auth, token = r.Header.Get("Authorization"), r.Header.Get("X-Access-Token")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body[5:], &got)
		stream(start(1), end(0))(w, r)
	}
	inst := f.backend(Options{}).instance(sandboxInfo{SandboxID: "sb1", EnvdAccessToken: "tok"})

	_, err := inst.Exec(context.Background(), sandbox.ExecRequest{Command: "echo $X; echo 'a b'", Cwd: "/workspace", Env: map[string]string{"X": "it's"}})

	p, _ := got["process"].(map[string]any)
	args, _ := p["args"].([]any)
	if err != nil || p["cmd"] != "/bin/bash" || args[0] != "-c" || args[1] != "echo $X; echo 'a b'" || p["cwd"] != "/workspace" || p["envs"].(map[string]any)["X"] != "it's" {
		t.Fatalf("err=%v process=%v", err, p)
	}
	if !strings.HasPrefix(auth, "Basic ") || token != "tok" {
		t.Fatalf("auth=%q token=%q", auth, token)
	}
}

func TestAMultiByteCharacterSplitAcrossChunksArrivesWhole(t *testing.T) {
	f := newFake(t)
	euro := []byte("€") // 3 bytes
	f.start = stream(start(1), data("stdout", "a"+string(euro[:1])), data("stdout", string(euro[1:])+"b"), end(0))
	inst := f.backend(Options{}).instance(sandboxInfo{SandboxID: "sb1"})
	var chunks []string

	_, err := inst.Exec(context.Background(), sandbox.ExecRequest{Command: "x", OnStdout: func(s string) { chunks = append(chunks, s) }})

	if err != nil || strings.Join(chunks, "") != "a€b" {
		t.Fatalf("chunks %q err %v", chunks, err)
	}
	for _, c := range chunks {
		if strings.ContainsRune(c, '�') {
			t.Fatalf("a split character was corrupted: %q", chunks)
		}
	}
}

func TestAStreamThatEndsWithoutAnExitStatusIsAnErrorNotSuccess(t *testing.T) {
	f := newFake(t)
	f.start = stream(start(1), data("stdout", "partial"))
	inst := f.backend(Options{}).instance(sandboxInfo{SandboxID: "sb1"})

	_, err := inst.Exec(context.Background(), sandbox.ExecRequest{Command: "x"})

	if !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestExecTimeoutReturnsAnErrorWithoutWaitingForTheCommand(t *testing.T) {
	f := newFake(t)
	f.start = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(start(9))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the command never ends
	}
	inst := f.backend(Options{}).instance(sandboxInfo{SandboxID: "sb1"})
	t0 := time.Now()

	_, err := inst.Exec(context.Background(), sandbox.ExecRequest{Command: "sleep 100", Timeout: 200 * time.Millisecond})

	if !errors.Is(err, context.DeadlineExceeded) || time.Since(t0) > 5*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(t0))
	}
}

func TestAMissingFileIsErrNotFoundAndAGoneSandboxIsErrGone(t *testing.T) {
	f := newFake(t)
	f.routes["/filesystem.Filesystem/Stat"] = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"code":"not_found","message":"no such file"}`, http.StatusNotFound)
	}
	f.routes["/filesystem.Filesystem/ListDir"] = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"sandboxId":"sb1","message":"The sandbox was not found","code":502}`, http.StatusBadGateway)
	}
	f.routes["/sandboxes/sb1"] = func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }
	inst := f.backend(Options{}).instance(sandboxInfo{SandboxID: "sb1"})

	_, statErr := inst.Stat(context.Background(), "/nope")
	_, listErr := inst.List(context.Background(), "/workspace", 1)

	if !errors.Is(statErr, sandbox.ErrNotFound) {
		t.Errorf("Stat = %v", statErr)
	}
	if !sandbox.IsGone(listErr) {
		t.Errorf("List on a vanished sandbox = %v, want a gone sandbox", listErr)
	}
}

func TestListParsesSixtyFourBitSizesGivenAsStrings(t *testing.T) {
	f := newFake(t)
	f.routes["/filesystem.Filesystem/ListDir"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"entries":[{"name":"d","type":"FILE_TYPE_DIRECTORY","path":"/w/d","size":"60","modifiedTime":"2026-11-02T22:27:22Z"},{"name":"a","type":"FILE_TYPE_FILE","path":"/w/d/a","size":"9"}]}`))
	}
	inst := f.backend(Options{}).instance(sandboxInfo{SandboxID: "sb1"})

	got, err := inst.List(context.Background(), "/w", 2)

	if err != nil || len(got) != 2 || got[0].Type != "directory" || got[0].Size != 60 || got[1].Type != "file" || got[1].Size != 9 || got[0].ModifiedAt.IsZero() {
		t.Fatalf("List = %+v, %v", got, err)
	}
}

func TestFramesRoundTripAndRefuseAnAbsurdLength(t *testing.T) {
	payload := []byte(`{"a":1}`)

	flags, got, err := readFrame(bytes.NewReader(connectFrame(flagEndStream, payload)))
	_, _, bigErr := readFrame(bytes.NewReader([]byte{0, 0xFF, 0xFF, 0xFF, 0xFF}))

	if err != nil || flags != flagEndStream || !bytes.Equal(got, payload) {
		t.Fatalf("round trip: %d %q %v", flags, got, err)
	}
	if bigErr == nil {
		t.Fatal("a 4 GiB frame must be refused before allocating")
	}
}

func TestAPIErrorsNeverQuoteTheKey(t *testing.T) {
	err := scrub(errors.New(`Post "https://x?k=secret-key-123": EOF`), "secret-key-123")

	if strings.Contains(err.Error(), "secret-key-123") {
		t.Fatalf("key leaked: %v", err)
	}
}

func TestTheRegistrySettingsNeedAKeyAndNameItsSources(t *testing.T) {
	_, err := BackendFromSettings(backends.E2BSettings{})
	b, err2 := BackendFromSettings(backends.E2BSettings{APIKey: "k"})

	if err == nil || !strings.Contains(err.Error(), "E2B_API_KEY") || !strings.Contains(err.Error(), "harness auth login e2b") {
		t.Fatalf("err = %v", err)
	}
	if err2 != nil || b == nil {
		t.Fatalf("with a key: %v, %v", b, err2)
	}
	if !contains(backends.Registered(), "e2b") {
		t.Fatalf("e2b did not register: %v", backends.Registered())
	}
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}
