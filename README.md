# bunk

**Your containers bunk on a friend's machine.**

`bunk` lets you run containers on machines you trust — family, teammates, a
friend across town — from anywhere, using the tools you already know. No
static IPs, no port forwarding, no VPN config, no learning a new tool: you
keep typing `docker run`, `docker compose up`, `psql` — they just run on
someone else's hardware.

```
YOUR HOME            BUNK-HUB (any always-on box)        BROTHER'S HOME
┌──────────────┐     ┌──────────────────────────┐       ┌──────────────────┐
│ any docker   │     │  blind E2E relay         │       │ bunk + Docker    │
│ tool/CLI/IDE │◄───►│  pairing codes, keys,    │◄─────►│ (GPU toolkit)    │
│ (unchanged)  │ E2E │  ACLs, revocation        │  E2E  │                  │
└──────────────┘     └──────────────────────────┘       └──────────────────┘
   dial-out only — works behind any NAT/CGNAT, no inbound ports anywhere
```

The hub is a **blind relay**: traffic between you and the other machine is
end-to-end encrypted (X25519 + XSalsa20-Poly1305), so whoever runs the hub
cannot read your containers, data, or commands — they only see that two
machines are talking.

## What you get

- **Zero learning.** The only new commands are `bunk pair` and `bunk use`.
  Everything else is docker. `bunk run postgres:16` runs on the linked
  machine with sane limits; `docker compose up` works too.
- **Safe by default.** CPU/RAM/pids limits are injected unless you specify
  your own (`--cpus 6 --memory 12g --pids-limit 256` by default, configured
  in `~/.bunk/config.yaml`). GPU passthrough is automatic on Nvidia hosts
  (`--gpus all`), CPU-only elsewhere, and `BUNK_GPU=off` disables it.
- **Seamless ports.** `bunk run -p 5432:5432 postgres` → `localhost:5432`
  on *your* machine just works (auto-forwarded over the tunnel), and the
  forward closes automatically when the container stops. For
  already-running services: `bunk forward <local>[:<remote>]`. Set
  `BUNK_NO_AUTO_FORWARD=1` to skip auto-forwarding and manage ports
  manually.
- **Interop.** Every linked machine gets a docker context (`bunk-<name>`)
  and a local proxy socket, so Docker Desktop "connect", Portainer,
  Lazydocker, devcontainers and IDE extensions work unchanged.
- **Consent + revocation.** The host generates the pairing code and each
  link is explicit. `bunk revoke <machine>` cuts access immediately — the
  hub stops routing that pair.

## Install (two machines, ~5 minutes)

Prerequisites: Go 1.22+ on each machine (to build), Docker on the machine
you want to share, and a reachable box for the hub (a $4 VPS, an old PC,
a free Oracle Cloud instance — anything always-on).

```bash
# 1. build (on any machine; binaries are static)
go build -o bunk ./cmd/bunk
go build -o bunk-hub ./cmd/bunk-hub

# 2. run the hub (one time, on the always-on box)
export BUNK_HUB_TOKEN="$(openssl rand -hex 32)"   # shared secret
./bunk-hub -addr :8080 -db bunk-hub.db             # put Caddy/nginx TLS in front for wss://

# 3. on the SHARED machine (e.g. brother's): install the agent
export BUNK_HOME="$HOME/.bunk" BUNK_HUB="wss://hub.example.com/ws" BUNK_HUB_TOKEN="$TOKEN"
./bunk start                                       # background daemon
./bunk pair --name brother-box                     # → prints a one-time code

#    Windows host? Docker Desktop + WSL2 Ubuntu, then the one-line installer:
#    https://github.com/root-explorer/bunk/blob/main/docs/windows-host.md

# 4. on YOUR machine: same, then redeem the code
export BUNK_HOME="$HOME/.bunk" BUNK_HUB="wss://hub.example.com/ws" BUNK_HUB_TOKEN="$TOKEN"
./bunk start
./bunk pair <CODE> --name me

# 5. done — now run things on the other machine
./bunk use brother-box
./bunk run --rm nvidia/cuda:12.0 nvidia-smi        # his GPU, your terminal
./bunk run -d -p 5432:5432 postgres:16             # and localhost:5432 is yours
./bunk machines                                    # who's online
```

`bunk enable-shim` optionally installs a `docker` shim into `~/.bunk/bin`
so plain `docker` (no `bunk` prefix) routes to the active machine; remove
it with `bunk disable-shim`. `bunk --dry-run run ...` shows the exact
docker command before running (`BUNK_SHOW_CMD=1` prints it while running).

## CLI reference

