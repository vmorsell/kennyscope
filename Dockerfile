# syntax=docker/dockerfile:1.7

FROM golang:1.25-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w" \
    -o /out/kennyscope ./cmd/kennyscope

FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        gosu \
        tini \
        wget \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/kennyscope /usr/local/bin/kennyscope
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

VOLUME ["/state"]

EXPOSE 8080

# Entrypoint runs as root briefly to fix /state ownership (volume mount
# clobbers Dockerfile chown), then gosu-drops to the nobody user.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["kennyscope"]
