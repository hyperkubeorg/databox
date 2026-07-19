# Makefile — build, test, lint, container, and local cluster orchestration
# for databox (§21.1).
#
# `make` with no target prints the help below. Every target is phony (no
# file-based dependency tracking) because the Go toolchain already does
# incremental builds itself.

# The Go module path; ldflags below inject build metadata into it.
MODULE := github.com/hyperkubeorg/databox

# Where the compiled binary lands.
BIN := ./bin/databox

# Container image name:tag used by `docker` and `kind-up`. The localhost/
# prefix is deliberate: podman qualifies unprefixed tags as localhost/<name>,
# so using the fully-qualified form keeps `kind load docker-image` working
# identically under both docker and podman.
IMAGE ?= localhost/databox:dev
# The personalcloudplatform example app image (sandbox/personalcloudplatform).
PCP_IMAGE ?= localhost/pcp:dev

# Per-run tag for the images kind-up deploys. The :dev builds are retagged
# with this before side-loading, and helm deploys this tag — so every
# kind-up run changes the pod templates and Kubernetes itself rolls all
# workloads onto the image just built. Re-running kind-up against a live
# cluster therefore always ends with the cluster serving the code in your
# tree; no kubectl surgery needed.
KIND_TAG := dev-$(shell date -u +%Y%m%d%H%M%S)

# Name of the local kind cluster (matches kind.yaml).
KIND_CLUSTER ?= databox

# How many databox replicas kind-up deploys. Five by default — one per
# kind node — so node-failure and decommission drills are realistic.
DATABOX_REPLICAS ?= 5

# Auto-detect the container runtime: prefer podman when installed,
# fall back to docker (§21.1).
DOCKER_CMD := $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)

# Build metadata injected into pkg/version via the linker.
# Each `git` call falls back gracefully when this is not a git checkout
# (2>/dev/null discards the error, `|| echo` supplies the default).
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# -X overwrites the string variables in pkg/version at link time so
# `databox version` reports the real build instead of "dev"/"unknown".
LDFLAGS := -X $(MODULE)/pkg/version.Version=$(VERSION) \
           -X $(MODULE)/pkg/version.Commit=$(COMMIT) \
           -X $(MODULE)/pkg/version.BuildDate=$(BUILD_DATE)

# Default target: `make` prints the help (§21.1).
.DEFAULT_GOAL := help

.PHONY: help build install fmt vet lint tidy test cover e2e kind-up kind-down docker pcp-docker postoffice cloudferry pcp-runner pcp-camd port-forward port-forward-pcp port-forward-s3 port-forward-sql relay relay-pcp relay-ssh relay-s3 relay-sql relay-build clean

help: ## show this help
	@echo 'Usage: make <target>'
	@echo ''
	@echo 'Development:'
	@echo '  build            compile the single binary into ./bin'
	@echo '  install          install the binary into GOBIN'
	@echo '  fmt              format all Go source'
	@echo '  vet              run go vet'
	@echo '  lint             fmt + vet (no third-party linters, per dependency policy)'
	@echo '  tidy             tidy go.mod/go.sum'
	@echo ''
	@echo 'Testing:'
	@echo '  test             run unit + integration tests'
	@echo '  cover            run tests with a coverage profile and print the summary'
	@echo '  e2e              run the end-to-end / chaos validation tests (in-process cluster)'
	@echo ''
	@echo 'Container & Cluster:'
	@echo '  kind-up          create/refresh the local 5-node kind cluster: every run'
	@echo '                   rebuilds the images from your tree and rolls all pods onto them'
	@echo '  kind-down        tear down the local kind cluster'
	@echo '  docker           build the container image'
	@echo '  pcp-docker          build the personalcloudplatform example app image'
	@echo '  postoffice          build the pcp mail gateway binary into ./bin/postoffice'
	@echo '  cloudferry          build the pcp web gateway binary into ./bin/cloudferry'
	@echo '  pcp-runner          build the pcp build runner binary into ./bin/pcp-runner'
	@echo '  pcp-camd            build the Smart Home camera agent binary into ./bin/pcp-camd'
	@echo '  port-forward     forward the databox GUI/API to https://localhost:8080'
	@echo '  port-forward-pcp    forward the personalcloudplatform demo to http://localhost:4001'
	@echo '  port-forward-s3  forward the S3 gateway to http://localhost:9000'
	@echo '  port-forward-sql forward the SQL gateway to localhost:5432'
	@echo '  relay[-pcp|-ssh|-s3|-sql|-build]  optional TCP relays on the same'
	@echo '                   local ports — streaming-safe (docs/admin/kindrelay.md);'
	@echo '                   relay-ssh serves pcp git SSH at ssh://git@localhost:4222;'
	@echo '                   relay-build serves the build-runner port at localhost:4223'
	@echo ''
	@echo 'Maintenance:'
	@echo '  clean            remove build and coverage artifacts'

