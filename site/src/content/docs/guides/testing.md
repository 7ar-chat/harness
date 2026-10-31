---
title: Test your agent
description: "Test code that embeds the harness without a network or a key: a scripted model, a fake sandbox backend, the Client conformance suite and golden tests for terminal front ends."
---

Everything the harness needs from the outside can be replaced by a test double that ships with the modules: a scripted chat model (`llmtest`), an in-memory sandbox backend (`sandboxtest`), an in-process `Client` (`hosts/inproc`) and a conformance suite for any `Client` (`hosts/clienttest`). With them a test runs a complete conversation, tools included, in milliseconds and offline. The harness' own tests use the same helpers, as does [`examples/website`](https://github.com/7ar-chat/harness/tree/main/examples/website).

## A complete test file

The code under test is a harness with one tool of the application (a `lookup_order` tool scoped to the run's user):

```go
// Package shop is the code under test: a harness with one tool of the application.
package shop

import (
	"context"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/tools"
)

type Orders map[string]string // "user/id" -> status

type orderTools struct{ orders Orders }

func (orderTools) Name() string { return "orders" }

func (p orderTools) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	type args struct {
		OrderID string `json:"orderId" jsonschema_description:"The order number"`
	}
	t, err := ai.InferTool("lookup_order", "Look up one of the customer's orders.",
		func(_ context.Context, in args) (string, error) {
			if s, ok := p.orders[rc.Login+"/"+in.OrderID]; ok {
				return s, nil
			}
			return "no such order for this customer", nil
		})
	return []ai.BaseTool{t}, nil, err
}

// New builds the application's harness. Tests pass a scripted model and a fake backend;
// production passes nil for both (the configuration chooses).
func New(cfg *config.Config, models llm.Factory, backend sandbox.Backend, orders Orders) (*harness.Harness, error) {
	return harness.New(harness.Options{
		Config:   cfg,
		Models:   models,
		Backend:  backend,
		Features: harness.ProfileWork(),
		Tools:    []tools.Provider{orderTools{orders}},
	})
}
```

The tests build it over a scripted model and a fake backend, run a conversation and assert on events, on what the model was offered and on what the backend saw:

```go
package shop_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/guides-testing/shop"
	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/clienttest"
	"github.com/7ar-chat/harness/hosts/httpapi"
	"github.com/7ar-chat/harness/hosts/inproc"
)

// start builds the harness over a scripted model and a fake backend, and runs it for the test.
func start(t *testing.T, model *llmtest.Model, backend *sandboxtest.Backend) *harness.Harness {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // quiet logs
	t.Setenv("HARNESS_NO_USER_DIR", "1")                            // hermetic: read nothing from ~/.har
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_DEFAULT_MODEL", "gpt-test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := shop.New(cfg, llmtest.Factory{M: model}, backend, shop.Orders{"alice/A-1042": "shipped"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	return h
}

// ask sends one message and returns the event types and the text of the answer.
func ask(t *testing.T, h *harness.Harness, user, text string, mode harness.SandboxMode) (types []string, answer string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	th, err := h.NewThread(ctx, user, "")
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.Send(ctx, user, th.ID, text, harness.RunOptions{Model: "gpt-test", Sandbox: mode})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range h.Events(ctx, run.ID, 0) {
		types = append(types, ev.Type)
		if ev.Type == agui.TextMessageContent {
			var e struct{ Delta string }
			_ = json.Unmarshal(ev.Data, &e)
			answer += e.Delta
		}
		if agui.IsTerminal(ev.Type) {
			return types, answer
		}
	}
	t.Fatal("the event stream ended before the run finished")
	return
}

// callThenAnswer scripts the model: call one tool, then repeat the tool's result.
func callThenAnswer(tool, args string) *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 { // the title generator calls the model without tools
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("Result: ", last.Content)}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", tool, args), "tool_calls")}}
	})
}

func TestTheToolSeesTheRunsUser(t *testing.T) {
	// Arrange
	h := start(t, callThenAnswer("lookup_order", `{"orderId":"A-1042"}`), sandboxtest.NewBackend())

	// Act
	_, forAlice := ask(t, h, "alice", "where is A-1042?", harness.SandboxOff)
	_, forBob := ask(t, h, "bob", "where is A-1042?", harness.SandboxOff)

	// Assert
	if forAlice != "Result: shipped" {
		t.Fatalf("alice got %q", forAlice)
	}
	if forBob != "Result: no such order for this customer" {
		t.Fatalf("bob got %q", forBob)
	}
}

func TestAnOffRunNeverCreatesASandbox(t *testing.T) {
	// Arrange
	model := callThenAnswer("lookup_order", `{"orderId":"A-1042"}`)
	backend := sandboxtest.NewBackend()
	h := start(t, model, backend)

	// Act
	ask(t, h, "alice", "where is A-1042?", harness.SandboxOff)

	// Assert: what the model was offered, and what the backend saw
	var names []string
	for _, ti := range model.Calls()[0].Tools {
		names = append(names, ti.Name)
	}
	if strings.Contains(strings.Join(names, " "), "execute_command") {
		t.Fatalf("an off run was offered %v", names)
	}
	if n := len(backend.Created()); n != 0 {
		t.Fatalf("%d sandboxes created", n)
	}
}

func TestACommandCreatesTheSandboxOnFirstUse(t *testing.T) {
	// Arrange
	backend := sandboxtest.NewBackend()
	h := start(t, callThenAnswer("execute_command", `{"command":"echo hi"}`), backend)

	// Act
	types, _ := ask(t, h, "alice", "run echo", harness.SandboxAuto)

	// Assert
	if types[len(types)-1] != agui.RunFinished {
		t.Fatalf("events %v", types)
	}
	created := backend.Created()
	if len(created) != 1 {
		t.Fatalf("%d sandboxes created, want 1", len(created))
	}
	var ran []string
	for _, req := range created[0].Execs() { // every command the agent ran in it
		ran = append(ran, req.Command)
	}
	if !strings.Contains(strings.Join(ran, "\n"), "echo hi") {
		t.Fatalf("commands %q", ran)
	}
}

// Both Client implementations must behave the same: the suite checks the contract
// (ownership, errors, event resume, steering...). Your own Client passes through here too.
func TestInProcessClientConformance(t *testing.T) {
	clienttest.Run(t, func(_ *testing.T, h *harness.Harness, user string) client.Client {
		return inproc.New(h, user, inproc.WithModels(clienttest.Models, clienttest.DefaultModel))
	})
}

// A front end that talks HTTP is checked against the real handler the same way.
func TestHTTPClientConformance(t *testing.T) {
	clienttest.Run(t, func(t *testing.T, h *harness.Harness, user string) client.Client {
		srv := httptest.NewServer(httpapi.New(h, httpapi.Options{
			Authenticate: func(r *http.Request) (string, error) { // the bearer token is the login in this test
				return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), nil
			},
			Models: clienttest.Models, DefaultModel: clienttest.DefaultModel,
		}))
		t.Cleanup(srv.Close)
		return client.HTTP(srv.URL, user)
	})
}

func TestAProviderErrorEndsTheRunWithRunError(t *testing.T) {
	// Arrange: the model fails on every call
	h := start(t, llmtest.New(llmtest.Turn{Err: errors.New("status code: 400, bad request")}), nil)

	// Act
	types, _ := ask(t, h, "alice", "hello", harness.SandboxOff)

	// Assert
	if types[len(types)-1] != agui.RunError {
		t.Fatalf("events %v", types)
	}
}
```

In a scratch module with `go.work` pointing at a checkout of v0.3.1, `go vet ./...` is clean and `go test ./... -v` reports (observed):

```text
--- PASS: TestTheToolSeesTheRunsUser
--- PASS: TestAnOffRunNeverCreatesASandbox
--- PASS: TestACommandCreatesTheSandboxOnFirstUse
--- PASS: TestInProcessClientConformance
--- PASS: TestHTTPClientConformance
--- PASS: TestAProviderErrorEndsTheRunWithRunError
PASS
```

Two details in `start` matter for every test of an embedding: `HARNESS_NO_USER_DIR=1` makes `config.Load` read nothing from `~/.har` (so a developer's own keys, `AGENTS.md` and skills never leak into the test), and the `LLM_*` variables name the model ids the run may use (`Options.Config` is required, and `RunOptions.Model` must be one the configuration allows).

## Script the model

`llmtest.Model` plays a list of turns, one per model call, streaming each turn's chunks the way a provider would, and records every call. It needs no network or key.

| Helper | Purpose |
|---|---|
| `llmtest.New(turns...)` | plays the turns in order, then answers `done` |
| `llmtest.NewFunc(fn)` | computes each turn from the call (`llmtest.Call`: `Messages`, `Tools`, `ToolChoice`, `ReasoningEffort`) |
| `llmtest.Text(parts...)` | streamed text chunks |
| `llmtest.ToolCall(idx, id, name, args)` | a single-chunk tool call; wrap it with `llmtest.Finish(chunk, "tool_calls")` |
| `llmtest.Reasoning`, `WithUsage`, `WithCachedUsage` | reasoning chunks, token usage on a chunk |
| `llmtest.Factory{M: model}` | an `llm.Factory` for `harness.Options.Models` |
| `model.Calls()` | every call recorded: what the code under test asked for |

A `Turn` can also fail instead of answering, to exercise the error paths:

| Field | Effect |
|---|---|
| `Err` | fail the call before any chunk (a provider error) |
| `FailAfter` | fail the stream after the chunks |
| `Block` | go silent after the chunks until the context ends (a watchdog test) |
| `ChunkDelay`, `KeepAlive`, `Pings` | a slow stream, silent reasoning, a gateway pinging a hung upstream |
| `RetryAfter`, `Refusal` | the `Retry-After` hint and refusal text a real transport would record |

The last test above uses `Err`: the run ends with `RUN_ERROR` as its terminal event.

<div class="callout note">

**Script by call, not by position, when the profile has more than one model call per run.** The work profile also generates a thread title with a model call that has no tools. `NewFunc` with the `len(c.Tools) == 0` branch (as in `callThenAnswer`) keeps the script stable; with `llmtest.New` the title call would consume a turn meant for the agent. `ProfileMinimal` has no title call.

</div>

A runnable scripted conversation without the harness is in the package examples (`ExampleModel`): the model answers a `weather` tool call, then a text turn.

## Fake the sandbox

`sandboxtest.NewBackend()` is an in-memory `sandbox.Backend`. Pass it as `Options.Backend` and the sandbox tools work against fake sandboxes; it records what happened:

| Method | Returns |
|---|---|
| `backend.Created()` | the sandboxes created, in order |
| `backend.CreateOptions()` | the options of each creation |
| `backend.Killed()` | the ids killed |
| `backend.Forget(id)`, `SetConnectErr(err)` | simulate a sandbox that expired, or a failing `Connect` |

`Created()` is the assertion for the sandbox policy: a question run with `SandboxOff` creates none, and a run with `SandboxAuto` creates one only when a command runs (see the tests above and [Embed in a website](/docs/guides/embed-website/)). Each fake sandbox records the commands it ran (`sb.Execs()`, used in the third test above) and its files (`sb.File(path)`). To script command output, set `backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(...) }`; `sandboxtest.ShellExec(sb)` is an `ExecFunc` that runs real shell commands against a temporary copy of the sandbox's in-memory `/workspace`.

To test a sandbox backend you wrote, run the adapter conformance suite instead: `sandboxtest.RunConformance(t, func(t *testing.T) sandbox.Backend { return myadapter.New(...) })`. See [Write a sandbox backend](/docs/guides/sandbox-backend/).

## Test a Client implementation

`hosts/client.Client` is the contract a front end is written against. Two implementations exist and behave the same: `hosts/inproc` drives a harness in the same process, and `client.HTTP(baseURL, token)` talks to a server that serves `hosts/httpapi`. `clienttest.Run` is the conformance suite: it builds its own harness (scripted model, fake sandbox) and asks your factory for a `client.Client` acting for a given user. It covers model listing, the thread lifecycle, send and stream, resuming a stream from a sequence number, steering and cancelling, approvals and `ask_user`, validation, and ownership (another user's thread is `client.ErrNotFound`).

```go
clienttest.Run(t, func(t *testing.T, h *harness.Harness, user string) client.Client {
	return inproc.New(h, user, inproc.WithModels(clienttest.Models, clienttest.DefaultModel))
})
```

The test file above runs it twice: in process, and through `httpapi.New` behind `httptest.NewServer` (the bearer token stands in for the login). Use the same shape for a Client of your own (a Slack bot's backend, a different transport), or for a server that wraps the harness with your own authentication.

For a front end under test, `inproc.New(h, user)` over a harness with a scripted model is the fastest real backend: it needs no server and yields real AG-UI events. Errors compare with `errors.Is(err, client.ErrNotFound)` (and the other `client.Err*` values). The contract is documented in [The Client contract](/docs/reference/client-contract/), and building a front end on it in [Use hosts/client and hosts/tui on their own](/docs/guides/use-client-tui/).

## Golden tests for terminal output

The terminal UI (`hosts/tui`) is tested against golden files: a test drives the model with scripted events and compares the rendered view with `testdata/<name>.golden`. The tests run with an ASCII colour profile so the files hold plain text. The repository's helper is small enough to copy (adapted here):

```go
var updateGolden = flag.Bool("update", false, "rewrite the golden files")

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

// golden compares got with testdata/<name>.golden (or rewrites it with -update).
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	got = strings.TrimRight(got, "\n") + "\n"
	if *updateGolden {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from the golden file.\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}
```

Review the diff of a rewritten golden file like any other change: it is the visible behaviour of the UI. This helper is adapted from the repository's `hosts/tui` tests (it was not run outside them).

## Habits that keep tests reliable

- **Arrange, Act, Assert** in every test, and a name that states the behaviour (`TestAnOffRunNeverCreatesASandbox`).
- **Per-test configuration**: use `t.Setenv` and `config.Load()`; never share a `Config` or a harness between tests.
- **Cancel the context** that runs `h.Run` with `t.Cleanup`, as `start` does, so no goroutine outlives the test.
- **Bound every wait** with a `context.WithTimeout`; `Events` closes only after the terminal event or when its context ends.
- **Test ownership**: ask as one user and read as another; the answer must be not found, with no hint that the id exists.
- **Keep real providers out of the default run.** If you test against a real model, put it behind a build tag or an environment check.

## Next steps

- Tool and feature patterns to test: [Add a custom tool](/docs/guides/custom-tool/), [Add a custom feature](/docs/guides/custom-feature/).
- The harness' own conformance runs: `make test` in the repository ([Releasing and versioning](/docs/guides/releasing/)).
