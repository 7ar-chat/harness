---
title: "Use core and modelinfo on their own"
description: "Import the small vocabulary modules without the engine: AG-UI events, store and blob ports, the sandbox contract with a Manager and a conformance suite, and model prices and context windows."
---

Two modules of the repository are useful without the engine. `github.com/7ar-chat/harness/core` is the vocabulary everything else agrees on: events, domain types and the ports that implementations plug into. `github.com/7ar-chat/harness/modelinfo` knows what a model can do and what it costs. Use them to write a front end or an event consumer, a sandbox backend or a store, or to show prices in your own app. If you only want to call models and run a tool loop, [the ai module](/docs/guides/use-ai/) is enough and does not need either.

```bash
go get github.com/7ar-chat/harness/core
go get github.com/7ar-chat/harness/modelinfo
```

`core` depends on one small module (uuid) and nothing else from this repository. `modelinfo` has no dependencies. Which module sits where is on the [Module map](/docs/introduction/modules/).

## What core holds

| Package | Holds |
|---|---|
| `core/agui` | the [AG-UI](https://docs.ag-ui.com) events a run streams (run, step, text, reasoning, tool call, interrupt, custom), `Usage`, error codes, `CheckOrder` to verify a stream |
| `core/sandbox` | the `Backend` / `Instance` / `Manager` contract for the place an agent runs commands, the background-job registry, and `sandboxtest` (an in-memory fake and a conformance suite) |
| `core/store` | threads, runs, messages, approvals, memory, artifacts, skills and the persistence interfaces that store them (`Store`, `Full`) |
| `core/blobs` | the port for content stored by key |
| `core/layout` | the names of the folders a project and a user keep (`.har/...`) |

## Events, blobs and a sandbox in one program

The program below imports only `core` (its `go list -deps` shows `agui`, `blobs`, `sandbox` and `sandboxtest`) and runs offline. It builds an event stream and checks its order, implements the five-method `blobs.Blobs` port in memory, and gives a thread a sandbox through `sandbox.NewManager` over the in-memory `sandboxtest` backend.

```go
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/blobs"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// memBlobs is a blobs.Blobs in memory: the interface is five methods.
type memBlobs struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (b *memBlobs) Put(_ context.Context, key string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[key] = append([]byte(nil), data...)
	return nil
}

func (b *memBlobs) Get(_ context.Context, key string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d, ok := b.m[key]; ok {
		return d, nil
	}
	return nil, blobs.ErrNotFound
}

func (b *memBlobs) Has(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.m[key]
	return ok, nil
}

func (b *memBlobs) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, key)
	return nil
}

func (b *memBlobs) MaxBytes() int64 { return 0 } // no cap

// threadStore is the part of a thread store the sandbox Manager needs.
type threadStore struct {
	mu sync.Mutex
	sb map[string]string // thread -> sandbox id
}

func (s *threadStore) ThreadSandbox(_ context.Context, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sb[id], nil
}

func (s *threadStore) SetThreadSandbox(ctx context.Context, id, expected, sandboxID string) error {
	_, err := s.SwapThreadSandbox(ctx, id, expected, sandboxID)
	return err
}

func (s *threadStore) SwapThreadSandbox(_ context.Context, id, expected, sandboxID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sb[id] != expected {
		return false, nil
	}
	s.sb[id] = sandboxID
	return true, nil
}

func (s *threadStore) CountUserSandboxes(context.Context, string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sb), nil
}

func (s *threadStore) UserSandboxes(context.Context, string) ([]sandbox.UserSandbox, error) {
	return nil, nil
}

func main() {
	ctx := context.Background()

	// 1. agui: build the events of a run and check their order.
	evs := []agui.Event{
		agui.RunStartedEvent("thread-1", "run-1"),
		agui.New(agui.TextMessageStart, map[string]any{"messageId": "m1", "role": "assistant"}),
		agui.New(agui.TextMessageContent, map[string]any{"messageId": "m1", "delta": "hello"}),
		agui.New(agui.TextMessageEnd, map[string]any{"messageId": "m1"}),
		agui.RunFinishedEvent("thread-1", "run-1", "success", []agui.Usage{{Model: "m", InputTokens: 12, OutputTokens: 3, TotalTokens: 15}}),
	}
	fmt.Println("well-formed run:", agui.CheckOrder(evs))
	bad := []agui.Event{evs[0], evs[1], evs[4]} // RUN_FINISHED with a text message still open
	fmt.Println("broken run:", agui.CheckOrder(bad))

	// 2. blobs: content stored by key, keys are content hashes.
	var store blobs.Blobs = &memBlobs{m: map[string][]byte{}}
	content := []byte("quarterly report")
	sum := sha256.Sum256(content)
	key := hex.EncodeToString(sum[:])
	if err := blobs.PutReader(ctx, store, key, bytes.NewReader(content), int64(len(content))); err != nil {
		log.Fatal(err)
	}
	got, _ := store.Get(ctx, key)
	_, err := store.Get(ctx, "missing")
	fmt.Printf("blob %s...: %q; missing: %v\n", key[:8], got, err)

	// 3. sandbox: a Manager over a Backend gives each thread its sandbox.
	mgr := sandbox.NewManager(sandboxtest.NewBackend(), &threadStore{sb: map[string]string{}}, nil,
		sandbox.ManagerOptions{MaxPerUser: 5})
	sb, err := mgr.Acquire(ctx, "thread-1", "alice")
	if err != nil {
		log.Fatal(err)
	}
	if err := sb.WriteFile(ctx, "/workspace/notes.txt", []byte("hi")); err != nil {
		log.Fatal(err)
	}
	data, size, _ := sb.ReadFile(ctx, "/workspace/notes.txt", 0)
	again, _ := mgr.Acquire(ctx, "thread-1", "alice")
	fmt.Printf("read %q (%d bytes); same sandbox on the second Acquire: %v\n", data, size, again.ID() == sb.ID())
	_, _, err = sb.ReadFile(ctx, "/workspace/none", 0)
	fmt.Println("missing file is ErrNotFound:", errors.Is(err, sandbox.ErrNotFound))
}
```

Output (stdout; the Manager also logs `sandbox created thread_id=thread-1 ...` through `slog` to stderr):

```text
well-formed run: <nil>
broken run: agui order: event 2: RUN_FINISHED while spans are still open: [TEXT_MESSAGE_START/m1]
blob 07da4984...: "quarterly report"; missing: blob not found
read "hi" (2 bytes); same sandbox on the second Acquire: true
missing file is ErrNotFound: true
```

The same program built with `GOWORK=off` against the published `core v0.1.0` prints the same.

### Events: agui

An `agui.Event` is a JSON object (`map[string]any`) with a `type`. The constructors (`RunStartedEvent`, `RunFinishedEvent`, `RunErrorEvent`, `CustomEvent`, `SteerEvent`, `SubagentEvent`, `New`) build the events an agent emits and the constants name the types (`RunStarted`, `TextMessageContent`, `ToolCallResult`, `CustomUsage`, ...). `agui.CheckOrder(events)` verifies one run's log the way strict AG-UI clients do: `RUN_STARTED` first and once, at most one terminal event (`RUN_FINISHED` or `RUN_ERROR`), every `*_START` closed by its `*_END` with the same id, nothing open at `RUN_FINISHED`. A log without a terminal event (a run in progress) is valid when what it holds is ordered; `CheckOrderJSON` takes raw JSON. Write a front end by decoding these events; the catalogue of what a run emits is on [Event types](/docs/reference/events/) and the concepts on [Threads, runs and events](/docs/concepts/threads-runs-events/).

### Ports: store and blobs

`core/store` defines the persistence interfaces the engine depends on: `store.Store` (threads, runs, messages, events, claim and heartbeat of runs) and the capability interfaces (artifacts, skills, approvals, memory, notifications) that together make `store.Full`, which the memory, file and Postgres stores implement. To write your own store, implement `Store` and the capability interfaces you need, and run it under the harness through `harness.Options.Store`; [The store](/docs/concepts/store/) explains the model and [Stores](/docs/integrations/stores/) the shipped ones. `core/blobs.Blobs` is the content store behind artifacts (`Put`, `Get`, `Has`, `Delete`, `MaxBytes`); keys are content hashes, `blobs.ErrNotFound` and `blobs.ErrTooLarge` are its errors, and `blobs.PutReader` streams when the implementation supports it.

### The sandbox contract

`core/sandbox` is the contract for the place an agent runs commands. `sandbox.NewManager(backend, store, coordinator, options)` builds the lifecycle around any `Backend`: one sandbox per thread created on first use, a per-user quota, health checks and recreation. Its `Store` is the small part of a thread store it needs (the sandbox id recorded per thread, the count of a user's sandboxes), shown as `threadStore` above; the coordinator is optional (`nil` keeps locks in the process). `sandboxtest` provides the in-memory backend used here and `RunConformance` for proving a real one. The whole procedure of writing a backend is [Write a sandbox backend](/docs/guides/sandbox-backend/), and the concept is [Sandboxes](/docs/concepts/sandboxes/).

## modelinfo: windows and prices

`modelinfo` reads context window, output limit, capabilities (vision, reasoning, tools) and per-token prices from [LiteLLM's public price table](https://github.com/BerriAI/litellm). The program loads a local table (so it runs offline) and prices a request.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/7ar-chat/harness/modelinfo"
)

const prices = `{
  "acme-large": {
    "litellm_provider": "acme",
    "mode": "chat",
    "max_input_tokens": 200000,
    "max_output_tokens": 16000,
    "input_cost_per_token": 3e-06,
    "output_cost_per_token": 1.5e-05,
    "cache_read_input_token_cost": 3e-07,
    "supports_vision": true,
    "supports_function_calling": true
  },
  "acme-free-preview": {
    "litellm_provider": "acme",
    "mode": "chat",
    "max_input_tokens": 32000
  }
}`

func main() {
	dir, _ := os.MkdirTemp("", "prices")
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "prices.json")
	if err := os.WriteFile(file, []byte(prices), 0o644); err != nil {
		log.Fatal(err)
	}

	// File reads a local table instead of fetching LiteLLM's: no network, no cache.
	table, err := modelinfo.Load(context.Background(), modelinfo.Options{File: file})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("models in the table:", table.Len())

	info, ok := table.Lookup("acme-large", "acme") // the second argument is a provider hint
	fmt.Println("found:", ok, "window:", info.ContextWindow, "max output:", info.MaxOutput, "vision:", info.Vision, "tools:", info.Tools)

	// 12k prompt tokens (9k of them served from the cache) and 800 answer tokens.
	usd, priced := modelinfo.Cost(info, modelinfo.Usage{In: 12_000, Out: 800, Cached: 9_000, Prompt: 12_000})
	fmt.Printf("cost: $%.5f priced: %v\n", usd, priced)

	// Unknown is not free: a model without a price reports priced=false.
	free, _ := table.Lookup("acme-free-preview", "acme")
	usd, priced = modelinfo.Cost(free, modelinfo.Usage{In: 1000, Out: 1000})
	fmt.Printf("no price: $%.2f priced: %v\n", usd, priced)

	// Matching is conservative: an unrelated id never matches.
	_, ok = table.Lookup("no-such-model", "acme")
	fmt.Println("no-such-model found:", ok, "| fallback window:", modelinfo.DefaultContextWindow)
}
```

Output:

```text
models in the table: 2
found: true window: 200000 max output: 16000 vision: true tools: true
cost: $0.02370 priced: true
no price: $0.00 priced: false
no-such-model found: false | fallback window: 128000
```

The cost is 3,000 uncached input tokens at $3 per million, 9,000 cached tokens at the cache-read price, and 800 output tokens.

| Piece | Behaviour |
|---|---|
| `modelinfo.Load(ctx, Options)` | fetches the table (`DefaultURL`, or `Options.URL`), keeps a cached copy in `CacheDir` (used as is for `TTL`, default 24 hours) |
| `Options.File` | a local table read instead of the URL, never cached |
| `Options.Offline` | no request: the cache, however old, is all there is |
| `Options.Override` | your own file in the same schema; its models win over the table's, so private or self-hosted models can be priced |
| `modelinfo.LoadAsync` | returns a table at once and fills it in the background; `Table.Ready()` says when |
| `Table.Lookup(id, providerHint)` | exact id, then provider-prefixed, then the same name under another provider or a dated release of the same model. Never across unrelated models: a wrong price is rarer than a missing one; `ok` is false when nothing matches |
| `modelinfo.Cost(info, Usage)` | dollars; `priced` is false when the table has no price, and zero then means unknown, not free. Cached tokens use the cache-read price when the table has one; models with a higher price above a prompt size (`Info.Tier`) use it when `Usage.Prompt` crosses the threshold |
| `modelinfo.Parse(data)` | reads the LiteLLM table (a JSON object of model id to specification); skips `sample_spec` and non-chat models |
| `DefaultContextWindow` | 128000, what a caller can fall back on for an unknown model |

<div class="callout note">

**The table is data, not authority.** Check prices that matter against the provider. Cache-write tokens are not priced by the harness's cost display, and sub-agent tokens only join the total when the run ends.

</div>

### In the harness: `HARNESS_MODEL_PRICES`

The harness binaries load the table in the background (a plain GET, nothing about you is sent, never blocking startup) and show it as the context-window badge, price per million tokens and session cost. Two settings control the source:

| Setting | Meaning |
|---|---|
| `HARNESS_MODEL_PRICES` | a file path or an `http(s)` URL replacing the default source (LiteLLM's table); `off` disables the table |
| `~/.har/model-prices.json` | an override file in the same schema; its models win, so a private or self-hosted endpoint can be priced |

The cache lives in `~/.har/cache/`. In code, `config.ModelPriceOptions()` returns the `modelinfo.Options` these settings produce and `ok=false` when the table is disabled. `hosts/client` converts a `modelinfo.Info` to its wire type with `client.FromModelInfo`, which is how a remote UI shows the same prices ([Use hosts/client and hosts/tui](/docs/guides/use-client-tui/)). The variable is also listed in [Environment variables](/docs/reference/env-vars/).
