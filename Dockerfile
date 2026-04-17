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
        tini \
        wget \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/kennyscope /usr/local/bin/kennyscope

VOLUME ["/state"]

EXPOSE 8080

USER nobody

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/kennyscope"]