build: ## compile ./cmd/databox into ./bin/databox with version metadata
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/databox

install: ## install into GOBIN (or GOPATH/bin) with the same metadata
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/databox

fmt: ## format all Go source in place
	gofmt -w ./cmd ./pkg

vet: ## static analysis with the standard toolchain
	go vet ./cmd/... ./pkg/...

lint: fmt vet ## the whole lint story: formatting + vet, nothing third-party

tidy: ## prune/complete go.mod and go.sum
	go mod tidy

test: ## unit + integration tests (everything without the e2e build tag)
	go test ./cmd/... ./pkg/...

cover: ## tests with coverage; prints the per-function summary total
	go test -coverprofile=coverage.out ./cmd/... ./pkg/...
	go tool cover -func=coverage.out | tail -n 1

e2e: ## end-to-end / chaos tests; skipped cleanly while e2e/ does not exist yet
	@if [ -d e2e ]; then \
		go test -tags e2e -timeout 30m ./e2e/...; \
	else \
		echo "e2e/ not present yet — nothing to run"; \
	fi

kind-up: docker pcp-docker ## build images, create the 5-node kind cluster, deploy databox + the example apps
	# Idempotent: reuse the cluster when it already exists, so a failed
	# or repeated kind-up just converges instead of erroring.
	kind get clusters 2>/dev/null | grep -qx '$(KIND_CLUSTER)' || \
		kind create cluster --config kind.yaml --name $(KIND_CLUSTER)
	# Best-effort GC of image generations from older kind-up runs inside
	# the nodes (crictl skips anything a pod still uses), so per-run tags
	# don't accumulate forever.
	for node in $$(kind get nodes --name $(KIND_CLUSTER)); do \
		$(DOCKER_CMD) exec $$node crictl rmi --prune >/dev/null 2>&1 || true; \
	done
	# kind pulls images from its own containerd, not the host daemon, so
	# freshly built images must be side-loaded into every node. Going via
	# a saved archive (rather than `kind load docker-image`) is the form
	# that works reliably under BOTH docker and podman providers — kind's
	# direct image lookup is docker-specific and misses podman images.
	# The :dev builds travel under this run's unique tag (see KIND_TAG);
	# the host-side unique tag is dropped again right after the save.
	$(DOCKER_CMD) tag $(IMAGE) localhost/databox:$(KIND_TAG)
	$(DOCKER_CMD) save localhost/databox:$(KIND_TAG) -o /tmp/databox-kind-image.tar
	$(DOCKER_CMD) rmi localhost/databox:$(KIND_TAG)
	kind load image-archive /tmp/databox-kind-image.tar --name $(KIND_CLUSTER)
	rm -f /tmp/databox-kind-image.tar
	$(DOCKER_CMD) tag $(PCP_IMAGE) localhost/pcp:$(KIND_TAG)
	$(DOCKER_CMD) save localhost/pcp:$(KIND_TAG) -o /tmp/pcp-kind-image.tar
	$(DOCKER_CMD) rmi localhost/pcp:$(KIND_TAG)
	kind load image-archive /tmp/pcp-kind-image.tar --name $(KIND_CLUSTER)
	rm -f /tmp/pcp-kind-image.tar
	# Deploy the databox cluster and the personalcloudplatform example app
	# (sandbox/personalcloudplatform) on top of it. The local cluster runs
	# FIVE databox nodes (matching the five kind nodes) so decommission,
	# rebalancing, and failure drills can be exercised realistically;
	# override with DATABOX_REPLICAS=n.
	# guiService publishes the web GUI on NodePort 30443 (the kind.yaml
	# host mapping) WITH ClientIP stickiness, so browsers can durably
	# accept a node's self-signed cert. The main `databox` Service stays
	# ClusterIP and UNPINNED — in-cluster clients must rotate backends.
	# (The pcp chart already defaults to NodePort 30081.)
	helm upgrade --install databox charts/databox \
		--set image.repository=localhost/databox --set image.tag=$(KIND_TAG) \
		--set replicaCount=$(DATABOX_REPLICAS) \
		--set guiService.enabled=true \
		--set gateways.s3.enabled=true --set gateways.sql.enabled=true \
		--set gateways.s3.service.type=NodePort --set gateways.s3.service.nodePort=30900 \
		--set gateways.sql.service.type=NodePort --set gateways.sql.service.nodePort=30432
	helm upgrade --install pcp sandbox/personalcloudplatform/charts/pcp \
		--set image.repository=localhost/pcp --set image.tag=$(KIND_TAG)
	# Block until every workload actually runs this build — when kind-up
	# returns, the cluster serves the code in your tree. The StatefulSet
	# rolls one pod at a time (raft-safe); first boot includes cluster
	# bootstrap, so give it headroom.
	kubectl rollout status statefulset/databox --timeout=10m
	kubectl rollout status deployment/databox-gateway-s3 --timeout=5m
	kubectl rollout status deployment/databox-gateway-sql --timeout=5m
	kubectl rollout status deployment/pcp --timeout=5m
	@echo ''
	@echo 'Deployed — all pods are running this build:'
	@echo '  databox GUI/API  → https://localhost:30443 (alias https://localhost:8443)'
	@echo '                     root password: kubectl get secret databox-auth -o jsonpath="{.data.root-password}" | base64 -d'
	@echo '  pcp demo         → http://localhost:30081'
	@echo '  pcp git SSH      → ssh://git@localhost:30422 (make relay-ssh serves it at :4222)'
	@echo '  S3 gateway       → http://localhost:30900 (cleartext in-dev; TLS via gateways.s3.tlsSecret)'
	@echo '  SQL gateway      → localhost:30432 (pg wire)'
	@echo ''
	@echo 'The port-forward targets serve the same services on the familiar ports'
	@echo '(8080/4001/9000/5432) via a TCP relay — docs/admin/kindrelay.md.'
	@echo ''
	@echo 'NOTE: kind host-port mappings only apply at CLUSTER CREATION. If this'
	@echo 'cluster predates kind.yaml'"'"'s extraPortMappings, the localhost ports are'
	@echo 'dead no matter what is deployed — run `make kind-down && make kind-up`'
	@echo 'once. The port-forward targets relay to these same ports.'

