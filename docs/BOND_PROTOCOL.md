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

The client must receive this acknowledgement before starting KCP. MTU and FEC
are wire-critical and must exactly match the server. Expected paths is limited
to 1..64 and lets the server size the KCP window before every path has connected.
FEC is either `0 0` or two positive shard counts totaling at most 256.

## Rollout

Deploy server support first. V1 clients remain compatible and do not receive an
acknowledgement. A V2-capable client can probe for the V2 acknowledgement and
recreate the bond with V1 when it is talking to an older server.
