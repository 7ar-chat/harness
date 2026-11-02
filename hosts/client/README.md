# hosts/client

The contract a front end is written against. A `Client` acts for one user and offers what a conversation UI needs:
threads, runs, a resumable event stream, steering, cancelling, answering an approval or a question, and a thread's files.

```
go get github.com/7ar-chat/harness/hosts/client
```

```go
c := client.HTTP("https://agent.example.com", token) // a remote harness server

th, _ := c.CreateThread(ctx, "")
run, _ := c.Send(ctx, th.ID, "Summarise the report", client.SendOptions{})
events, _ := c.Events(ctx, run.ID, 0) // AG-UI events; reconnects and resumes without a gap or a duplicate
for ev := range events {
    // ev.Type: TEXT_MESSAGE_CONTENT, TOOL_CALL_START, RUN_FINISHED, ...
}
```

## Two implementations, one behaviour

| | |
|---|---|
| `client.HTTP(baseURL, token)` | speaks the JSON + Server-Sent Events API of a harness server (this module) |
| `inproc.New(h, user)` | drives an engine in the same process (`hosts/inproc`, in the main module) |

Both pass the same conformance suite (`hosts/clienttest`), so a front end written against `Client` behaves the same on
either: a terminal UI ([`hosts/tui`](../tui)), a Slack bot, a desktop app. Swap one line to move from embedded to remote.

## Errors and optional operations

Errors are `*client.Error` values with an HTTP status and a code; test them with `errors.Is(err, client.ErrNotFound)` and
the other `Err*` values. A user never sees another user's threads: they are reported as `ErrNotFound`. Operations a server
may lack (session history, slash commands, undo) are in `client.Extras` and return `ErrNotSupported` when absent.

The DTOs in this package are the wire format of the harness HTTP API: changing a JSON tag changes the API.

## License

MIT. See [LICENSE](LICENSE).
