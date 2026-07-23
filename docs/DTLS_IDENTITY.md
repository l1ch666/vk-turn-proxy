# DTLS server identity

The client authenticates the exact leaf certificate presented by the DTLS
server. It compares the certificate's SHA-256 fingerprint in constant time and
aborts the handshake on a mismatch. This applies to default UDP forwarding,
independent VLESS sessions, and VLESS bond paths.

## Recommended persistent identity

Create an ECDSA P-256 or RSA certificate and private key on the server. A
self-signed certificate is sufficient because trust comes from the out-of-band
fingerprint rather than a public certificate authority. For example, with
OpenSSL:

```sh
openssl req -x509 -newkey ec \
  -pkeyopt ec_paramgen_curve:P-256 \
  -nodes \
  -keyout dtls-server-key.pem \
  -out dtls-server-cert.pem \
  -days 365 \
  -subj '/CN=vk-turn-server'
chmod 600 dtls-server-key.pem
```

Keep the private key only on the server. The repository ignores the two
root-level filenames used above, but service deployments should store them in
a protected system configuration directory instead of the source tree.

Start the server with both files:

```sh
./bin/vk-turn-server \
  -listen 0.0.0.0:56000 \
  -connect 127.0.0.1:51820 \
  -dtls-cert-file ./dtls-server-cert.pem \
  -dtls-key-file ./dtls-server-key.pem \
  -client-auth-token-file ./client-auth-token
```

The server validates that both flags are present, loads the key pair before
opening the listener, and prints a line like:

```text
DTLS server certificate SHA-256 fingerprint: 01:23:...:EF
```

Transfer that fingerprint to the client over an authenticated channel such as
an existing SSH session. Do not obtain the initial pin through the same
untrusted path that the DTLS connection is meant to protect.

```sh
./bin/vk-turn-client \
  -peer SERVER_IP:56000 \
  -dtls-server-fingerprint '01:23:...:EF' \
  -client-auth-token-file ./client-auth-token \
  ...
```

The client accepts either the colon-separated form printed by the server or 64
plain hexadecimal characters.

## Ephemeral and migration modes

When neither certificate-file flag is set, the server generates an ephemeral
certificate and logs a warning. This is secure only after its current
fingerprint is delivered to the client, and the pin changes on every server
restart. It is intended for initial testing, not unattended service operation.

The client refuses to start without a fingerprint. The explicit
`-dtls-insecure-skip-verify` flag is available only to stage a migration from an
older deployment. It cannot be combined with a pin and restores susceptibility
to an active man-in-the-middle attack.

## Rotation and remaining boundary

Renewing or replacing the certificate changes the pin even when the hostname
or private key is unchanged. Coordinate the server identity change and client
configuration update; clients with the old pin will fail closed during the
rotation.

This mechanism authenticates the server to the client. It does not yet
authenticate a client at the DTLS layer. Keep the global/per-IP connection
limits enabled and protect conference invite links. Client authentication is
required separately through `-client-auth-token-file`; see
[CLIENT_AUTH.md](CLIENT_AUTH.md).
