# VLESS bond control protocol

Each TURN/DTLS path sends exactly one standalone control record before KCP
traffic. Control records are newline-terminated UTF-8/ASCII and limited to 256
bytes. They must not be combined with a KCP datagram in the same DTLS record.

## Version 1

```text
VKTURNBOND/1 <hex-bond-id>\n
```

V1 contains no transport profile. A new server continues to accept it and grows
the KCP window from the maximum number of paths observed for that bond.

## Version 2

```text
VKTURNBOND/2 <hex-bond-id> <expected-paths> <kcp-mtu> <fec-data> <fec-parity>\n
```

After validating the profile, the server replies in its own DTLS record:

```text
VKTURNBOND/2 OK\n
```

An understood but rejected V2 profile is reported before the path is closed:

```text
VKTURNBOND/2 ERR <CODE>\n
```

The client must receive this acknowledgement before starting KCP. MTU and FEC
are wire-critical and must exactly match the server. Expected paths is limited
to 1..64 and lets the server size the KCP window before every path has connected.
FEC is either `0 0` or two positive shard counts totaling at most 256.

## Rollout

Deploy server support first. V1 clients remain compatible and do not receive an
acknowledgement. Client `auto` mode probes V2 first and remembers a V1 fallback
for the lifetime of the process when every path lacks a V2 acknowledgement. An
explicit V2 `ERR` does not fall back: profile mismatches must be corrected rather
than hidden behind the profile-less V1 protocol.
