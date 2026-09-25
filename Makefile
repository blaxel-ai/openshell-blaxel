# OpenShell (main) on Blaxel. See README.md.
#
# OpenShell main ships as the rolling `dev` pre-release of NVIDIA/OpenShell.
OPENSHELL_RELEASE ?= dev
GOBIN_DIR := $(HOME)/go/bin
MODULE := github.com/blaxel-ai/openshell-blaxel/driver
PROTO_MAP := Mcompute_driver.proto=$(MODULE)/gen/computev1,Mdatamodel.proto=$(MODULE)/gen/datamodelv1,Mextension.proto=$(MODULE)/gen/extensionv1,Moptions.proto=$(MODULE)/gen/optionsv1,Msandbox.proto=$(MODULE)/gen/sandboxv1
MAIN_BIN := bin/main
LINUX := CGO_ENABLED=0 GOOS=linux GOARCH=amd64

.PHONY: all fetch build test proto deploy connect configure e2e status os destroy clean

all: fetch build

## fetch: OpenShell main binaries (linux gateway/supervisor/sandbox, host CLI), checksum-verified
fetch: $(MAIN_BIN)/openshell-gateway
$(MAIN_BIN)/openshell-gateway:
	mkdir -p $(MAIN_BIN) && cd $(MAIN_BIN) && \
	host=$$(uname -m | sed 's/arm64/aarch64/')-$$(uname -s | sed 's/Darwin/apple-darwin/;s/Linux/unknown-linux-musl/') && \
	gh release download $(OPENSHELL_RELEASE) -R NVIDIA/OpenShell --clobber \
	  -p 'openshell-gateway-x86_64-unknown-linux-gnu.tar.gz' -p 'openshell-supervisor-x86_64-unknown-linux-gnu.tar.gz' \
	  -p 'openshell-sandbox-x86_64-unknown-linux-musl.tar.gz' -p "openshell-$$host.tar.gz" -p '*checksums-sha256.txt' && \
	cat *checksums-sha256.txt | grep -E "(gateway|supervisor)-x86_64-unknown-linux-gnu|sandbox-x86_64-unknown-linux-musl|openshell-$$host.tar.gz" | shasum -a 256 -c - && \
	for f in *.tar.gz; do tar xzf "$$f"; done && rm -f *.tar.gz *checksums-sha256.txt && ./openshell --version

## build: driver + os-tunnel for the control/workload sandboxes (linux), os-tunnel + os-deploy for the laptop
build:
	cd driver && go vet ./... && \
	$(LINUX) go build -o bin/openshell-driver-blaxel-linux-amd64 ./cmd/openshell-driver-blaxel && \
	$(LINUX) go build -o bin/os-tunnel-linux-amd64 ./cmd/os-tunnel && \
	go build -o bin/os-tunnel ./cmd/os-tunnel && \
	go build -o bin/os-deploy ./cmd/os-deploy

## test: unit tests and script syntax (no network)
test:
	cd driver && go test ./...
	for f in gw/*.sh experiments/*.sh; do case "$$(head -n1 "$$f")" in *bash*) bash -n "$$f" ;; *) sh -n "$$f" ;; esac || exit 1; done

## proto: regenerate Go stubs from driver/proto (OpenShell main compute-driver contract)
proto:
	cd driver && PATH=$(GOBIN_DIR):$$PATH protoc -I proto --go_out=. --go-grpc_out=. \
	  --go_opt=module=$(MODULE),$(PROTO_MAP) --go-grpc_opt=module=$(MODULE),$(PROTO_MAP) proto/*.proto

## deploy: create/update the control sandbox (gateway, driver, supervisors, CLAT, DoH)
deploy: all
	. ./gw/env.sh && driver/bin/os-deploy up -name $$CONTROL_SANDBOX -owner $$GATEWAY_NAME -workspace $$BL_WORKSPACE -env $$BL_ENV -region $$BL_REGION -bin $(MAIN_BIN) -driver driver/bin -config $$(dirname $$CLIENT_BUNDLE)

## connect: laptop tunnel + CLI gateway registration
connect:
	./gw/connect.sh

## configure: import the Claude Code provider profile
configure:
	. ./gw/env.sh && (oscli provider profile export claude-code-blaxel >/dev/null && echo "profile claude-code-blaxel present") || \
	  oscli provider profile import --file gw/claude-code-blaxel.yaml

## e2e: end-to-end test against real Blaxel sandboxes (~4 min)
e2e:
	./gw/e2e.sh

## status: control plane, gateway, OpenShell <-> Blaxel sandbox mapping
status:
	./gw/status.sh

## os: run the OpenShell main CLI against this control plane, e.g. make os ARGS='sandbox list'
os:
	@. ./gw/env.sh && "$$OS_BIN" -g "$$GATEWAY_NAME" $(ARGS)

## destroy: delete the control sandbox (delete workload sandboxes through OpenShell first)
destroy:
	. ./gw/env.sh && driver/bin/os-deploy down -name $$CONTROL_SANDBOX -workspace $$BL_WORKSPACE -env $$BL_ENV
	-pkill -f "os-tunnel dial -sandbox"

clean:
	rm -rf bin driver/bin
