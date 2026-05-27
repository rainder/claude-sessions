# claude-sessions — build + deploy targets.
#
# Common flows:
#   make             # build per-arch binaries into ./bin
#   make install     # copy the current-host arch's binary to ~/.local/bin
#   make deploy      # scp Linux binaries to beluga & rpi1

BIN          := claude-sessions
BIN_DIR      := bin
LDFLAGS      := -s -w
GO_BUILD     := go build -trimpath -ldflags='$(LDFLAGS)'

PLATFORMS    := darwin/arm64 linux/amd64 linux/arm64

# Override on the command line if your host names differ:
#   make deploy BELUGA_SSH=beluga.tail-net.ts.net RPI1_SSH=pi@rpi1
BELUGA_SSH   ?= beluga
RPI1_SSH     ?= rpi1

# Local install dir is on THIS machine; remote install dir is relative to
# the destination user's home (scp resolves relative paths there).
INSTALL_DIR        ?= $$HOME/.local/bin
REMOTE_INSTALL_DIR ?= .local/bin

.PHONY: all build install deploy deploy-beluga deploy-rpi1 clean run

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

deploy: deploy-beluga deploy-rpi1

deploy-beluga: build
	ssh $(BELUGA_SSH) 'mkdir -p $(REMOTE_INSTALL_DIR)'
	scp $(BIN_DIR)/$(BIN)-linux-amd64 $(BELUGA_SSH):$(REMOTE_INSTALL_DIR)/$(BIN)
	@echo "deployed to $(BELUGA_SSH):$(REMOTE_INSTALL_DIR)/$(BIN)"

deploy-rpi1: build
	ssh $(RPI1_SSH) 'mkdir -p $(REMOTE_INSTALL_DIR)'
	scp $(BIN_DIR)/$(BIN)-linux-arm64 $(RPI1_SSH):$(REMOTE_INSTALL_DIR)/$(BIN)
	@echo "deployed to $(RPI1_SSH):$(REMOTE_INSTALL_DIR)/$(BIN)"

run: build
	@os=$$(go env GOOS); arch=$$(go env GOARCH); ./$(BIN_DIR)/$(BIN)-$$os-$$arch

clean:
	rm -rf $(BIN_DIR)
