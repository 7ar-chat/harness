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

## Verified build

Built and smoke-tested on 2026-10-30 (Docker/OrbStack on macOS arm64) with `docker build` for
`linux/arm64` (native) and `linux/amd64` (emulated, the Dokploy server's likely architecture); the
Dockerfile has nothing architecture-specific (both base images are multi-arch, packages come from
Debian and PyPI/npm wheels).

| | linux/arm64 | linux/amd64 |
|---|---|---|
| Image size (`docker image ls` content size) | 690 MB (2.79 GB unpacked in the local image store) | 711 MB |
| Base | `python:3.11-slim-bookworm` + Node 22 copied from `node:22-bookworm` | same |

`docker run` assertions that passed on both: `/workspace` is the working directory and writable
(the image runs as root, as OpenSandbox starts it); `rg`, `git`, `tar`, `gzip`, `python3`, `node`,
`npm`, `soffice`, `pdftoppm`, `pdftotext`, `pandoc`, `qpdf`, `ffmpeg`, `curl`, `file`, `unzip`,
`column`, `setsid`, `pkill`, `find` are on `PATH`; GNU `realpath -m -z`, `mv -f -T`, `stat -c`
and `base64 -w0` behave as the `place`/`realpath`/`read-files` protocol scripts need; Python
(`pypdf`, `markitdown`, `python-pptx`, `python-docx`, `openpyxl`, `pandas`, `matplotlib`) and Node
(`pptxgenjs`, `docx`, `sharp`) imports work; a generated `.pptx` converts to PDF with headless
LibreOffice and renders with `pdftoppm`. Not exercised: running under a live OpenSandbox
lifecycle server (`opensandbox.toml`) and the `certs/` path with a real extra CA.

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
