#!/bin/sh
set -eu

connect_addr="${CONNECT_ADDR:?CONNECT_ADDR is required}"
listen_addr="${LISTEN_ADDR:-0.0.0.0:56000}"
client_auth_token_file="${CLIENT_AUTH_TOKEN_FILE:-/var/lib/vk-turn/client-auth-token}"

if [ -n "${DTLS_CERT_FILE:-}" ] || [ -n "${DTLS_KEY_FILE:-}" ]; then
    [ -n "${DTLS_CERT_FILE:-}" ] && [ -n "${DTLS_KEY_FILE:-}" ] || {
        echo "DTLS_CERT_FILE and DTLS_KEY_FILE must be set together" >&2
        exit 1
    }
    cert_file="$DTLS_CERT_FILE"
    key_file="$DTLS_KEY_FILE"
else
    # A combined PEM can be installed atomically. The server creates it with
    # mode 0600 on first start and reuses it from the persistent volume.
    identity_file="${DTLS_IDENTITY_FILE:-/var/lib/vk-turn/dtls-server-identity.pem}"
    cert_file="$identity_file"
    key_file="$identity_file"
fi

set -- \
    /app/vk-turn-server \
    -listen "$listen_addr" \
    -connect "$connect_addr" \
    -dtls-cert-file "$cert_file" \
    -dtls-key-file "$key_file" \
    -client-auth-token-file "$client_auth_token_file" \
    "$@"

if [ "${VLESS_BOND:-false}" = "true" ]; then
    set -- "$@" -vless -vless-bond
elif [ "${VLESS_MODE:-false}" = "true" ]; then
    set -- "$@" -vless
fi

exec "$@"
