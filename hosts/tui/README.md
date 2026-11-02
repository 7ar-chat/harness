# hosts/tui

A terminal front end for an agent, written with [Bubble Tea](https://github.com/charmbracelet/bubbletea): streaming
answers rendered as Markdown, tool calls (expandable), diffs, approvals and questions, plans, reasoning, a status line with
tokens and cost, model switching, input history and key bindings you can change.

```
go get github.com/7ar-chat/harness/hosts/tui
```

```go
be := &tui.ClientBackend{
    C:           client.HTTP("https://agent.example.com", token), // any client.Client
    ThreadID:    thread.ID,
    ModelID:     "gpt-4o",
    Interactive: true, // risky tool calls wait for approval
}
err := tui.Run(ctx, be, tui.Options{StateDir: stateDir})
```

It imports only [`hosts/client`](../client) and `core/agui`, so it works the same against a remote server and an engine in
your process. `tui.Run` is the interactive UI. `tui.RunPrint(ctx, be, "prompt", tui.PrintOptions{...})` is the
non-interactive mode for scripts: the answer on stdout, tool activity on stderr, and an exit code (`ExitOK`, `ExitFailed`,
`ExitInterrupted` when the run waits for the user); `PrintOptions.JSON` prints the events as JSON lines instead.

To drive something other than a `client.Client`, implement `tui.Backend` (Send, Events, Steer, Cancel, Resume).
The render functions (`RenderMarkdown`, `RenderDiff`, `RenderToolCall`, ...) are exported and pure, for reuse.

Settings come from a `tui.json` (`Options.ConfigData`), and UI state (theme, recent models, history) is kept in
`Options.StateDir`; an empty `StateDir` keeps it in memory only.

## License

MIT. See [LICENSE](LICENSE).
