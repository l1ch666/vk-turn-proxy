# vk-turn-proxy

`vk-turn-proxy` carries local UDP or TCP traffic through a TURN allocation and
an end-to-end DTLS connection to a server with a fixed backend. It is useful
when the client can reach a TURN service but cannot connect to the backend
directly.

Canonical repository and Go module:
`github.com/l1ch666/vk-turn-proxy`.

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

The module declares Go 1.25.5.

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
  -connect 127.0.0.1:51820
```

Start the client with exactly one supported conference invite link:

```sh
./bin/vk-turn-client \
  -listen 127.0.0.1:51820 \
  -peer SERVER_IP:56000 \
  -vk-link 'https://vk.com/call/join/REDACTED' \
  -n 10
```

Use `-yandex-link` instead of `-vk-link` for a Yandex Telemost invite. Add
`-udp` on the client to use TURN over UDP.

For independent TCP sessions, add `-vless` to both processes and point the
server at a TCP backend. For packet-level bonding, add both `-vless` and
`-vless-bond` to both processes. The bond control protocol and deployment order
are documented in [docs/BOND_PROTOCOL.md](docs/BOND_PROTOCOL.md).

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
- DTLS encrypts the data plane, but pinned peer identity or PSK/mTLS
  authentication is not implemented yet. Do not assume protection against an
  active man-in-the-middle until that roadmap item is complete.
- Release signing, checksums, SBOM/provenance, and hardened service/container
  definitions are still roadmap items.

## Development checks

Before committing a behavior change, run:

```sh
gofmt -w <changed-go-files>
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

Performance claims additionally require at least three comparable runs and a
median result from the documented benchmark matrix.

## Provenance note

Buildable commands come from `./client`, `./server`, and
`./cmd/vk-turn-bench`; those names are the source of truth for future artifact
names. This local snapshot does not yet contain release automation or a
repository-wide license file. Source files retain their existing per-file SPDX
notices where present; repository-wide licensing and release provenance must be
verified before publishing binaries.
