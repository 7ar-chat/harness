# adapters/sandbox

Sandbox backends for the [`core/sandbox`](../../core/sandbox) contract: where an agent's commands and file operations run.

```
go get github.com/7ar-chat/harness/adapters/sandbox
```

| Package | Backend | Isolation |
|---|---|---|
| [`local`](local) | a directory on this machine | **none**: commands run with the user's own rights; for a trusted developer machine |
| [`opensandbox`](opensandbox) | one container per conversation on an [OpenSandbox](https://github.com/alibaba/OpenSandbox) server | resource limits, an egress allow-list, optional credential proxying |
| [`e2b`](e2b) | one micro-VM per conversation on [E2B](https://e2b.dev) (REST API plus the envd agent; standard library only) | VM isolation, an egress allow-list; conformance suite passes against the live service |
| [`daytona`](daytona) | one sandbox per conversation on [Daytona](https://www.daytona.io) (official Go SDK) | provider isolation; conformance suite passes against the live service; egress follows the account tier (tiers 1 and 2: Daytona's own restricted policy) |
| [`backends`](backends) | the registry that selects one by name | |

```go
b, err := backends.NewBackend(backends.Settings{
    Name:  "local",
    Local: backends.LocalSettings{Workspace: "/path/to/project"},
})
mgr := sandbox.NewManager(b, store, coordinator, sandbox.ManagerOptions{MaxPerUser: 5}) // from core/sandbox
sb, err := mgr.Acquire(ctx, threadID, user) // created on first use, one per conversation
out, err := sb.Exec(ctx, sandbox.ExecRequest{ /* ... */ })
```

## Your own backend

Implement `sandbox.Backend` (`Create`, `Connect`, `Kill`) returning `sandbox.Instance` (`Exec`, `ReadFile`, `WriteFile`,
`MkdirAll`, `List`, `Stat`, `Healthy`, `Gone`, `Renew`). Prove it with `sandboxtest.RunConformance`, then register it:

```go
func init() { backends.Register("mybackend", func(s backends.Settings) (sandbox.Backend, error) { /* ... */ }) }
```

`Connect` must return `sandbox.ErrNoSandbox` only when the sandbox definitively no longer exists; any other error is
treated as transient and retried.

## License

MIT. See [LICENSE](LICENSE).