kind-down: ## delete the local kind cluster and everything in it
	kind delete cluster --name $(KIND_CLUSTER)

docker: ## build the container image with the detected runtime (podman or docker)
	$(DOCKER_CMD) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(IMAGE) .

pcp-docker: ## build the personalcloudplatform example app image (context = repo root)
	$(DOCKER_CMD) build -f sandbox/personalcloudplatform/Dockerfile --target pcp -t $(PCP_IMAGE) .

postoffice: ## build the pcp mail gateway into ./bin/postoffice (static; scp it to your cloud host)
	@mkdir -p bin
	cd sandbox/personalcloudplatform && CGO_ENABLED=0 go build -trimpath -o ../../bin/postoffice ./cmd/postoffice

cloudferry: ## build the pcp web gateway into ./bin/cloudferry (static; scp it to your cloud host)
	@mkdir -p bin
	cd sandbox/personalcloudplatform && CGO_ENABLED=0 go build -trimpath -o ../../bin/cloudferry ./cmd/cloudferry

pcp-runner: ## build the pcp build runner into ./bin/pcp-runner (static; deploy in a cluster or on a bare-metal host)
	@mkdir -p bin
	cd sandbox/personalcloudplatform && CGO_ENABLED=0 go build -trimpath -o ../../bin/pcp-runner ./cmd/pcp-runner

