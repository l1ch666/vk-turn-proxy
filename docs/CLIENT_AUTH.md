# Client authentication

The server requires a shared 256-bit client token before it accepts UDP, KCP,
smux, or bond control payloads. Authentication runs only after the client has
verified the pinned DTLS server certificate, so the bearer token is never sent
outside the authenticated encrypted channel.

## Create and distribute

Pass a private path on the server:

```sh
./bin/vk-turn-server \
  -listen 0.0.0.0:56000 \
  -connect 127.0.0.1:51820 \
  -dtls-cert-file ./dtls-server-cert.pem \
  -dtls-key-file ./dtls-server-key.pem \
  -client-auth-token-file ./client-auth-token
```

If the token file is missing, the server creates a random 64-hex-character
token with mode `0600`. Copy that file to each authorized client over SSH or
another authenticated confidential channel. Never pass the raw token on the
command line, store it in a profile JSON file, or print it in logs.

The client receives only the file path:

```sh
./bin/vk-turn-client \
  -peer SERVER_IP:56000 \
  -dtls-server-fingerprint 'SERVER_SHA256_FINGERPRINT' \
  -client-auth-token-file ./client-auth-token \
  -vk-link 'https://vk.com/call/join/REDACTED'
```

## Rotation

1. Stop or drain the server.
2. Replace the token file atomically with a new 64-hex-character value and mode
   `0600`.
3. Distribute the new file to authorized clients.
4. Restart the server and clients.

The current protocol uses one shared deployment token, so rotation disconnects
old clients and there are no per-device revocations yet.

## Migration only

`-unsafe-allow-unauthenticated-clients` on the server and
`-unsafe-disable-client-auth` on the client restore the legacy unauthenticated
wire behavior. Both sides must agree. These flags expose the fixed backend to
any client that can reach the DTLS port and are intended only for a short,
controlled rollout.