```
bunk pair [code] [--name NAME]   one-time linking (host: no code = create one)
bunk use <machine> [--unset]     point docker at that machine (or go local)
bunk unuse                     alias for 'bunk use --unset'
bunk machines                    list linked machines + status
bunk forward <local>[:<remote>]  expose a remote service port on localhost
bunk unforward <port>            close a forward
bunk forwards                    list active forwards
bunk revoke <machine>            cut a link immediately
bunk status                      daemon + hub status
bunk start | stop | restart      manage the background daemon
bunk enable-shim | disable-shim  make plain 'docker' route to the active machine
bunk install-idle-gate           pause containers while the owner is active
bunk --dry-run run <image> ...   show the exact docker command
```

Any other subcommand is passed straight to docker on the active machine:
`run`, `exec`, `logs`, `ps`, `stop`, `rm`, `cp`, `build`, `push`, `pull`,
`compose`, `inspect`, ... — identical semantics, plus safe defaults on
`run`.

## Config

`~/.bunk/config.yaml` (per machine; `BUNK_HOME` overrides the directory):

```yaml
hub: wss://hub.example.com/ws
token: <BUNK_HUB_TOKEN>
serve_docker: true          # expose this machine's docker to linked peers
gpu: auto                   # auto | off
limits:
  cpus: 6
  memory_gb: 12
  pids: 256
idle_gate:
  enabled: false            # see below
  idle_seconds: 15
  threshold_ms: 30000
```

The limits above are the *caps for what you may ask of any host*; injected
defaults are then **clamped to each host's real capacity** (reported at link
time), so a 2-core/1GB box only ever gets asked for 2 CPUs / 1GB, and a
32-core rig gets the full defaults. User-supplied flags always win.

## Idle gate (optional "respect the host" add-on)

`bunk install-idle-gate` enables a small loop on the *shared* machine:
when its owner is actively typing/mousing (X11 + `xprintidle` required),
containers labeled `bunk.idle-gate=1` are paused, and resumed when the
machine goes idle again. Off by default — v1's default respect is the
resource limits plus an explicit agreement.

## Security model (be honest about it)

- **E2E encrypted, hub blind.** Frames are sealed with `nacl/box` using
  keys exchanged at pairing; the hub routes ciphertext it cannot read and
  cannot forge (sealing authenticates the sender).
- **Pairing codes are one-time** and expire after 24h; each link is an
  explicit ACL between two machines; revocation deletes the ACL at the hub.
- **No exposed ports.** Every machine dials out; nothing listens on a
  public interface. The hub is the only reachable point and it only speaks
  the blind-relay protocol.
- **What you must accept:** an authorized peer gets remote container
  execution on the shared machine — that's the deal. Keys are revocable
  and the docker socket is only reachable through the authenticated
  tunnel, but trust in your peers is the model. Use it with people you
  trust (the plan's "trust-groups" scope), not strangers.
- **Known limits (planned v2):** static-ECDH (no forward secrecy), relay
  bandwidth for heavy transfers (P2P hole-punching planned), no TEE-based
  confidential compute.

## How it works (30 seconds)

1. Each machine runs a `bunk` daemon that dials the hub and registers its
   identity + public key.
2. Hosts mint one-time pairing codes; guests redeem them; the hub records
   a bidirectional ACL.
3. `bunk use brother-box` starts a local TCP proxy (`127.0.0.1:<port>`)
   and registers docker context `bunk-brother-box` pointing at it.
4. Every docker API call hits the local proxy, is chunked into sealed
   frames, relayed through the hub to the peer, and replayed into the
   peer's docker daemon socket — then the response tunnels back.
5. `-p` publishes are mirrored as local port forwards while the container
   runs; `bunk forward` does the same for existing services.

The relay is the "wire"; docker is the runtime; `bunk` is a transparent
layer, not a reimplementation. (Network caveat: the local proxy is plain
HTTP on loopback only, so use it on the same machine — never expose
`state.json`'s ports.)

## Development

```bash
go test ./...          # crypto, shim injection, hub pairing/relay/revoke
./scripts/smoke.sh     # full E2E: hub + 2 agents + real docker + port forward
```

## Roadmap (trust-groups, v2+)

Orgs/teams + roles, host-side policies (per-guest limits, image
allowlists, time windows), audit logs, P2P hole-punching, a management
dashboard/mobile grant-revoke, CI/runner and devcontainer integrations,
one-click installers. Explicitly **not** planned: open marketplace,
compute payments, crypto — the graveyard of every decentralized-compute
project before this one.

## License

MIT (see LICENSE). `bunk` is a hobby project — the sibling story is real:
it exists so your containers can bunk at your brother's place.
