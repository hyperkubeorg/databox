# Dockerfile — multi-stage production image for databox
# (§21.2). Stage 1 compiles a static binary; stage 2 is a
# minimal alpine runtime running as a non-root user.

# ---------------------------------------------------------------------------
# Stage 1: build. golang:alpine has the toolchain; CGO is disabled so the
# resulting binary is fully static and runs on any base image.
# ---------------------------------------------------------------------------
FROM golang:alpine AS build

WORKDIR /src

# Copy the module manifests first and download dependencies as their own
# layer — source edits then reuse the cached dependency layer.
COPY go.mod go.sum ./
RUN go mod download

# Now the actual source.
COPY . .

# Build metadata, injected by `make docker` (falls back to dev/unknown).
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Static build; -trimpath removes host paths from stack traces, and the
# -X flags stamp pkg/version so `databox version` reports this exact build.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/hyperkubeorg/databox/pkg/version.Version=${VERSION} \
              -X github.com/hyperkubeorg/databox/pkg/version.Commit=${COMMIT} \
              -X github.com/hyperkubeorg/databox/pkg/version.BuildDate=${BUILD_DATE}" \
    -o /out/databox ./cmd/databox

# ---------------------------------------------------------------------------
# Stage 2: runtime. Plain alpine plus CA certificates (needed for outbound
# TLS, e.g. backups to S3). Nothing from the build stage ships except the
# single binary.
# ---------------------------------------------------------------------------
FROM alpine

# CA bundle for outbound HTTPS (S3/SFTP backup destinations).
RUN apk add --no-cache ca-certificates

# Non-root user with a fixed UID (1000) so Kubernetes securityContext /
# fsGroup settings can reference it deterministically. The data directory
# is pre-created and owned by that user.
RUN adduser -D -u 1000 databox \
    && mkdir -p /var/lib/databox \
    && chown databox:databox /var/lib/databox

COPY --from=build /out/databox /usr/local/bin/databox

# Everything below runs unprivileged.
USER databox

# The node's persistent state: PebbleDB, blob chunks, identity material.
VOLUME /var/lib/databox

# The single HTTPS port: API, GUI, and internal node RPC (TLS from startup —
# probe /healthz over HTTPS).
EXPOSE 8443

# `docker run databox` starts a storage node; append any other subcommand
# (console, cluster status, ...) to run it instead.
ENTRYPOINT ["/usr/local/bin/databox"]
CMD ["server"]
