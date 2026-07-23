# UDP multipath safety

The default UDP mode currently uses one TURN/DTLS path. This is intentional:
each legacy server path opens an independent UDP socket to the backend, so
striping one WireGuard peer across several paths makes its observed endpoint
roam between backend source ports. That can add loss and reordering instead of
throughput.

The client therefore clamps non-VLESS `-n` values to one. The
`-unsafe-udp-multipath` flag exists only to reproduce and measure the legacy
behavior. KCP-based `-vless -vless-bond` is unaffected.

Correct UDP bonding needs a negotiated logical-session protocol:

1. After DTLS pinning and client-token authentication, every path sends a
   bounded control record containing a random per-tunnel ID, stable path slot,
   and expected path count.
2. The server groups only matching authenticated tunnel IDs and opens exactly
   one connected UDP backend socket for that logical session.
3. Uplink packets from every path use that socket. Downlink stays on one sticky
   healthy path and fails over without duplicating packets.
4. A path reconnect keeps its slot and tunnel ID. The backend socket survives a
   bounded zero-path grace so its source port remains stable.
5. Different tunnel IDs, even when they use the same deployment token, always
   receive separate backend sockets.

The protocol must be opt-in until parser/fuzz, multi-device isolation,
duplicate-slot, reconnect/grace, resource-limit, race, and live TURN tests all
pass. A new client falling back to a legacy server must use exactly one path;
silently falling back all workers would restore the roaming bug.
