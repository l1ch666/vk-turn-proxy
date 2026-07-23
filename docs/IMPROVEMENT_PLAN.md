# Stability and Performance Improvement Plan

Baseline: `main` at `1200c50810194a09cd2864d56d324e6b1caca0b8`.

The goal is to improve correctness, resilience, throughput, and operational safety without changing the core design: local UDP/TCP traffic is carried through TURN and end-to-end DTLS to a fixed backend on the VPS.

## Working rules

- Changes are split into small reviewable commits.
- Every behavior fix gets a regression test where practical.
- Formatting, unit tests, race tests, vet/lint, and builds must pass before a phase is marked complete.
- Performance work starts only after baseline metrics exist.
- Experimental transport changes remain opt-in until benchmarked.

## Phase 1: correctness and lifecycle

- [x] Make manual captcha waiting context-aware and always release loopback listeners.
- [x] Apply TURN authentication-cache invalidation consistently in UDP and VLESS modes.
- [x] Treat fatal captcha errors consistently in all connection maintainers.
- [x] Use unique zero-based stream IDs in the plain UDP dispatcher.
- [x] Prevent stale TURN readers from consuming packets after reconnect.
- [x] Close DTLS and packet-pipe resources after failed handshakes.
- [x] Make bidirectional proxy, smux, KCP, and bond shutdown bounded.
- [x] Preserve TCP half-close in both directions with a smux version that supports `CloseWrite`.

## Phase 2: bond correctness

- [x] Supervise and recreate the complete bond/KCP/smux session after failure.
- [x] Negotiate protocol version, expected path count, MTU, and FEC configuration.
- [x] Scale the server KCP window from the negotiated path count.
- [x] Validate all KCP, FEC, MTU, and smux configuration at startup.
- [x] Keep a bond generation alive through a bounded zero-path recovery grace period.

## Phase 3: observability and baseline

- [x] Count active paths/sessions, reconnects, authentication failures, and queue drops.
- [x] Record bytes, write latency, and errors per path.
- [x] Export KCP retransmission and FEC recovery counters.
- [x] Add an opt-in loopback diagnostics endpoint and protected pprof support.
- [ ] Establish reproducible `iperf3` baselines for TCP/UDP TURN, multi-session/bond, upload/download, and one/many flows.
  - [x] Add a deterministic matrix runner, authenticated metric snapshots, raw artifacts, median summaries, and an operator runbook.
  - [ ] Capture all four profiles on a live deployment with `iperf3` installed and retain the reports as the numeric baseline.

## Phase 4: measured performance work

- [ ] Replace synchronous bond writes with bounded per-path queues and a non-blocking scheduler.
- [ ] Size queues from measured BDP and expose their high-water marks.
- [ ] Remove packet-hot-path allocations with pooled buffers and atomic path snapshots.
- [ ] Evaluate UDP-first TURN connection racing with TCP fallback.
- [ ] Evaluate adaptive FEC and path-MTU profiles.
- [ ] Add a negotiated, authenticated UDP-session ID so multipath shares one
  WireGuard backend socket without mixing devices.
  - [x] Fail safe to one non-VLESS UDP path until that protocol passes
    multi-device, reconnect, race, and live-mobile tests.

## Phase 5: security, packaging, and releases

- [x] Authenticate both sides of the DTLS transport before accepting proxy payloads.
  - [x] Require the client to pin the server leaf certificate's SHA-256 fingerprint in every transport mode.
  - [x] Support a persistent server certificate/key pair and fail closed on malformed identity configuration.
  - [x] Require a 256-bit client bearer token inside the pinned DTLS channel and document safe file distribution/rotation.
- [x] Add global/per-IP resource limits and bounded backend/smux concurrency.
  - [x] Bound active DTLS transports globally and per source IP, including the full lifetime of handed-off bond paths.
  - [x] Bound accepted smux streams and concurrent TCP backend dials/connections across sessions.
- [x] Make unimplemented compatibility flags fail safely.
- [x] Run tagged releases only after required race, vet, lint, build, and vulnerability checks.
- [ ] Publish checksums, SBOM, provenance, and signed artifacts.
  - [x] Publish checksums and request container SBOM/provenance attestations.
  - [ ] Sign release archives and the checksum manifest.
- [ ] Correct README/module/artifact provenance for this repository.
  - [x] Use the SemVer-compatible `github.com/l1ch666/vk-turn-proxy/v2` module path and document the actual build targets and current security status.
  - [x] Verify repository-wide licensing, record upstream provenance, and gate tagged release automation.
- [ ] Harden Docker and systemd execution with non-root users and resource limits.
  - [x] Run the container as a fixed non-root user with a minimal build context and persistent private identity volume.
  - [ ] Add a hardened systemd unit and packaging.

## Verification gates

- Unit and regression tests pass.
- `go test -race ./...` passes.
- `go vet` and configured linters pass.
- Client and server build for the supported CI matrix.
- No performance claim is accepted without at least three comparable runs and median results.
