# Front ends and the client contract

The engine is a library; a front end should not be written against it. `hosts/client` is the narrow contract
between the two, and the only thing a terminal UI, a web UI, a Slack bot or a desktop app needs to import:

```
engine/ features/ store/ sandbox/            the core (a library, no HTTP, no UI)
        ▲
   harness.Harness                           the facade
        ▲                       ▲
  inproc.New(h, user)   hosts/httpapi      the HTTP API server (JSON + SSE, bearer auth)
        │                       ▲
        │                client.HTTP(url, token)     Last-Event-ID reconnect with backoff
        └───────────┬───────────┘
                client.Client                the contract
              ▲      ▲       ▲
         hosts/tui   web UI   your Slack bot, desktop app, …
```

`Client` acts for one user and has: `Models`; `CreateThread`, `ListThreads`, `GetThread`, `RenameThread`,
`DeleteThread`; `Send` (returns the `Run`), `GetRun`, `Events(ctx, runID, after)` (AG-UI events with an ordered `Seq`,
resumable from any seq), `Steer`, `Cancel`, `Resume` (answers a tool approval or an `ask_user` question);
`ListFiles`, `DownloadFile`; `Sessions`. Errors are `*client.Error` values matched with `errors.Is` against
`client.ErrNotFound` (also what another user's thread or run gives), `ErrInvalidRequest`, `ErrRunNotActive`,
`ErrNotSupported`... The DTOs (`Thread`, `Message`, `Run`, `File`, `Session`, `SendOptions`, `ResumeRequest`) are the
JSON that `hosts/httpapi` serves: the server uses these very types, so the wire format has a single definition. The
run-creation request also takes an optional `sandbox` string, which the server forwards once the engine supports a
per-run sandbox.

Both implementations pass the same suite, `clienttest.Run(t, newClient)`: create, send, stream to `RUN_FINISHED`,
resume from a seq, steer, cancel, approval and `ask_user` round trips, files, validation and ownership.
A new implementation (a gRPC client, a recorded replay…) is tested by calling it.

A complete front end, in about 25 lines (`integration/client/example_test.go` compiles and runs it):

```go
func ask(ctx context.Context, c client.Client, question string) error {
	th, err := c.CreateThread(ctx, "")
	if err != nil {
		return err
	}
	run, err := c.Send(ctx, th.ID, question, client.SendOptions{})
	if err != nil {
		return err
	}
	events, err := c.Events(ctx, run.ID, 0)
	if err != nil {
		return err
	}
	for ev := range events {
		switch ev.Type {
		case agui.TextMessageContent:
			var e struct{ Delta string }
			_ = json.Unmarshal(ev.Data, &e)
			fmt.Print(e.Delta)
		case agui.RunFinished, agui.RunError:
			fmt.Println()
		}
	}
	return nil
}

// in-process:  ask(ctx, inproc.New(h, "alice", inproc.WithModels(cfg.LLMModels, "gpt-5")), "Say hello")
// remote:      ask(ctx, client.HTTP("http://127.0.0.1:8080", os.Getenv("HARNESS_REMOTE_TOKEN")), "Say hello")
```

A run that ends with `RUN_FINISHED` outcome `interrupt` is waiting for the user: read the interrupt (`reason`,
`id`) from the event, or the thread's `PendingApproval` / `PendingQuestion`, and answer with `Resume`. A stream
whose connection cannot be recovered ends with a synthetic `RUN_ERROR` (code `stream_lost`), so a consumer that
waits for a terminal event never hangs.

**Dependency rules** (`layers_test.go`; see [docs/architecture.md](architecture.md)): `core/` imports only `core/`;
`engine/` imports `core/` and `engine/`; features import `core/`, `engine/` and `coding/`, never another feature;
`adapters/` and `coding/` do not import each other; `hosts/tui` imports only `hosts/client` and `core/agui`;
`hosts/httpapi` imports only the facade, `hosts/client` and `core/`; `app/product/` builds on `hosts/httpapi`; the facade
imports no host.

