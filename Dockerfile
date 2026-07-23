# syntax=docker/dockerfile:1

FROM golang:1.25.12-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags="-s -w" -o /out/vk-turn-server ./server

FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 65532 vkturn \
    && adduser -S -D -H -u 65532 -G vkturn vkturn \
    && install -d -o vkturn -g vkturn -m 0700 /var/lib/vk-turn

WORKDIR /app
COPY --from=builder /out/vk-turn-server /app/vk-turn-server
COPY --chown=vkturn:vkturn --chmod=0755 docker-entrypoint.sh /app/docker-entrypoint.sh
COPY --chmod=0444 LICENSE NOTICE README.md docs/RELEASE_LEGAL.md /usr/share/licenses/vk-turn-proxy/
COPY THIRD_PARTY_LICENSES /usr/share/licenses/vk-turn-proxy/THIRD_PARTY_LICENSES

USER vkturn:vkturn
VOLUME ["/var/lib/vk-turn"]
EXPOSE 56000/udp

ENTRYPOINT ["/app/docker-entrypoint.sh"]
