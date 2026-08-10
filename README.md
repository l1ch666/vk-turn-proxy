# vk-turn-proxy

`vk-turn-proxy` carries local UDP or TCP traffic through a TURN allocation and
an end-to-end DTLS connection to a server with a fixed backend. It is useful
when the client can reach a TURN service but cannot connect to the backend
directly.

Canonical repository and Go module:
`github.com/l1ch666/vk-turn-proxy/v2`.

## How it is arranged

```text
local application
       |
       | UDP, or TCP in -vless mode
       v
vk-turn client -> TURN relay -> DTLS -> vk-turn server -> fixed backend
```

The project has three data-plane modes:

- default mode forwards local UDP packets to a UDP backend;
- `-vless` accepts local TCP connections, multiplexes them with smux, and
  opens one TCP backend connection per stream;
- `-vless -vless-bond` combines multiple TURN/DTLS paths below KCP and smux so
  one TCP stream can use the bonded path set.

Despite the flag name, the proxy does not parse VLESS requests. The mode is a
raw TCP forwarder intended to sit in front of a fixed VLESS/Xray-compatible
backend.

The client uses TURN over TCP by default. `-udp` changes the client-to-TURN
transport to UDP; it does not change a VLESS-mode TCP payload into UDP.

## Build

The module declares the minimum supported Go patch in `go.mod`. Use that
version or newer within the same supported Go release line.

```sh
mkdir -p bin
go build -trimpath -o bin/vk-turn-client ./client
go build -trimpath -o bin/vk-turn-server ./server
go build -trimpath -o bin/vk-turn-bench ./cmd/vk-turn-bench
```

Use `./bin/vk-turn-client -h` and `./bin/vk-turn-server -h` for the complete,
version-specific flag list. Unsupported legacy compatibility flags are retained
only to produce a clear startup error; they never silently select a different
transport.

## Minimal examples

Start the server near the fixed backend. Without `-vless`, `-connect` is a UDP
backend:

```sh
./bin/vk-turn-server \
  -listen 0.0.0.0:56000 \
  -connect 127.0.0.1:51820 \
  -client-auth-token-file ./client-auth-token
```

Copy the `DTLS server certificate SHA-256 fingerprint` from the server log over
an authenticated channel, together with the generated `client-auth-token`
file. The token is created with mode `0600` and its value is never logged. For
a stable identity across restarts, configure the server with `-dtls-cert-file`
and `-dtls-key-file` as described in
[docs/DTLS_IDENTITY.md](docs/DTLS_IDENTITY.md). Client authentication and
migration overrides are documented in
[docs/CLIENT_AUTH.md](docs/CLIENT_AUTH.md).

Start the client with a VK conference invite link:

```sh
./bin/vk-turn-client \
  -listen 127.0.0.1:51820 \
  -peer SERVER_IP:56000 \
  -dtls-server-fingerprint 'SHA256_FINGERPRINT_FROM_SERVER' \
  -client-auth-token-file ./client-auth-token \
  -vk-link 'https://vk.com/call/join/REDACTED' \
  -n 1
```

Add `-udp` on the client to use TURN over UDP.

Yandex Telemost is no longer supported: the provider closed the anonymous
conference path this proxy relied on, so that code was removed rather than left
in place as a broken option. The history before this change still contains it.

Default UDP mode is intentionally shown with one path. Several independent
UDP/DTLS paths currently create separate backend UDP associations and can make
a single WireGuard peer roam between them, so the client clamps non-VLESS
`-n` values to one. The `-unsafe-udp-multipath` flag restores the legacy
behavior only for controlled experiments. Use multiple paths with
`-vless -vless-bond` until UDP paths share one authenticated logical session.
The required protocol and rollout constraints are recorded in
[docs/UDP_MULTIPATH.md](docs/UDP_MULTIPATH.md).

For independent TCP sessions, add `-vless` to both processes and point the
server at a TCP backend. For packet-level bonding, add both `-vless` and
`-vless-bond` to both processes. The bond control protocol and deployment order
are documented in [docs/BOND_PROTOCOL.md](docs/BOND_PROTOCOL.md).

### Desktop full-tunnel routing

Do not enable a desktop full-tunnel WireGuard profile until the proxy has
resolved its TURN endpoints. Otherwise the proxy's own control traffic can be
routed back into the tunnel and disconnect itself. Release archives include the
matching helper for the default UDP/WireGuard flow:

- Linux: `routes.sh`;
- macOS: `routes-macos.sh`;
- Windows: `routes.ps1`.

The client prints resolved TURN IPv4 addresses on standard output, so start it
and feed that output to the helper before enabling WireGuard. For example:

```sh
./vk-turn-client <client flags> | ./routes.sh
```

The Linux and macOS helpers request elevated route privileges when needed.
Review a helper before running it and keep the terminal open while the client
is active.

## Diagnostics and benchmarks

