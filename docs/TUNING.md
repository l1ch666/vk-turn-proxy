# Performance tuning

The transport stack is: app → (full‑tunnel) sing‑box TUN → VLESS → local core
`-listen` → smux → KCP → DTLS → TURN ChannelData → TCP/UDP → server → backend.

VK throttles **~5 Mbit/s per TURN stream**, so aggregate throughput comes from
running several parallel streams. There is no single "fast" switch — measure,
then turn one knob at a time.

## Always measure first

Without a baseline you are guessing.

1. Bring the tunnel up and run a throughput test end‑to‑end, e.g. `iperf3 -c <host>`
   (or a speedtest) **through** the tunnel. Record aggregate Mbit/s and RTT.
2. Run the core with `-debug` and watch for KCP retransmits / errors.
3. Change **one** parameter, re‑measure, keep what helps. Note the value.

Recommended A/B comparisons, in order of impact:

- **bond vs multi‑session** (see below) — usually the biggest difference.
- **MTU** sweep.
- **KCP window** vs your measured BDP.
- **TCP vs UDP TURN** (`-udp`).

## The knobs

All KCP/smux knobs are read from env (precedence: `-flag` > env > default).
Defaults equal the previous hardcoded values, so behaviour is unchanged until
you set something.

| Flag | Env | Default | What it does |
|---|---|---|---|
| `-kcp-window` | `VK_TURN_KCP_WND` | 256 | KCP send/recv window (packets). Raise for high‑RTT TURN paths so more data is in flight. |
| `-kcp-interval` | `VK_TURN_KCP_INTERVAL` | 20 | KCP flush interval (ms). Lower = lower latency, more CPU. |
| `-kcp-mtu` | `VK_TURN_KCP_MTU` | 1200 | KCP segment MTU (bytes). Must fit inside DTLS+TURN. Keep ≤ the inner tunnel MTU. |
| `-smux-recvbuf` | `VK_TURN_SMUX_RECVBUF` | 4194304 | smux max receive buffer (bytes). |
| `-smux-streambuf` | `VK_TURN_SMUX_STREAMBUF` | 1048576 | smux max per‑stream buffer (bytes). |
| (env only) | `VK_TURN_KCP_NODELAY` | 1 | KCP nodelay (0/1). |
| (env only) | `VK_TURN_KCP_RESEND` | 2 | KCP fast‑retransmit dup‑ACK threshold. |
| (env only) | `VK_TURN_KCP_NC` | 1 | KCP no‑congestion (0/1). |
| `-kcp-fec` | `VK_TURN_KCP_FEC` | off | Reed‑Solomon FEC `data:parity` (e.g. `10:3`). **MUST match client and server.** |

## Why single‑stream VLESS is slow — and what actually helps

The symptom (a single big download / speed test capping at ~200–500 KB/s) comes
from three compounding facts, not from a tunable being wrong:

1. **VK caps ~5 Mbit/s per TURN stream.** Aggregate speed only comes from running
   several TURN streams in parallel (`-n`).
2. **One TCP connection is pinned to one stream.** Many connections (normal
   browsing) spread across the `-n` paths and approach `n × 5 Mbit`. But a *single*
   flow uses *one* path and is capped at one path's rate. The other paths sit idle.
3. **TCP‑over‑TCP.** VLESS carries your TCP traffic over KCP/DTLS/**TCP**‑TURN. At
   TURN's high RTT, with any loss, the inner and outer retransmit timers fight and
   throughput collapses *below* the cap. (WireGuard mode avoids this — its payload
   is UDP.)

What actually moves the needle, in order:

- **KCP FEC (`-kcp-fec 10:3` / `VK_TURN_KCP_FEC`).** Recovers lost packets without
  a retransmit round‑trip — the biggest single win against (3). Must be set on
  **both** ends; the Android app does this when "KCP FEC" is enabled. *(Before
  this fix the toggle was a no‑op — the core never read the env.)*
- **`-vless-bond`.** The only mechanism that lets a *single* flow exceed one
  path's cap, by striping one KCP pipe across N TURN paths (attacks (2)). Its
  bonded KCP window now scales with path count. Still experimental: it couples
  head‑of‑line across paths, so measure vs non‑bond.
- **UDP‑TURN (`-udp` with `-vless`).** Removes the outer TCP layer → no
  TCP‑over‑TCP. Big win **where UDP to TURN isn't blocked** by the carrier; keep
  TCP as the fallback.
- **`-n`** higher (more parallel paths) helps aggregate (multi‑connection) speed.
- **`-kcp-window`** larger to fill a high‑RTT pipe (see BDP below).

> Quick start for a slow single stream: enable **KCP FEC** first; if your carrier
> allows UDP, also try **`-udp`**; for single‑flow speed tests specifically, try
> **`-vless-bond`** and compare. Change one at a time and measure.
| `-n` | — | 10 (VK) / 1 (Yandex) | Parallel TURN streams. Main throughput multiplier. |
| `-streams-per-cred` | — | 10 | Streams sharing one credential cache (fewer captcha/auth round‑trips). |

On Android the core is launched by the app, so set env via the proxy service or
pass flags through the client's **raw command** mode. The TUN (inner) MTU lives
on the app side — see `FullTunnelConfig.tunMtu` (default 1280).

### KCP window and BDP

Window × MTU ≈ bytes in flight. Target ≈ bandwidth‑delay product:
`BDP = rate × RTT`. Example: one ~5 Mbit/s stream at 200 ms RTT ≈ 125 KB, so the
default 256 × 1200 ≈ 300 KB is enough; at higher RTT raise `-kcp-window`
(512–1024). Too small a window caps throughput below the link.

### MTU alignment

The inner TUN MTU must leave room for the full outer overhead
(VLESS/TLS + smux + KCP + DTLS + TURN ChannelData + TCP/IP), otherwise outer‑path
fragmentation hurts throughput. 1280 is the safe default; you can try raising it
toward ~1340 and re‑measure. Keep `-kcp-mtu` ≤ the inner MTU.

## bond vs multi‑session

`-vless-bond` puts **one** KCP/smux session on top of several TURN/DTLS paths.
Because each path is a reliable, ordered TCP/TURN stream, striping one KCP session
across them causes head‑of‑line coupling and retransmission amplification — it is
usually **slower** than the default multi‑session mode, where N independent
sessions each carry their own KCP/smux and TCP connections are load‑balanced
across them. Multi‑session is the default; treat bond as experimental and A/B it
before relying on it.
