# Local OpenSandbox (docker-compose profile `work`)

Files: `Dockerfile` (sandbox image, `OPENSANDBOX_IMAGE`), `opensandbox.toml` (server config).

```
work-api / work-runtime ──► opensandbox-relay ──► opensandbox server ──► sandbox execd
   (compose net)            172.17.0.1:8090        127.0.0.1:18090        127.0.0.1:<40000-60000>
                            (host netns, socat)    (host netns)           (published on loopback only)
```

The OpenSandbox Docker runtime always publishes each sandbox's execd port, and the egress
sidecar only works on the default `bridge` network. So ports are published on host loopback
only, the server runs in the host network namespace, and a socat relay exposes the server on
the default-bridge gateway. Work services set `OPENSANDBOX_USE_SERVER_PROXY=true`.

Every sandbox gets a network policy from work-runtime (`SANDBOX_EGRESS_ALLOWLIST`, deny by
default, DNS + nftables). That policy is also what stops sandboxes reaching each other, the
host and compose services.

## Native Linux Docker (default, verified)

Nothing to set. `host.docker.internal` (`extra_hosts: host-gateway`) is the default-bridge
gateway, `172.17.0.1`, which the relay binds (`OPENSANDBOX_RELAY_BIND`). If
`docker network inspect bridge -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}'` prints another
IP (custom `bip`), set `OPENSANDBOX_RELAY_BIND=<that IP>` in the root `.env`.

## Docker Desktop (Mac / Windows; not verified)

`host.docker.internal` there is the desktop host (192.168.65.254), not the VM's bridge, and
`network_mode: host` refers to the Docker VM and is opt-in.

1. Enable host networking: Settings → Resources → Network → "Enable host networking" (4.34+).
2. In the root `.env`: `WORK_OPENSANDBOX_URL=http://172.17.0.1:8090`. Keep
   `OPENSANDBOX_RELAY_BIND=172.17.0.1`, the VM's bridge gateway.
3. If host networking is not available, run without sandboxes: `WORK_OPENSANDBOX_URL=`
   (empty). The sandbox tools are then disabled; the chat still works.

## Dev-only caveats

- The relay binds the bridge gateway IP, not `0.0.0.0`. Connecting through the host's LAN
  address is refused (`curl http://<eth0-ip>:8090` → connection refused). A LAN peer that
  deliberately routes 172.17.0.0/16 to this host could still reach it.
- Any container on the default bridge can reach the relay, not only the Work services; only
  the dev API key (`OPENSANDBOX_API_KEY`, default `dev-only-insecure-opensandbox-key`)
  protects it. Sandboxes with a network policy cannot reach it (blocked by nftables).
