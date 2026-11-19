# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Stage 1: build a static, stripped binary
# ---------------------------------------------------------------------------
FROM golang:1.27.4-alpine3.24 AS build

WORKDIR /src
COPY go.mod go.sum ./
# Dependencies first, so this layer is cached until go.mod/go.sum change.
# Behind a TLS-inspecting proxy, pass its root CA as a build secret (never
# stored in the image):
#   docker build --secret id=cacert,src=certs/corp-root.pem .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=cacert,required=false \
    if [ -s /run/secrets/cacert ]; then \
      cat /etc/ssl/certs/ca-certificates.crt /run/secrets/cacert > /tmp/ca.crt \
      && export SSL_CERT_FILE=/tmp/ca.crt; \
    fi \
    && go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/

# TARGETOS/TARGETARCH are set by buildx for multi-arch builds.
ARG TARGETOS=linux
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/fim2chat ./cmd/fim2chat

# ---------------------------------------------------------------------------
# Stage 2: empty image with just the binary and the public CA bundle
# ---------------------------------------------------------------------------
FROM scratch

COPY --from=build /out/fim2chat /fim2chat

# A scratch image has no trust store, so without this every HTTPS upstream fails
# with "x509: certificate signed by unknown authority". This is the generic
# public root bundle from Alpine's ca-certificates package (the same one the
# build image uses), so the image stays organization-neutral. A corporate or
# private root CA is NOT baked in: mount it at run time and set FIM_CA_CERT,
# which is added on top of this bundle.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Inside a container the proxy must listen on all interfaces.
ENV FIM_LISTEN=0.0.0.0:8787
EXPOSE 8787

# Run as nobody.
USER 65534:65534

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/fim2chat", "-healthcheck"]

ENTRYPOINT ["/fim2chat"]
