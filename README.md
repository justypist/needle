# needle

A lightweight, unprivileged host probe. An agent reads `/proc` and `/sys` and
POSTs a small JSON report over HTTPS; a server keeps the history in SQLite and
serves a dark, minimal dashboard.

It exists because the available probes are heavy and over-privileged. needle
is built around three rules:

- **The agent only reads.** Nothing in it opens a file for writing or changes
  system state. It runs as an ordinary user with zero capabilities.
- **The server cannot command anything.** The agent opens no socket, accepts
  no input and polls no job queue. It only pushes. A fully compromised server
  still cannot run a command anywhere.
- **No features that trade weight for reach.** There is no web shell, no
  remote execution, no container control and no agent self-update.

## Layout

```
cmd/needle-client     the agent (static, ~6.6 MB, ~11 MB RSS)
cmd/needle-server     ingest + API + dashboard (static, ~11 MB, ~2 MB RSS)
internal/collect      /proc, /sys and statfs readers
internal/proto        the wire format the two share
internal/store        SQLite schema, tiered rollup, pruning
internal/server       HTTP handlers, agent-token and session auth
web/                  dashboard: vanilla ES2020 + vendored uPlot
scripts/              toolchain bootstrap and cross-compilation
deploy/               container deployment and the agent installer
.github/workflows/    tests, cross-compiled binaries, container image
```

## Build

The host needs only bash, curl and tar: Go is fetched into `.toolchain/` and
pinned by `scripts/bootstrap-go.sh`.

```shell
./scripts/build.sh              # host architecture
./scripts/build.sh --all        # every supported target
./scripts/build.sh --target linux-arm64
```

Binaries land in `dist/` as `needle-{server,client}-linux-<arch>`. They are
static (`CGO_ENABLED=0`), so the same file runs on Debian and on OpenWrt.

The agent builds for `amd64`, `arm64`, `arm` and `mipsle` — enough for the
Debian boxes and for a MIPS router. The server embeds a SQLite engine whose
32-bit port is incomplete, so it is only built for `amd64` and `arm64`; it is
meant to run as a container in any case.

## Server

The server ships as a container only. Images are built and published by
GitHub Actions; nothing is compiled on the deployment host.

| Image tag | Meaning |
| --- | --- |
| `ghcr.io/justypist/needle:latest` | newest build of `main` |
| `ghcr.io/justypist/needle:1.2.3` | a tagged release |
| `ghcr.io/justypist/needle:sha-abc1234` | one specific commit |

```shell
./deploy/install-server.sh --dest ~/.compose/needle --domain needle.dev.typist.cc
```

That writes a compose root — `compose.yaml`, `.env` with freshly generated
credentials, and the `backup.sh` / `restore.sh` pair from the `~/.compose`
convention — then pulls and starts it. Re-running it updates the compose file
and keeps the existing `.env`. `--no-start` assembles without starting, and
`--tag` pins a version instead of tracking `latest`.

The container runs from a `scratch` image as uid 65534, with `read_only: true`,
`cap_drop: ALL`, `no-new-privileges` and a writable `/data` volume as its only
filesystem. `NEEDLE_SECURE_COOKIES=true` marks the session cookie `Secure`,
so serve it behind TLS or logins will silently fail.

Caddy picks the service up from container labels (`caddy`, `caddy.import`,
`caddy.reverse_proxy`), so no reverse-proxy configuration is needed beyond a
caddy instance watching the `public` network.

| Environment variable | Meaning |
| --- | --- |
| `NEEDLE_ADDR` | listen address, default `:8080` |
| `NEEDLE_DB` | SQLite path, default `needle.db` |
| `NEEDLE_AGENT_TOKEN` | shared secret for `POST /report` (required) |
| `NEEDLE_PASSWORD` | dashboard password; empty disables dashboard auth |
| `NEEDLE_SESSION_SECRET` | cookie signing key; derived from the two above when unset |
| `NEEDLE_SECURE_COOKIES` | mark the session cookie `Secure` |

### Releases

Pushing to `main` publishes `latest` and `sha-<commit>`. Pushing a `vX.Y.Z` tag
additionally publishes `X.Y.Z` and `X.Y`. Both multi-arch (`amd64`, `arm64`)
and both the image and the bare binaries (as a workflow artifact) are built
from the same commit.

## Agent

```shell
./deploy/client/install.sh --url https://probe.example.com --token <token>
```

The installer copies the binary into place, then picks a supervision mechanism
by what the host actually has:

