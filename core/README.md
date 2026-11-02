# core

The vocabulary the rest of the harness agrees on: events, domain types, and the ports that implementations plug into.
It depends on one small module (uuid) and nothing else from this repository.

```
go get github.com/7ar-chat/harness/core
```

| Package | Holds |
|---|---|
| [`agui`](agui) | the [AG-UI](https://docs.ag-ui.com) events a run streams (run, step, text, reasoning, tool call, interrupt), usage, error codes, `CheckOrder` to verify a stream |
| [`sandbox`](sandbox) | the `Backend` / `Instance` / `Manager` contract for the place an agent runs commands, the background-job registry, and `sandboxtest` (an in-memory fake and a conformance suite) |
| [`store`](store) | threads, runs, messages, approvals, memory, artifacts, skills and the persistence interfaces that store them (`Store`, `Full`) |
| [`blobs`](blobs) | the port for content stored by key |
| [`layout`](layout) | the names of the folders a project and a user keep (`.har/...`) |

## When you would use it directly

- **Writing a sandbox backend.** Implement `sandbox.Backend`, run `sandboxtest.RunConformance` against it, and the harness
  (or your own agent through `sandbox.Manager`) can use it. See `adapters/sandbox` for two real ones.
- **Writing a store.** Implement `store.Store` (and the capability interfaces you need) over your database.
- **Writing a front end or an event consumer.** Decode and check `agui` events.

If you only want to call models and run a tool loop, use [`ai`](../ai) instead: it does not need this module.

## License

MIT. See [LICENSE](LICENSE).
