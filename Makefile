BINDIR  ?= $(HOME)/.local/bin
CONFDIR ?= $(HOME)/.config/amux
GOFLAGS ?=
GOVULNCHECK_VERSION := v1.8.0
# Product version derived from git (scripts/version.sh): a checkout build is
# 0.1.<count>-dev+<sha>[.dirty]; RELEASE=1 (the release workflow) produces the
# plain 0.1.<count> it publishes. Override VERSION only for local experiments.
RELEASE ?=
VERSION ?= $(shell AMUX_RELEASE=$(RELEASE) sh scripts/version.sh)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
GO_VERSION := $(shell sed -n 's/^go //p' go.mod)
NOTICE_TOOLCHAIN := go$(GO_VERSION)
LDFLAGS := -s -w -X amux/internal/buildinfo.Version=$(VERSION) -X amux/internal/buildinfo.Commit=$(COMMIT)
DIST := dist
RELEASE_PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

# Go modules in this repo. harnessproto is a nested, independently-published
# module (the wire protocol harness imports), so it is NOT covered by the root
# module's ./... — test/vet must recurse into it explicitly or a wire-tag change
# there would merge green.
GO_MODULES := . ./harnessproto

.PHONY: all build install uninstall test test-live fmt vet vuln notices notices-check clean cross run version release-assets

all: build

build: notices-check
	@if [ -n "$(strip $(GOFLAGS))" ]; then echo "custom GOFLAGS are outside the supported notice-provenance build; run go build directly and regenerate/review notices for that artifact"; exit 1; fi
	GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) CGO_ENABLED=0 GOFLAGS= go build -ldflags '$(LDFLAGS)' -o bin/amux ./cmd/amux
	GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) go run ./internal/licenses/cmd/verify -binary bin/amux

version:
	@echo $(VERSION)

# Release assets: one binary per supported platform, each built and
# provenance-verified exactly like `make build`, plus the shell shim, a
# manifest (version, daemon protocol, schema range — printed by the
# host-platform binary so it cannot drift from the code) and SHA256SUMS. The
# release workflow runs this with RELEASE=1 and uploads dist/*.
release-assets: notices-check
	@if [ -n "$(strip $(GOFLAGS))" ]; then echo "custom GOFLAGS are outside the supported notice-provenance build"; exit 1; fi
	mkdir -p $(DIST)
	find $(DIST) -mindepth 1 -maxdepth 1 -type f -delete
	@set -e; for p in $(RELEASE_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=$(DIST)/amux-$$os-$$arch; \
		echo "== $$out =="; \
		GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) CGO_ENABLED=0 GOFLAGS= GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' -o $$out ./cmd/amux; \
		GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) go run ./internal/licenses/cmd/verify -binary $$out; \
	done
	cp scripts/amux.sh $(DIST)/amux.sh
	$(DIST)/amux-$$(go env GOHOSTOS)-$$(go env GOHOSTARCH) version --manifest > $(DIST)/amux-release.json
	cd $(DIST) && if command -v sha256sum >/dev/null 2>&1; then sha256sum amux-* amux.sh > SHA256SUMS; else shasum -a 256 amux-* amux.sh > SHA256SUMS; fi
	@cat $(DIST)/amux-release.json $(DIST)/SHA256SUMS

notices:
	GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) go generate ./internal/licenses

notices-check:
	GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) go run ./internal/licenses/cmd/generate -check

# Install the binary and drop the shell shim into the config dir. Prints the one
# line to add to your shell rc. Claude status hooks install on first run.
install: build
	@mkdir -p $(BINDIR) $(CONFDIR)
	install -m 0755 bin/amux $(BINDIR)/amux
	cp scripts/amux.sh $(CONFDIR)/amux.sh
	@echo ""
	@echo "Installed amux -> $(BINDIR)/amux"
	@echo "Add this to your ~/.zshrc (and/or ~/.bashrc):"
	@echo ""
	@echo "    [ -f \"$(CONFDIR)/amux.sh\" ] && . \"$(CONFDIR)/amux.sh\""
	@echo ""
	@echo "Ensure $(BINDIR) is on your PATH. Then open a new terminal."

uninstall:
	rm -f $(BINDIR)/amux

test:
	@for m in $(GO_MODULES); do echo "== go test $$m =="; (cd $$m && go test ./...) || exit 1; done

# The live steering check: it drives the agent CLIs installed on this machine to
# confirm a steered prompt is actually submitted, not left in the composer. It is
# behind the `livecli` tag and out of `test` on purpose — it needs authenticated
# CLIs and costs a model turn per runtime — and is the check to run after touching
# agent.Keys or the daemon's steer payload.
test-live:
	go test -tags livecli -run TestLiveSteer -timeout 15m -v ./internal/daemon

fmt:
	gofmt -w .

vet:
	@for m in $(GO_MODULES); do echo "== go vet $$m =="; (cd $$m && go vet ./...) || exit 1; done

# Use the root module's selected, patched toolchain for both modules. The wire
# module keeps a lower language floor for consumers; that is not a build policy.
vuln:
	@toolchain=$$(go env GOVERSION); for m in $(GO_MODULES); do echo "== govulncheck $$m =="; (cd $$m && GOTOOLCHAIN=$$toolchain go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...) || exit 1; done

clean:
	rm -rf bin

# Cross-compile sanity check for the two supported platforms.
cross: notices-check
	@if [ -n "$(strip $(GOFLAGS))" ]; then echo "custom GOFLAGS are outside the supported notice-provenance build"; exit 1; fi
	GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) CGO_ENABLED=0 GOFLAGS= GOOS=linux  GOARCH=amd64 go build -o /dev/null ./cmd/amux
	GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) CGO_ENABLED=0 GOFLAGS= GOOS=linux  GOARCH=arm64 go build -o /dev/null ./cmd/amux
	GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) CGO_ENABLED=0 GOFLAGS= GOOS=darwin GOARCH=amd64 go build -o /dev/null ./cmd/amux
	GOTOOLCHAIN=$(NOTICE_TOOLCHAIN) CGO_ENABLED=0 GOFLAGS= GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/amux
	@echo "cross build OK"

run: build
	./bin/amux
