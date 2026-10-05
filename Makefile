VERSION ?= dev
MODULE  := github.com/jhyoong/KumaBoard
# Default the dev release key so agent builds can never be zero-key
# (zero-key agents fail every self-update with verify_failed).
# Override on the command line for production releases.
DEV_RELEASE_KEY := $(shell sed -n 's/^RELEASE_KEY_CURRENT=//p' configs/dev-release-keys.txt 2>/dev/null)
RELEASE_KEY_CURRENT ?= $(DEV_RELEASE_KEY)
RELEASE_KEY_NEXT    ?=
LDFLAGS := -s -w -X $(MODULE)/internal/buildinfo.Version=$(VERSION)
ifeq ($(RELEASE_KEY_CURRENT),)
$(error RELEASE_KEY_CURRENT is empty and configs/dev-release-keys.txt was not found. Zero-key agents cannot self-update. Pass RELEASE_KEY_CURRENT=<hex>.)
endif
ifneq ($(RELEASE_KEY_CURRENT),)
LDFLAGS += -X $(MODULE)/internal/buildinfo.ReleaseKeyCurrentHex=$(RELEASE_KEY_CURRENT)
endif
ifneq ($(RELEASE_KEY_NEXT),)
LDFLAGS += -X $(MODULE)/internal/buildinfo.ReleaseKeyNextHex=$(RELEASE_KEY_NEXT)
endif
BIN     := bin

export CGO_ENABLED=0

.PHONY: all test vet fmt web server agent-all \
        agent-linux-amd64 agent-linux-arm64 agent-darwin-arm64 agent-windows-amd64 \
        release stage-release

all: test server agent-all

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

web:
	cd web && npm ci && npm run build

server: web
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/kumaboard ./cmd/kumaboard

agent-all: agent-linux-amd64 agent-linux-arm64 agent-darwin-arm64 agent-windows-amd64

agent-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_linux_amd64 ./cmd/kuma-agent

agent-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_linux_arm64 ./cmd/kuma-agent

agent-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_darwin_arm64 ./cmd/kuma-agent

agent-windows-amd64:
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_windows_amd64.exe ./cmd/kuma-agent

RELEASES := releases

# Stage a built release directory into the control plane's data_dir so that
# `kumaboard sign` / `release ingest` (which read data_dir/releases/<ver>/)
# can operate. LOCAL_DATA_DIR overrides for non-production local runs.
LOCAL_DATA_DIR ?= /var/lib/kumaboard
stage-release:
	@test -n "$(VERSION)" || (echo "VERSION is required: make stage-release VERSION=x.y.z"; exit 1)
	@test -d $(RELEASES)/$(VERSION) || (echo "run 'make release VERSION=$(VERSION)' first"; exit 1)
	sudo mkdir -p $(LOCAL_DATA_DIR)/releases
	sudo cp -r $(RELEASES)/$(VERSION) $(LOCAL_DATA_DIR)/releases/
	@echo "staged $(RELEASES)/$(VERSION) -> $(LOCAL_DATA_DIR)/releases/$(VERSION)"

release:
	@test -n "$(VERSION)" || (echo "VERSION is required: make release VERSION=x.y.z" ; exit 1)
	@test "$(VERSION)" != "dev" || (echo "VERSION must not be dev"; exit 1)
	@mkdir -p $(RELEASES)/$(VERSION)/linux_amd64 $(RELEASES)/$(VERSION)/linux_arm64 \
	          $(RELEASES)/$(VERSION)/darwin_arm64 $(RELEASES)/$(VERSION)/windows_amd64
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(RELEASES)/$(VERSION)/linux_amd64/kuma-agent   ./cmd/kuma-agent
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(RELEASES)/$(VERSION)/linux_arm64/kuma-agent   ./cmd/kuma-agent
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(RELEASES)/$(VERSION)/darwin_arm64/kuma-agent  ./cmd/kuma-agent
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(RELEASES)/$(VERSION)/windows_amd64/kuma-agent.exe ./cmd/kuma-agent
	@cd $(RELEASES)/$(VERSION) && \
	  echo '{"version":"$(VERSION)","artifacts":[' > manifest.json && \
	  for d in linux_amd64 linux_arm64 darwin_arm64 windows_amd64; do \
	    os=$$(echo $$d | cut -d_ -f1); \
	    arch=$$(echo $$d | cut -d_ -f2); \
	    bin=$$d/kuma-agent; \
	    if [ "$$os" = "windows" ]; then bin=$$d/kuma-agent.exe; fi; \
	    sha=$$(shasum -a 256 $$bin | cut -d' ' -f1); \
	    size=$$(stat -f%z $$bin 2>/dev/null || stat --printf=%s $$bin); \
	    printf '{"os":"%s","arch":"%s","sha256":"%s","size_bytes":%s,"signature":""}' "$$os" "$$arch" "$$sha" "$$size"; \
	    if [ "$$d" != "windows_amd64" ]; then printf ','; fi; \
	  done >> manifest.json && \
	  echo ']}' >> manifest.json
	@echo "Release $(VERSION) built in $(RELEASES)/$(VERSION)/"

-include deploy/hosts.mk
