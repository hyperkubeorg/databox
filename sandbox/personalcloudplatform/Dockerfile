# Dockerfile for the Personal Cloud Platform example: five targets from
# one build stage — pcp (the app, default), postoffice (mail gateway),
# cloudferry (web gateway), pcp-runner (build runner), pcp-camd (Smart
# Home camera agent).
#
# BUILD CONTEXT IS THE REPOSITORY ROOT (the app's go.mod replaces the
# databox module with ../../, so the build needs both trees):
#
#   docker build -f sandbox/personalcloudplatform/Dockerfile -t pcp:dev .
#   docker build -f sandbox/personalcloudplatform/Dockerfile --target postoffice -t postoffice:dev .
#   docker build -f sandbox/personalcloudplatform/Dockerfile --target cloudferry -t cloudferry:dev .
#   docker build -f sandbox/personalcloudplatform/Dockerfile --target pcp-runner -t pcp-runner:dev .
#   docker build -f sandbox/personalcloudplatform/Dockerfile --target pcp-camd -t pcp-camd:dev .
#
# Same two-stage shape as the main databox image: static build on
# golang:alpine, minimal non-root runtime.

# --- build stage ---------------------------------------------------------
FROM golang:alpine AS build
WORKDIR /src
# The whole repo: databox (the dependency) + the example module.
COPY . .
WORKDIR /src/sandbox/personalcloudplatform
# CGO off → fully static binaries that run on a bare alpine layer.
RUN CGO_ENABLED=0 go build -trimpath -o /pcp ./cmd/pcp \
	&& CGO_ENABLED=0 go build -trimpath -o /postoffice ./cmd/postoffice \
	&& CGO_ENABLED=0 go build -trimpath -o /cloudferry ./cmd/cloudferry \
	&& CGO_ENABLED=0 go build -trimpath -o /pcp-runner ./cmd/pcp-runner \
	&& CGO_ENABLED=0 go build -trimpath -o /pcp-camd ./cmd/pcp-camd

# --- postoffice ----------------------------------------------------------
# The mail gateway needs a data dir (identity + sealed spool) and low
# ports for SMTP; run it with a volume at /var/lib/postoffice.
FROM alpine:latest AS postoffice
RUN adduser -D -H postoffice && mkdir -p /var/lib/postoffice && chown postoffice /var/lib/postoffice
USER postoffice
COPY --from=build /postoffice /usr/local/bin/postoffice
VOLUME /var/lib/postoffice
EXPOSE 25 8443
ENTRYPOINT ["/usr/local/bin/postoffice"]
CMD ["run", "--data-dir", "/var/lib/postoffice"]

# --- cloudferry ----------------------------------------------------------
# The web gateway needs a data dir (identity + keyless config cache) and
# the public/tunnel/control ports.
FROM alpine:latest AS cloudferry
RUN adduser -D -H cloudferry && mkdir -p /var/lib/cloudferry && chown cloudferry /var/lib/cloudferry
USER cloudferry
COPY --from=build /cloudferry /usr/local/bin/cloudferry
VOLUME /var/lib/cloudferry
EXPOSE 80 443 7443 7444
ENTRYPOINT ["/usr/local/bin/cloudferry"]
CMD ["run", "--data-dir", "/var/lib/cloudferry"]

# --- pcp-runner ----------------------------------------------------------
# The build runner holds its pairing identity in a data dir and dials OUT
# to PCP's buildwire port — no listeners. The chart's init container calls
# `pcp-runner setup` from PATH via /bin/sh, so the binary must be on PATH
# in a shell-capable image (alpine).
FROM alpine:latest AS pcp-runner
RUN adduser -D -H pcp-runner && mkdir -p /var/lib/pcp-runner && chown pcp-runner /var/lib/pcp-runner
USER pcp-runner
COPY --from=build /pcp-runner /usr/local/bin/pcp-runner
VOLUME /var/lib/pcp-runner
ENTRYPOINT ["/usr/local/bin/pcp-runner"]
CMD ["run", "--data-dir", "/var/lib/pcp-runner"]

# --- pcp-camd -------------------------------------------------------------
# The Smart Home camera agent (Draft 005 §4): pulls RTSP, segments with
# ffmpeg (bundled — the one runtime dependency), pushes to PCP over
# HTTPS, spools in its data dir. Exposes only the LAN event-callback
# port; everything else dials OUT.
FROM alpine:latest AS pcp-camd
RUN apk add --no-cache ffmpeg \
	&& adduser -D -H pcp-camd && mkdir -p /var/lib/pcp-camd && chown pcp-camd /var/lib/pcp-camd
USER pcp-camd
COPY --from=build /pcp-camd /usr/local/bin/pcp-camd
VOLUME /var/lib/pcp-camd
EXPOSE 8480
ENTRYPOINT ["/usr/local/bin/pcp-camd"]
CMD ["run", "-state", "/var/lib/pcp-camd"]

# --- pcp (default) -------------------------------------------------------
# The app itself is stateless: environment-only config, no filesystem
# state — every byte lives in databox.
FROM alpine:latest AS pcp
RUN adduser -D -H pcp
USER pcp
COPY --from=build /pcp /usr/local/bin/pcp
EXPOSE 8080 4222
ENTRYPOINT ["/usr/local/bin/pcp"]
