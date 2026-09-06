BINDIR  ?= $(HOME)/.local/bin
CONFDIR ?= $(HOME)/.config/amux
GOFLAGS ?=
VERSION ?= 0.1.0
LDFLAGS := -s -w -X amux/internal/buildinfo.Version=$(VERSION)

# Go modules in this repo. harnessproto is a nested, independently-published
# module (the wire protocol harness imports), so it is NOT covered by the root
# module's ./... — test/vet must recurse into it explicitly or a wire-tag change
# there would merge green.
GO_MODULES := . ./harnessproto

.PHONY: all build install uninstall test test-live fmt vet clean cross run

all: build

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/amux ./cmd/amux

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

clean:
	rm -rf bin

# Cross-compile sanity check for the two supported platforms.
cross:
	GOOS=linux  GOARCH=amd64 go build -o /dev/null ./cmd/amux
	GOOS=linux  GOARCH=arm64 go build -o /dev/null ./cmd/amux
	GOOS=darwin GOARCH=amd64 go build -o /dev/null ./cmd/amux
	GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/amux
	@echo "cross build OK"

run: build
	./bin/amux
