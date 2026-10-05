# Per-device deploy targets: build + scp + remote restart.
# Config, ca.pem, and token are never copied by these targets.
#
# Device definitions live in deploy/hosts.local.mk, which is gitignored so SSH
# users and addresses stay out of the repo. Start from deploy/hosts.example.mk.

-include deploy/hosts.local.mk

deploy-%:
	@test -n "$(DEPLOY_$*)" || (echo "unknown device $* (define DEPLOY_$* in deploy/hosts.local.mk; see deploy/hosts.example.mk)"; exit 1)
	$(eval D_TARGET := $(word 1,$(DEPLOY_$*)))
	$(eval D_GOOS   := $(word 2,$(DEPLOY_$*)))
	$(eval D_GOARCH := $(word 3,$(DEPLOY_$*)))
	$(eval D_RESTART := $(wordlist 4,99,$(DEPLOY_$*)))
	GOOS=$(D_GOOS) GOARCH=$(D_GOARCH) go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_$* ./cmd/kuma-agent
	scp $(BIN)/kuma-agent_$* $(D_TARGET):$(if $(filter windows,$(D_GOOS)),kuma-agent.new,/tmp/kuma-agent.new)
	ssh $(D_TARGET) $(D_RESTART)
