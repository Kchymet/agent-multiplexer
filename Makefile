BINDIR  ?= $(HOME)/.local/bin
CONFDIR ?= $(HOME)/.config/amux
GOFLAGS ?=
LDFLAGS := -s -w

.PHONY: all build install uninstall test fmt vet clean cross run

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
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

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