pcp-camd: ## build the Smart Home camera agent into ./bin/pcp-camd (static; run near the cameras — needs ffmpeg on PATH)
	@mkdir -p bin
	cd sandbox/personalcloudplatform && CGO_ENABLED=0 go build -trimpath -o ../../bin/pcp-camd ./cmd/pcp-camd

port-forward: ## forward the databox GUI/API from the kind cluster to https://localhost:8080
	@echo 'databox GUI/API → https://localhost:8080  (Ctrl-C to stop)'
	@echo 'root password:   kubectl get secret databox-auth -o jsonpath="{.data.root-password}" | base64 -d'
	kubectl port-forward svc/databox 8080:8443

port-forward-pcp: ## forward the personalcloudplatform demo app to http://localhost:4001
	@echo 'pcp demo → http://localhost:4001  (Ctrl-C to stop)'
	kubectl port-forward svc/pcp 4001:8080

port-forward-s3: ## forward the S3 gateway to http://localhost:9000
	@echo 'S3 gateway → http://localhost:9000  (Ctrl-C to stop)'
	kubectl port-forward svc/databox-gateway-s3 9000:9000

port-forward-sql: ## forward the SQL gateway to localhost:5432 (psql/pg drivers)
	@echo 'SQL gateway → localhost:5432  (Ctrl-C to stop)'
	kubectl port-forward svc/databox-gateway-sql 5432:5432

# OPTIONAL streaming-safe alternative: kubectl port-forward multiplexes
# everything through one SPDY stream via the API server, and sustained
# transfers (video streaming, large blobs) can wedge it until restarted
# (docs/admin/kindrelay.md). The relay-* targets serve the SAME local
# ports as their port-forward-* siblings via a raw TCP relay
# (cmd/kindrelay) to the NodePorts kind publishes on the host. Targets
# dial 127.0.0.1 EXPLICITLY: kind's extraPortMappings bind the IPv4
# wildcard, and a container runtime's stray ::1 proxy socket can accept
# connections it can't serve — "localhost" must not land there.
RELAY_RUN = go run ./cmd/kindrelay

relay: ## optional: streaming-safe relay for the databox GUI/API at https://localhost:8080
	@echo 'databox GUI/API → https://localhost:8080  (TCP relay; Ctrl-C to stop)'
	$(RELAY_RUN) localhost:8080 127.0.0.1:30443

relay-pcp: ## optional: streaming-safe relay for pcp at http://localhost:4001
	@echo 'pcp demo → http://localhost:4001  (TCP relay; Ctrl-C to stop)'
	$(RELAY_RUN) localhost:4001 127.0.0.1:30081

relay-ssh: ## optional: streaming-safe relay for pcp git-over-SSH at localhost:4222
	@echo 'pcp git SSH → ssh://git@localhost:4222 (TCP relay; Ctrl-C to stop)'
	$(RELAY_RUN) localhost:4222 127.0.0.1:30422

relay-s3: ## optional: streaming-safe relay for the S3 gateway at http://localhost:9000
	@echo 'S3 gateway → http://localhost:9000  (TCP relay; Ctrl-C to stop)'
	$(RELAY_RUN) localhost:9000 127.0.0.1:30900

relay-sql: ## optional: streaming-safe relay for the SQL gateway at localhost:5432
	@echo 'SQL gateway → localhost:5432  (TCP relay; Ctrl-C to stop)'
	$(RELAY_RUN) localhost:5432 127.0.0.1:30432

relay-build: ## optional: streaming-safe relay for the pcp build-runner control port at localhost:4223
	@echo 'pcp buildwire → localhost:4223 (TCP relay; Ctrl-C to stop) — point pcp-runner setup here'
	$(RELAY_RUN) localhost:4223 127.0.0.1:30423

clean: ## remove build outputs, coverage, local data dirs, and stray logs
	# bin/ and coverage.out are build/test outputs; databox-data/ is where a
	# zero-config `databox server` run from the repo root puts its state;
	# *.log catches redirected gateway/server output from manual testing.
	rm -rf bin coverage.out databox-data
	rm -f *.log
