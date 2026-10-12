FROM golang:1.27.2-alpine3.23@sha256:9e45f0eb4a63ed37ad6e604950407558b7c738e34e015ae77a5d4ad369cde079 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -trimpath -o /out/witness ./cmd/witness

ARG TARGETARCH
RUN apk add --no-cache curl \
 && case "$TARGETARCH" in \
      arm64) checksum=3968855c11b4093af271c5226909789ea294468f6e50203d738b7504995b6247; asset_arch=arm64 ;; \
      amd64) checksum=ab073515b78ef345f64f018c0d79aa7db50106806efc686dda7181253765ae13; asset_arch=x86_64 ;; \
      *) exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/pressly/goose/releases/download/v3.28.0/goose_linux_${asset_arch}" -o /out/goose \
 && echo "$checksum  /out/goose" | sha256sum -c - \
 && chmod 755 /out/goose

FROM alpine:3.23.3@sha256:25109184c71bdad752c8312a8623239686a9a2071e8825f20acb8f2198c3f659
RUN addgroup -S -g 10001 relaydesk && adduser -S -D -H -u 10001 -G relaydesk relaydesk \
 && mkdir -p /var/lib/relaydesk-witness && chown relaydesk:relaydesk /var/lib/relaydesk-witness \
 && chmod 700 /var/lib/relaydesk-witness
WORKDIR /app
COPY --from=build /out/ /app/
COPY migrations/ /app/migrations/
COPY docs/licenses/ /app/licenses/
USER relaydesk
ENTRYPOINT ["/app/api"]