| Method | Detected when | Survives logout | Survives reboot |
| --- | --- | --- | --- |
| `systemd-user` | a working user manager | yes (with lingering) | yes |
| `procd` | OpenWrt / iStoreOS, as root | yes | yes |
| `tmux` | `tmux` is present | yes | no |
| `nohup` | nothing else | no | no |

`systemd-user` is preferred because it needs no root at all: the unit lands in
`~/.config/systemd/user/`, with its own `EnvironmentFile` at mode 600 and a
sandbox (`ProtectSystem=strict`, `ProtectHome=read-only`, `NoNewPrivileges`,
`MemoryDenyWriteExecute`, and the rest). The installer tries
`loginctl enable-linger` so the agent outlives logout, and tells you the
command if that needs sudo.

On OpenWrt there is no systemd, so it writes an `/etc/init.d/needle-client`
procd script and enables it.

```shell
./deploy/client/install.sh --url ... --token ... --method auto   # default
./deploy/client/install.sh --uninstall
```

The agent takes `--url`, `--token`, `--interval` (default `30s`) and `--once`,
and reads `NEEDLE_URL` / `NEEDLE_TOKEN` / `NEEDLE_INTERVAL` from the
environment. It caps its own heap at 32 MB.

## What is collected

Scalars per report: CPU total, memory and swap, load 1/5/15, uptime, process
count, network and disk throughput in bytes per second, and the highest
thermal-zone temperature. Alongside them, labelled series: per-core CPU,
per-interface rx/tx, per-device read/write, per-mount used/total, and per-zone
temperature.

Two deliberate choices keep the data honest on a container host:

- **Virtual interfaces are dropped.** `veth*`, `br-*`, `docker*`, `virbr*` and
  friends duplicate traffic the physical NIC already reports and would
  otherwise crowd it out of the per-report cap.
- **Overlay mounts are skipped.** A container host carries one per running
  container; monitoring them is noise.

Byte counters are converted to rates by the agent, which makes every stored
value a gauge. That is what lets a tier be collapsed with a plain average.

## History and rollup

Three tiers, rolled up on a timer and pruned along with them:

| Tier | Resolution | Retained |
| --- | --- | --- |
| 0 | as reported (raw) | 48 h |
| 1 | 5-minute averages | 30 d |
| 2 | hourly averages | 365 d |

The dashboard picks a tier from the requested span alone, so a narrower window
automatically gets finer data. Rollups only ever rewrite closed buckets, and
recomputing one from complete source data always yields the same value, so the
timer being late or the process restarting changes nothing.

Gauges are averaged within a bucket; monotonic counters and capacities take
their maximum, which is the end-of-bucket value. Each tier's source is queried
over the *source* tier's retention window, which keeps a cold start cheap
instead of rescanning all history.

## API

Every `/api/*` route needs a session cookie except `POST /api/login`.

| Route | Purpose |
| --- | --- |
| `POST /report` | agent ingest (Bearer agent token) |
| `POST /api/login` | exchange the password for a session cookie |
| `POST /api/logout` | clear it |
| `GET /api/session` | whether the session is valid, and whether auth is on |
| `GET /api/nodes` | all hosts with their latest sample and series |
| `GET /api/node?id=&range=&from=&to=` | one host plus a chart window |
| `GET /api/stats` | row counts |

`range` takes an ordinary duration and additionally understands days and
weeks: `1h`, `6h`, `24h`, `7d`, `30d`, `90d`, `365d`. `from`/`to` are
millisecond epoch and override it. Chart columns are keyed `metric` for
scalars, `kind:label` for a series, and `kind:label:b` for the second value of
a paired series (tx, write, total). A gap is JSON `null`, which uPlot draws as
a break in the line rather than a drop to zero.

## Dashboard

No build step and no framework. `web/` is served straight from the binary via
`go:embed` — `index.html`, `style.css`, `app.js` and a vendored copy of
uPlot. The node list refreshes every 5 s, the detail page every 30 s. Login
uses an HMAC-signed `HttpOnly` cookie; there is no user table and no session
store, and changing either credential invalidates existing sessions.

## Tests

```shell
./.toolchain/go/bin/go test ./...
```

They cover the parts where a mistake is silent: tier rollup arithmetic and
idempotence, open-bucket handling, pruning, gap filling, range parsing, and
the session and agent-token checks.

## Non-goals

Threshold alerting, notifications, multi-user access control, a public status
page, a node map, theme options, and any non-Linux support.
