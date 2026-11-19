# syntax=docker/dockerfile:1
#
# Slim picoclaw image: a static Go binary on busybox (~1.5 MB base).
#
# busybox is the smallest base that still ships `sh`, which the agent's shell
# tool and docker/entrypoint.sh need. CA certificates are copied in from the
# builder, and timezone data is embedded in the binary (-tags timetzdata).
#
# The builder runs on the build host and cross-compiles for the target, and the
# final stage has no RUN steps, so multi-arch builds need no QEMU emulation.
#
#   docker buildx build --platform linux/amd64,linux/arm64 -f docker/Dockerfile .

# ============================================================
# Stage 1: cross-compile the picoclaw binary
# ============================================================
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS builder

RUN apk add --no-cache ca-certificates

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG GIT_COMMIT=dev
ARG BUILD_TIME=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -tags goolm,stdjson,timetzdata \
      -ldflags "-s -w \
        -X github.com/sipeed/picoclaw/pkg/config.Version=${VERSION} \
        -X github.com/sipeed/picoclaw/pkg/config.GitCommit=${GIT_COMMIT} \
        -X github.com/sipeed/picoclaw/pkg/config.BuildTime=${BUILD_TIME} \
        -X github.com/sipeed/picoclaw/pkg/config.GoVersion=$(go env GOVERSION)" \
      -o /out/usr/local/bin/picoclaw ./cmd/picoclaw

# Assemble the rest of the runtime filesystem here so the final stage stays
# COPY-only: a non-root user, its home directory and the entrypoint.
RUN mkdir -p /out/etc/ssl/certs /out/home/picoclaw/.picoclaw \
 && cp /etc/ssl/certs/ca-certificates.crt /out/etc/ssl/certs/ \
 && printf 'root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/home:/bin/false\npicoclaw:x:1000:1000:picoclaw:/home/picoclaw:/bin/sh\n' > /out/etc/passwd \
 && printf 'root:x:0:\nnobody:x:65534:\npicoclaw:x:1000:\n' > /out/etc/group \
 && install -m 0755 docker/entrypoint.sh /out/entrypoint.sh \
 && chown -R 1000:1000 /out/home/picoclaw

# ============================================================
# Stage 2: runtime
# ============================================================
FROM busybox:1.38-musl

COPY --from=builder /out/ /

ENV HOME=/home/picoclaw \
    PICOCLAW_GATEWAY_HOST=0.0.0.0

USER picoclaw
WORKDIR /home/picoclaw
VOLUME ["/home/picoclaw/.picoclaw"]
EXPOSE 18790

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q --spider http://127.0.0.1:18790/health || exit 1

ENTRYPOINT ["/entrypoint.sh"]