Diagnostics are disabled by default. When enabled, they are restricted to a
literal loopback listener and require a 64-hex Bearer token. See
[docs/DIAGNOSTICS.md](docs/DIAGNOSTICS.md) for token handling, metrics, and
protected pprof endpoints.

The reproducible benchmark runner records raw iperf3 JSON, authenticated metric
snapshots, counter deltas, and receiver-side medians. Setup and comparison rules
are in [docs/BASELINE.md](docs/BASELINE.md).

The active engineering roadmap is tracked in
[docs/IMPROVEMENT_PLAN.md](docs/IMPROVEMENT_PLAN.md).

## Container

The image runs as an unprivileged user. By default it atomically creates the
combined DTLS identity `/var/lib/vk-turn/dtls-server-identity.pem` and the
client token in the same persistent directory. Keep that directory on a named
or bind-mounted volume; otherwise replacing the container changes the
fingerprint and pinned clients will correctly reject it. Custom
`DTLS_CERT_FILE` and `DTLS_KEY_FILE` values must always be supplied together.

The image ships only `vk-turn-server`, whose entire linked dependency graph
passes the third-party license audit, so building and running it is
unrestricted:

```sh
docker build -t vk-turn-proxy:local .
```

Publishing to a registry additionally requires the human approval switch
described in [docs/RELEASE_LEGAL.md](docs/RELEASE_LEGAL.md). Release *archives*
remain blocked by client-only dependencies, so do not assume that an existing
`latest` tag contains this v2 code.

```sh
docker volume create vk-turn-state
docker run --name vk-turn-server \
  --restart unless-stopped \
  --read-only \
  --cap-drop ALL \
  --tmpfs /tmp \
  -p 56000:56000/udp \
  --add-host host.docker.internal:host-gateway \
  -v vk-turn-state:/var/lib/vk-turn \
  -e CONNECT_ADDR=host.docker.internal:51820 \
  ghcr.io/l1ch666/vk-turn-proxy:latest
```

Set `VLESS_MODE=true` for independent raw-TCP sessions or
`VLESS_BOND=true` for bonded sessions. Extra command-line flags are passed
through to the server after the safe defaults. On the first start, copy the
logged certificate fingerprint and `/var/lib/vk-turn/client-auth-token` over
an authenticated channel (for example,
`docker cp vk-turn-server:/var/lib/vk-turn/client-auth-token ./client-auth-token`
followed by `chmod 600 ./client-auth-token`).

## Security status

- Treat conference invite links and diagnostics tokens as secrets; do not put
  them in benchmark notes, service files, or committed configuration.
- The diagnostics listener must remain loopback-only. Use an authenticated SSH
  tunnel when remote access is necessary.
- The server admits at most 256 active DTLS transports and 64 per source IP by
  default. Tune `-max-connections` and `-max-connections-per-ip` together when a
  deployment legitimately needs more bonded paths.
- VLESS forwarding is also bounded to 1024 active backend streams globally and
  256 per smux session by default. Tune `-max-backend-connections` and
  `-max-streams-per-session` together for larger deployments.
- The client requires the server certificate's pinned SHA-256 fingerprint by
  default and fails the DTLS handshake if it changes. The explicit
  `-dtls-insecure-skip-verify` migration override restores the old unsafe
  behavior and must not be used in a normal deployment.
- DTLS pinning authenticates the server, and a 256-bit token inside that pinned
  encrypted channel authenticates clients before any proxy payload is
  accepted. The token is shared, not a per-device identity: protect it like a
  private key, rotate it after suspected disclosure, and keep connection
  limits/firewall rules enabled. The explicit `unsafe-*` migration flags
  disable one side of authentication and must not be used normally.
- Manual captcha proxying is loopback/Host restricted, limits request and
  response sizes, and only permits HTTPS destinations under an explicit VK
  domain allowlist. Treat `-debug` output as sensitive even though known token
  and credential payloads are redacted.
- Release candidates are quality-gated and prepare `LICENSE`, `NOTICE`,
  generated third-party license texts, and `SHA256SUMS`; container builds
  request SBOM and provenance attestations. Public release and image jobs fail
  closed unless `RELEASE_LEGAL_APPROVED=true`, and the known dependency-license
  blockers are documented in
  [docs/RELEASE_LEGAL.md](docs/RELEASE_LEGAL.md). Artifact signing remains a
  roadmap item.

## Development checks

Before committing a behavior change, run:

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
go build ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
```

Performance claims additionally require at least three comparable runs and a
median result from the documented benchmark matrix.

## Provenance and releases

Buildable commands come from `./client`, `./server`, and
`./cmd/vk-turn-bench`. Release tags use the `v2.x.y` scheme, matching the
`/v2` Go module path, and prepare target-specific archives under those command
names. Linux candidates also retain raw `server-linux-*` compatibility assets
for the Android server installer. Repository licensing is in [LICENSE](LICENSE);
upstream attribution and retained per-file notices are summarized in
[NOTICE](NOTICE). Publishing remains blocked until the release legal gate is
approved.
