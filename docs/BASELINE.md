# Reproducible performance baseline

`vk-turn-bench` runs a fixed iperf3 matrix through an already-running VLESS
client, captures authenticated diagnostics before and after every measured run,
keeps the raw artifacts, and reports medians. It does not start or reconfigure
the TURN proxy itself because doing so would require placing invite links and
other credentials in an orchestration file.

## What TCP and UDP mean here

The matrix always uses an iperf3 **TCP payload** through the local VLESS TCP
listener. `turn_transport=tcp|udp` records how the vk-turn client connected to
the TURN service:

- `tcp`: the client was started without `-udp`;
- `udp`: the client was started with `-udp`.

This distinction measures the TURN transport without changing the proxy idea or
mixing an iperf3 UDP control/data topology into the result.

## Test topology

1. On the VPS/backend host, start iperf3:

   ```sh
   iperf3 -s -B 127.0.0.1 -p 5201
   ```

2. Point the vk-turn server at that backend. Use `-vless-bond` only for bond
   profiles:

   ```sh
   ./server -listen 0.0.0.0:56000 -connect 127.0.0.1:5201 -vless
   ```

3. Start the matching client and enable authenticated diagnostics. The harness
   queries the **client** endpoint so its counters cover the same local iperf3
   traffic:

   ```sh
   ./client \
     -vless -n 10 \
     -listen 127.0.0.1:9000 \
     -peer VPS_IP:56000 \
     -dtls-server-fingerprint 'SHA256_FINGERPRINT_FROM_SERVER' \
     -vk-link 'REDACTED_INVITE_LINK' \
     -diagnostics-listen 127.0.0.1:6060 \
     -diagnostics-token-file ./diagnostics.token
   ```

   Add `-udp` for the TURN-UDP profile. Add `-vless-bond` to both proxy
   binaries for a bond profile. Keep the same persistent server certificate
   for every comparable run; a changed pin indicates a different deployment
   configuration. DTLS identity setup is documented in
   [DTLS_IDENTITY.md](DTLS_IDENTITY.md), and token generation and endpoint
   security are documented in [DIAGNOSTICS.md](DIAGNOSTICS.md).

Wait until the client log shows an established session. The harness also fails
early when diagnostics reports zero active sessions (or zero active paths for a
bond profile).

## Required profile matrix

Run the harness once for each proxy configuration, restarting/reconfiguring the
client and server between rows:

| Scenario | Client TURN flag | Client mode | Server mode |
| --- | --- | --- | --- |
| `turn-tcp-multi-n10` | no `-udp` | `-vless -n 10` | `-vless` |
| `turn-udp-multi-n10` | `-udp` | `-vless -n 10` | `-vless` |
| `turn-tcp-bond-n10` | no `-udp` | `-vless -vless-bond -n 10` | `-vless -vless-bond` |
| `turn-udp-bond-n10` | `-udp` | `-vless -vless-bond -n 10` | `-vless -vless-bond` |

The defaults exercise upload/download and one/eight concurrent flows, with one
warmup and three measured 20-second runs per case. Three is a hard minimum;
the report uses receiver-side throughput medians.

## Running the harness

```sh
REVISION="$(git rev-parse HEAD)"
go run ./cmd/vk-turn-bench \
  -scenario turn-tcp-multi-n10 \
  -turn-transport tcp \
  -proxy-mode multi-session \
  -sessions 10 \
  -target 127.0.0.1:9000 \
  -diagnostics-url http://127.0.0.1:6060 \
  -diagnostics-token-file ./diagnostics.token \
  -revision "$REVISION"
```

The Bearer token is read from the file or
`VK_TURN_DIAGNOSTICS_TOKEN`; it is never accepted as a benchmark command-line
value or written to an artifact.

Useful matrix controls:

```text
-flows=1,8
-directions=upload,download
-runs=3
-warmups=1
-duration=20s
-settle=1s
-cooldown=2s
```

`-settle` lets late KCP acknowledgements/retransmission accounting arrive before
the post-run metrics snapshot. Durations are limited to five minutes to bound
the in-memory iperf3 JSON artifact.

Validate the complete plan without requiring a token, diagnostics endpoint, or
installed iperf3:

```sh
go run ./cmd/vk-turn-bench \
  -scenario turn-tcp-multi-n10 \
  -turn-transport tcp \
  -proxy-mode multi-session \
  -sessions 10 \
  -revision "$REVISION" \
  -dry-run
```

## Artifacts

Each invocation creates a new timestamped directory below
`benchmarks/results` (or `-output`):

- `report.json`: scenario/revision/environment, command arguments, parsed runs,
  diagnostics deltas, and per-case medians;
- `initial.metrics.json`: readiness/pre-run process state;
- `warmup-*.iperf.json` and `*.stderr.txt`: raw warmup output;
- `run-*.iperf.json` and `*.stderr.txt`: raw measured output;
- `run-*.metrics-before.json` and `*.metrics-after.json`: exact cumulative
  snapshots used to calculate each delta.

The report is updated after each successful operation. On a failure it remains
with `status: "failed"`, and already-written raw artifacts are preserved.
Counter decreases abort the run because they indicate a proxy restart or reset
inside a measurement window.

## Comparison rules

- Keep the proxy revision, flags, MTU/FEC tuning, hosts, iperf3 version, and
  network path fixed between comparisons. Record non-secret deviations in
  `-notes`.
- Run one scenario at a time without unrelated traffic through the client;
  KCP/FEC counters are process-wide.
- Use the median of at least three comparable measured runs. Retain the raw
  files and examine reconnects, queue drops, write errors, retransmissions, and
  FEC recovery before accepting a throughput change. Connection/backend limit
  rejections make a run invalid unless the limit itself is the variable under
  test.
- Do not claim an improvement from a single best run.
