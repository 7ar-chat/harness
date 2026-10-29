# Sandbox definition

What the agent's tools run in: an [OpenSandbox](https://github.com/alibaba/OpenSandbox)
container per thread, created by the harness (`sandbox.OpenSandbox`, selected by
`OPENSANDBOX_URL`).

| File | Purpose |
|---|---|
| `Dockerfile` | The sandbox image (`OPENSANDBOX_IMAGE`): Office/PDF/data toolchain, Python and Node. Optional extra CA certificates in `certs/`, mirrors through build args (see the header of the file). |
| `opensandbox.toml` | Lifecycle server configuration for a local Docker runtime. |

Build and run locally:

```sh
docker build -t harness-sandbox:local .
# run the OpenSandbox lifecycle server with opensandbox.toml (see its docs), then:
export OPENSANDBOX_URL=http://127.0.0.1:18090
export OPENSANDBOX_IMAGE=harness-sandbox:local
```

## Isolation model

- The Docker runtime always publishes each sandbox's execd port on a host port, so the
  server publishes them on `127.0.0.1` only and sandboxes cannot reach host-loopback ports.
- Every sandbox is created with a network policy (`SANDBOX_EGRESS_ALLOWLIST`, deny by
  default, DNS + nftables via the egress sidecar). That policy is also what stops sandboxes
  reaching each other, the host and sibling containers: a sandbox created **without** a
  policy sits on the default bridge and can reach other sandboxes' execd.
- Containers run with dropped capabilities, `no-new-privileges` and a pids limit.

## Workspace layout (what the prompts and tools advertise)

```
/workspace            working directory
/workspace/outputs    final deliverables
/workspace/.work      scratch (never saved), tool-output spills, renders
/skills/<name>        installed skills (load_skill / lazy install)
```

The harness' own sandbox contract is the `sandbox.Sandbox` interface (Exec, ReadFile,
WriteFile, MkdirAll, List, Stat) — implement `sandbox.Backend` to run the tools on anything
else (a VM, Firecracker, a remote dev box). `core/sandbox/sandboxtest` is an in-memory fake.
