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
- [ ] Add versioned TCP half-close signaling (smux v1 cannot express CloseWrite/CloseRead).

## Phase 2: bond correctness

- [x] Supervise and recreate the complete bond/KCP/smux session after failure.
- [x] Negotiate protocol version, expected path count, MTU, and FEC configuration.
- [x] Scale the server KCP window from the negotiated path count.
- [x] Validate all KCP, FEC, MTU, and smux configuration at startup.

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
- [ ] Investigate a shared WireGuard backend socket per logical client session.

## Phase 5: security, packaging, and releases

- [ ] Authenticate the DTLS peer with a pinned server identity and client PSK or mTLS.
- [ ] Add global/per-IP resource limits and bounded backend/smux concurrency.
  - [x] Bound active DTLS transports globally and per source IP, including the full lifetime of handed-off bond paths.
  - [ ] Bound accepted smux streams and concurrent TCP backend dials/connections across sessions.
- [x] Make unimplemented compatibility flags fail safely.
- [ ] Run releases only from commits that passed required CI checks.
- [ ] Publish checksums, SBOM, provenance, and signed artifacts.
- [ ] Correct README/module/artifact provenance for this repository.
  - [x] Use the canonical `github.com/l1ch666/vk-turn-proxy` module path and document the actual build targets and current security status.
  - [ ] Verify repository-wide licensing and add release automation before declaring published artifact provenance complete.
- [ ] Harden Docker and systemd execution with non-root users and resource limits.

## Verification gates

- Unit and regression tests pass.
- `go test -race ./...` passes.
- `go vet` and configured linters pass.
- Client and server build for the supported CI matrix.
- No performance claim is accepted without at least three comparable runs and median results.
