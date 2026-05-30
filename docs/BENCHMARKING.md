# Benchmarking the tunnel (how to measure speed)

This guide makes performance work **measurable** instead of guesswork. Pair it
with [`TUNING.md`](TUNING.md), which lists the knobs; this doc tells you how to
get a number before and after turning one.

Target setup: a **Linux (Ubuntu/Debian) VPS** running the `server` side, an
Android phone running the client. Main mode = full-tunnel (VLESS).

---

## 0. The one rule

**Change one thing, measure, write it down, repeat.** Network speed varies run to
run (cell signal, VK load, time of day), so:

- Always take **3 runs** and use the **median**, not the best.
- Keep everything else identical between runs (same place, same Wi‑Fi/LTE, phone
  plugged in, screen on).
- A change is "real" only if the median moves by **more than ~15%**. Smaller
  differences are usually noise.

---

## 1. Why you can't just trust one speedtest

The data path in full-tunnel mode is long:

```
phone apps → TUN (sing-box) → VLESS → local vk-turn core (127.0.0.1:9000)
           → smux → KCP → DTLS → TURN relay (VK) → your VPS → internet
```

Every layer adds overhead, and the **TURN relay + VK's ~5 Mbit/s per-stream cap**
dominate. So the goal isn't "max Mbit/s" in absolute terms — it's **relative**:
"did this knob make my own setup faster than it was 5 minutes ago?"

That's why the method below is built for **A/B comparison on your own line**, not
for chasing a leaderboard number.

---

## 2. Method A — phone speedtest (quick, good enough)

No VPS setup needed. Use this for fast iteration.

1. Connect the tunnel (full-tunnel mode), wait until it's solidly connected.
2. Open a speedtest: the **Ookla Speedtest** app, or `fast.com` in a browser.
3. Run it **3 times**, note download Mbit/s each time, take the median.
4. Stop the tunnel, change **one** knob (see §4), reconnect, repeat.
5. Compare medians.

**Pros:** zero setup. **Cons:** speedtest servers and the public internet add
their own variance, so only big changes show clearly.

> Tip: pick the **same** speedtest server each time (Ookla lets you choose one) —
> different servers give wildly different numbers and ruin the comparison.

---

## 3. Method B — iperf3 (controlled, recommended)

`iperf3` measures raw throughput between two points you control, so it removes
"random internet server" variance. You run the **server on your VPS** and the
**client on your phone** (via Termux).

### 3.1 Install iperf3 on the VPS (Ubuntu/Debian)

```bash
sudo apt update
sudo apt install -y iperf3
```

Start it listening (port 5201 by default):

```bash
iperf3 -s
```

Leave that running in one SSH session (or `tmux`/`screen` so it survives
disconnects). It prints results for each test that connects.

> If your VPS firewall blocks 5201, open it (only needs to be reachable through
> the tunnel, but the simplest is to allow it): `sudo ufw allow 5201`.

### 3.2 Install iperf3 on the phone (Termux)

Install **Termux** from F-Droid, then:

```bash
pkg update
pkg install iperf3
```

### 3.3 Run a test through the tunnel

With the tunnel connected, from Termux:

```bash
# download throughput (server -> phone), 30s, 4 parallel streams
iperf3 -c <VPS_PUBLIC_IP> -t 30 -P 4 -R

# upload throughput (phone -> server)
iperf3 -c <VPS_PUBLIC_IP> -t 30 -P 4
```

- `-R` = reverse (measures **download**, which is what you usually care about).
- `-P 4` = 4 parallel TCP streams. This matters: a single stream can't show the
  benefit of multiple TURN sessions or of stream-level balancing. Use `-P 4` (or
  `-P 8`) so the tunnel's parallelism is actually exercised.
- `-t 30` = run for 30 seconds (long enough for KCP to ramp up).

### 3.4 Reading the output

The line that matters is the **last summary line**:

```
[SUM]   0.00-30.00  sec   180 MBytes   50.3 Mbits/sec   123   receiver
                                       ^^^^^^^^^^^^^^^^   ^^^
                                       throughput        retransmits
```

- **Mbits/sec** — your throughput. Higher = better.
- **retransmits** (the number before `sender`/`receiver`) — TCP retransmissions.
  High and climbing = packet loss / a path struggling. If a knob raises throughput
  but also explodes retransmits, it may be unstable — prefer the stable setting.

> Note: because full-tunnel routes everything, traffic to `<VPS_PUBLIC_IP>` also
> goes **through** the tunnel and exits at the VPS — so this exercises the whole
> TURN stack end to end. That's exactly what we want for A/B. (It is a there-and-
> back path to the same box, so treat the number as *relative*, not as your raw
> line speed.)

### 3.5 Also record RTT (needed for KCP tuning)

```bash
# from the phone, through the tunnel
ping -c 20 <VPS_PUBLIC_IP>
```

Note the **avg** round-trip time. You need it to size the KCP window
(see `TUNING.md` → "Sizing the KCP window to BDP"). TURN relays often add
150–300 ms; that's normal.

---

## 4. How to change knobs on Android

The app builds the core command line for you, so to pass tuning flags use one of:

- **Raw command mode** in the client (lets you type the full `libvkturn.so …`
  argument line yourself — add flags like `-kcp-window 512 -n 12`), or
- **Environment variables** (`VK_TURN_KCP_WND`, `VK_TURN_KCP_MTU`,
  `VK_TURN_SMUX_RECVBUF`, …) in the process environment that launches the core.

The TUN MTU (`tunMtu`, full-tunnel) currently defaults to 1280 in code; change it
there if you rebuild, or treat it as fixed for now and tune the core-side
`-kcp-mtu` to stay ≤ it.

Server-side flags (set where you start `server` on the VPS) take the same
`-kcp-*` / `-smux-*` flags and `VK_TURN_*` env vars — **keep client and server in
sync** for KCP/smux or the session won't behave.

---

## 5. Suggested first session (≈30 min)

1. **Baseline.** Defaults, full-tunnel, `-P 4` iperf3 download ×3 → median. Write
   it down. Also record RTT.
2. **Sweep `-n`.** Try `-n 4`, `-n 8`, `-n 12`. More streams = more aggregate
   bandwidth until overhead/throttling bites. Find the knee. (Keep server `-n`
   high enough to accept them.)
3. **MTU sanity.** Make sure inner MTU (`tunMtu`, ~1280) ≥ `-kcp-mtu` headroom;
   if you see lots of retransmits, lower `-kcp-mtu` a notch and re-measure.
4. **KCP window.** Compute BDP from your measured RTT (see `TUNING.md`); if one
   stream is under-filling, try `-kcp-window 512` then `1024`. Watch RAM with
   high `-n`.
5. **Stop when** a change doesn't move the median by >15%. You've found the knee.

Record each step as `knob=value → median Mbit/s (retransmits)`. That table is the
input for any deeper optimization — share it and the next tuning step becomes
data-driven instead of guesswork.

---

## 6. What NOT to do

- Don't compare `-vless-bond` vs multi-session with `-P 1` — bonding only ever
  matters under parallelism, and even then it's usually slower (head-of-line).
  Use `-P 4`+.
- Don't trust a single 5-second run.
- Don't change two knobs at once — you won't know which one did it.
