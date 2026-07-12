# Runtime diagnostics

The client and server expose the same optional loopback-only HTTP diagnostics
service. It is disabled by default and does not open a listener unless
`-diagnostics-listen` is set.

## Security model

- The listen host must be a literal loopback IP (`127.0.0.0/8` or `::1`).
  Hostnames, wildcard addresses, and LAN addresses are rejected at startup.
- Every route requires `Authorization: Bearer <token>`.
- The token is exactly 32 random bytes encoded as 64 hexadecimal characters.
- Supply the token through `-diagnostics-token-file` or the
  `VK_TURN_DIAGNOSTICS_TOKEN` environment variable. It is deliberately not a
  command-line value because command lines are visible to other processes and
  through the pprof cmdline handler.
- On Unix, a token file must not grant group or other permissions. Create it
  with mode `0600`.
- Tokens in query parameters, cookies, or Basic authentication are ignored.

Generate a token file on Linux:

```sh
umask 077
openssl rand -hex 32 > diagnostics.token
```

Or set an environment token in PowerShell:

```powershell
$bytes = [byte[]]::new(32)
[Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
$env:VK_TURN_DIAGNOSTICS_TOKEN = [Convert]::ToHexString($bytes).ToLowerInvariant()
```

## Starting the endpoint

These flags work on both binaries:

```text
-diagnostics-listen=127.0.0.1:6060
-diagnostics-token-file=/run/secrets/vk-turn-diagnostics
-diagnostics-pprof=false
```

The token file flag is optional when `VK_TURN_DIAGNOSTICS_TOKEN` is set.
Pprof is independently opt-in:

```sh
# Append the normal server transport flags to this command.
./server \
  -diagnostics-listen=127.0.0.1:6060 \
  -diagnostics-token-file=./diagnostics.token \
  -diagnostics-pprof
```

Port `0` is accepted for temporary runs and tests; the selected address is
written to the process log.

## Routes

- `GET /healthz` returns `{"status":"ok"}`.
- `GET /metrics` returns the process metric snapshot as JSON.
- `/debug/pprof/*` is registered only with `-diagnostics-pprof`.

The metrics snapshot contains monotonic process totals plus active and recent
per-path details. Per-path history is bounded to the 128 most recently closed
paths; process byte, operation, error, and latency totals are not discarded
when a path leaves that history. Write latency is sampled once every 64 physical
path writes to keep hot-path overhead bounded.

Example requests:

```sh
TOKEN="$(tr -d '\r\n' < diagnostics.token)"
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:6060/metrics
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:6060/debug/pprof/
curl -H "Authorization: Bearer $TOKEN" \
  'http://127.0.0.1:6060/debug/pprof/profile?seconds=30' \
  -o cpu.pprof
go tool pprof cpu.pprof
```

CPU and delta profiles are limited to 60 seconds per request, and traces to 15
seconds. The diagnostics server uses bounded shutdown and does not enable CORS.
