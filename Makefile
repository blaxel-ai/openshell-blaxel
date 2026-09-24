# OpenShell on Blaxel. See README.md.
OPENSHELL_VERSION ?= v0.0.116
GOBIN_DIR := $(HOME)/go/bin
MODULE := github.com/blaxel-ai/openshell-blaxel/driver
PROTO_MAP := Mcompute_driver.proto=$(MODULE)/gen/computev1,Moptions.proto=$(MODULE)/gen/computev1

.PHONY: all fetch build test proto setup up configure e2e status down clean

all: fetch build

## fetch: download and verify the linux x86_64 openshell-sandbox runtime
fetch: bin/openshell-sandbox
bin/openshell-sandbox:
	mkdir -p bin && cd bin && \
	gh release download $(OPENSHELL_VERSION) -R NVIDIA/OpenShell --clobber \
	  -p 'openshell-sandbox-x86_64-unknown-linux-musl.tar.gz' -p 'openshell-sandbox-checksums-sha256.txt' && \
	grep x86_64-unknown-linux-musl openshell-sandbox-checksums-sha256.txt | shasum -a 256 -c - && \
	tar xzf openshell-sandbox-x86_64-unknown-linux-musl.tar.gz && \
	rm -f openshell-sandbox-x86_64-unknown-linux-musl.tar.gz openshell-sandbox-checksums-sha256.txt

## build: driver (host) and os-tunnel (linux, uploaded into each sandbox)
build:
	cd driver && go vet ./... && \
	go build -o bin/openshell-driver-blaxel ./cmd/openshell-driver-blaxel && \
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/os-tunnel-linux-amd64 ./cmd/os-tunnel

## test: unit tests (driver helpers, tunnel round trip) and script syntax
test:
	cd driver && go test ./...
	for f in gw/*.sh; do case "$$(head -n1 "$$f")" in *bash*) bash -n "$$f" ;; *) sh -n "$$f" ;; esac || exit 1; done

## status: gateway/driver health and OpenShell <-> Blaxel sandbox mapping
status:
	./gw/status.sh

## proto: regenerate Go stubs from driver/proto (OpenShell v0.0.116 contract)
proto:
	cd driver && PATH=$(GOBIN_DIR):$$PATH protoc -I proto \
	  --go_out=gen/computev1 --go_opt=paths=source_relative,$(PROTO_MAP) \
	  --go-grpc_out=gen/computev1 --go-grpc_opt=paths=source_relative,$(PROTO_MAP) \
	  proto/compute_driver.proto proto/options.proto

## setup: gateway PKI + CLI registration (once)
setup:
	./gw/setup.sh

## up: (re)start driver + gateway, then apply gateway settings
up: all
	./gw/restart.sh
	$(MAKE) configure

## configure: provider profile composition + Claude Code profile
configure:
	. ./gw/env.sh && openshell -g $$GATEWAY_NAME settings set --global --key providers_v2_enabled --value true --yes
	. ./gw/env.sh && if openshell -g $$GATEWAY_NAME provider profile export claude-code-blaxel >/dev/null 2>&1; then \
	  echo "provider profile claude-code-blaxel already present"; \
	else openshell -g $$GATEWAY_NAME provider profile import --file gw/claude-code-blaxel.yaml; fi

## e2e: end-to-end test against real Blaxel sandboxes (~3 min)
e2e:
	./gw/e2e.sh

## down: stop driver + gateway (Blaxel sandboxes keep running)
down:
	-. ./gw/env.sh && pkill -f "openshell-gateway --name $$GATEWAY_NAME " ; pkill -f "openshell-driver-blaxel -socket $$DRIVER_SOCKET "

clean:
	rm -rf bin driver/bin
