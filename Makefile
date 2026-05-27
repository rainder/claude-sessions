# claude-sessions — build + deploy targets.
#
# Common flows:
#   make             # build per-arch binaries into ./bin
#   make install     # copy the current-host arch's binary to ~/.local/bin
#   make deploy-linux-amd64 HOST=user@server   # scp the linux/amd64 binary
#   make deploy-linux-arm64 HOST=user@pi       # scp the linux/arm64 binary
#
# Personal shortcuts (e.g. `deploy-beluga`) go in Makefile.local (gitignored).
# See the README for an example.

BIN          := claude-sessions
BIN_DIR      := bin
LDFLAGS      := -s -w
GO_BUILD     := go build -trimpath -ldflags='$(LDFLAGS)'

PLATFORMS    := darwin/arm64 linux/amd64 linux/arm64

# Local install dir is on THIS machine; remote install dir is relative to
# the destination user's home (scp resolves relative paths there).
INSTALL_DIR        ?= $$HOME/.local/bin
REMOTE_INSTALL_DIR ?= .local/bin

# HOST is required for deploy-linux-* targets. Override on the command line:
#   make deploy-linux-amd64 HOST=user@server
HOST ?=

.PHONY: all build install deploy-linux-amd64 deploy-linux-arm64 clean run

all: build

# Build per-arch binaries into ./bin. One file per target platform.
build:
	@mkdir -p $(BIN_DIR)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "→ $$os/$$arch"; \
	  GOOS=$$os GOARCH=$$arch $(GO_BUILD) -o $(BIN_DIR)/$(BIN)-$$os-$$arch .; \
	done

install: build
	@os=$$(go env GOOS); arch=$$(go env GOARCH); \
	  mkdir -p $(INSTALL_DIR); \
	  cp $(BIN_DIR)/$(BIN)-$$os-$$arch $(INSTALL_DIR)/$(BIN); \
	  echo "installed → $(INSTALL_DIR)/$(BIN)  ($$os/$$arch)"

deploy-linux-amd64: build
	@if [ -z "$(HOST)" ]; then echo "error: set HOST=user@host (or just host)"; exit 1; fi
	ssh $(HOST) 'mkdir -p $(REMOTE_INSTALL_DIR)'
	scp $(BIN_DIR)/$(BIN)-linux-amd64 $(HOST):$(REMOTE_INSTALL_DIR)/$(BIN)
	@echo "deployed to $(HOST):$(REMOTE_INSTALL_DIR)/$(BIN)"

deploy-linux-arm64: build
	@if [ -z "$(HOST)" ]; then echo "error: set HOST=user@host (or just host)"; exit 1; fi
	ssh $(HOST) 'mkdir -p $(REMOTE_INSTALL_DIR)'
	scp $(BIN_DIR)/$(BIN)-linux-arm64 $(HOST):$(REMOTE_INSTALL_DIR)/$(BIN)
	@echo "deployed to $(HOST):$(REMOTE_INSTALL_DIR)/$(BIN)"

run: build
	@os=$$(go env GOOS); arch=$$(go env GOARCH); ./$(BIN_DIR)/$(BIN)-$$os-$$arch

clean:
	rm -rf $(BIN_DIR)

# Optional per-developer overrides — define personal deploy shortcuts here.
-include Makefile.local
