# RequestBite brick Makefile
# Cross-platform build automation for macOS, Linux, and Windows

# ENV_FILE is only loaded when explicitly set (e.g. via `make build-dev` /
# `make build-prod`, or `ENV_FILE=... make build`). Left unset for targets
# like build-all/release that expect the real values to already be in the
# environment (e.g. from CI secrets).
ENV_FILE ?=
ifneq ($(strip $(ENV_FILE)),)
-include $(ENV_FILE)
export
endif

# Extract version from git tag (strip 'v' prefix), fallback to "dev"
# If on exact tag like v1.2.3, VERSION = 1.2.3
# If ahead of tag, VERSION = 1.2.3-abc1234 (tag-commit)
# If no tags, VERSION = dev
# Use environment variable VERSION if already set (e.g., from CI/CD)
VERSION ?= $(shell if git describe --tags --exact-match 2>/dev/null >/dev/null; then \
	git describe --tags --exact-match | sed 's/^v//'; \
else \
	git describe --tags 2>/dev/null | sed 's/^v//' | sed 's/-[0-9]\+-g/-/' || echo "dev"; \
fi)

# Binary name
BINARY_NAME := brick

# Build metadata
BUILD_TIME := $(shell date -u '+%Y-%m-%d %H:%M:%S UTC')
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# Build flags
LDFLAGS := -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.BuildTime=$(BUILD_TIME)' \
	-X 'main.GitCommit=$(GIT_COMMIT)' \
	-X 'main.DefaultAPIURL=$(ACC_API_URL)' \
	-X 'main.DefaultStorageAPIURL=$(STORAGE_API_URL)' \
	-X 'main.DefaultOAuthClientID=$(OAUTH_CLIENT_ID)' \
	-X 'main.DefaultOAuthScopes=$(OAUTH_SCOPES)' \
	-X 'main.DefaultOAuthCallbackURL=$(OAUTH_CALLBACK_URL)'

BUILD_FLAGS := -ldflags="$(LDFLAGS)" -trimpath

# Target platforms
PLATFORMS := \
	darwin/amd64 \
	darwin/arm64 \
	linux/amd64 \
	windows/amd64

# Output directories
BUILD_DIR := build
DIST_DIR := dist

# Colors for output
COLOR_RESET := \033[0m
COLOR_BOLD := \033[1m
COLOR_GREEN := \033[32m
COLOR_BLUE := \033[34m
COLOR_YELLOW := \033[33m

.PHONY: all build build-dev build-prod build-all release clean install dev help

# Default target
all: build

# Build for current platform, using whichever ENV_FILE is set (internal;
# use build-dev/build-prod instead).
build:
	@if [ -z "$(ENV_FILE)" ]; then \
		echo "$(COLOR_YELLOW)No environment specified.$(COLOR_RESET) Use '$(COLOR_BOLD)make build-dev$(COLOR_RESET)' or '$(COLOR_BOLD)make build-prod$(COLOR_RESET)'."; \
		exit 1; \
	fi
	@if [ ! -f "$(ENV_FILE)" ]; then \
		echo "$(COLOR_YELLOW)Error:$(COLOR_RESET) $(ENV_FILE) not found. Copy .env.example to $(ENV_FILE) and fill in the values."; \
		exit 1; \
	fi
	@echo "$(COLOR_BOLD)$(COLOR_BLUE)Building $(BINARY_NAME) v$(VERSION) for current platform ($(ENV_FILE))...$(COLOR_RESET)"
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/brick
	@echo "$(COLOR_GREEN)✓ Build complete: $(BUILD_DIR)/$(BINARY_NAME)$(COLOR_RESET)"

# Build using .env.dev
build-dev:
	@$(MAKE) build ENV_FILE=.env.dev

# Build using .env.prod
build-prod:
	@$(MAKE) build ENV_FILE=.env.prod

# Build for all platforms
build-all: clean
	@echo "$(COLOR_BOLD)$(COLOR_BLUE)Building $(BINARY_NAME) v$(VERSION) for all platforms...$(COLOR_RESET)"
	@mkdir -p $(BUILD_DIR)
	@$(foreach platform,$(PLATFORMS),\
		$(call build_platform,$(platform)))
	@echo "$(COLOR_GREEN)✓ All builds complete$(COLOR_RESET)"
	@echo ""
	@echo "Built binaries:"
	@ls -lh $(BUILD_DIR)/$(BINARY_NAME)-*

# Build for a specific platform (internal function)
define build_platform
	$(eval OS := $(word 1,$(subst /, ,$(1))))
	$(eval ARCH := $(word 2,$(subst /, ,$(1))))
	$(eval OUTPUT := $(BUILD_DIR)/$(BINARY_NAME)-$(VERSION)-$(OS)-$(ARCH)$(if $(filter windows,$(OS)),.exe,))
	@echo "  Building for $(OS)/$(ARCH)..."
	@CGO_ENABLED=0 GOOS=$(OS) GOARCH=$(ARCH) go build $(BUILD_FLAGS) -o $(OUTPUT) ./cmd/brick
endef

# Create release archives and checksums
release: build-all
	@echo ""
	@echo "$(COLOR_BOLD)$(COLOR_BLUE)Creating release archives...$(COLOR_RESET)"
	@mkdir -p $(DIST_DIR)
	@cd $(BUILD_DIR) && \
	for binary in $(BINARY_NAME)-$(VERSION)-*; do \
		if [ -f "$$binary" ]; then \
			base=$$(basename "$$binary"); \
			os_arch=$$(echo "$$base" | sed 's/$(BINARY_NAME)-$(VERSION)-//'); \
			\
			if echo "$$os_arch" | grep -q "windows"; then \
				archive="$(BINARY_NAME)-$(VERSION)-$${os_arch%.exe}.zip"; \
				echo "  Creating $$archive..."; \
				cp ../LICENSE . 2>/dev/null || true; \
				cp ../README.md . 2>/dev/null || true; \
				zip -q "../$(DIST_DIR)/$$archive" "$$binary" LICENSE README.md 2>/dev/null || zip -q "../$(DIST_DIR)/$$archive" "$$binary"; \
				rm -f LICENSE README.md; \
			else \
				archive="$(BINARY_NAME)-$(VERSION)-$$os_arch.tar.gz"; \
				echo "  Creating $$archive..."; \
				temp_dir="$(BINARY_NAME)"; \
				mkdir -p "$$temp_dir"; \
				cp "$$binary" "$$temp_dir/$(BINARY_NAME)"; \
				cp ../LICENSE "$$temp_dir/" 2>/dev/null || true; \
				cp ../README.md "$$temp_dir/" 2>/dev/null || true; \
				cp -r ../completions "$$temp_dir/" 2>/dev/null || true; \
				cp -r ../man "$$temp_dir/" 2>/dev/null || true; \
				tar -czf "../$(DIST_DIR)/$$archive" "$$temp_dir"; \
				rm -rf "$$temp_dir"; \
			fi; \
		fi; \
	done
	@echo ""
	@echo "$(COLOR_BOLD)$(COLOR_BLUE)Generating checksums...$(COLOR_RESET)"
	@cd $(DIST_DIR) && \
	if command -v shasum >/dev/null 2>&1; then \
		shasum -a 256 *.tar.gz *.zip 2>/dev/null > SHA256SUMS; \
	else \
		sha256sum *.tar.gz *.zip 2>/dev/null > SHA256SUMS; \
	fi
	@echo "$(COLOR_GREEN)✓ Release archives created$(COLOR_RESET)"
	@echo ""
	@echo "Release artifacts:"
	@ls -lh $(DIST_DIR)/*.tar.gz $(DIST_DIR)/*.zip 2>/dev/null || true
	@echo ""
	@echo "Checksums (SHA256SUMS):"
	@cat $(DIST_DIR)/SHA256SUMS

# Clean build artifacts
clean:
	@echo "$(COLOR_BOLD)$(COLOR_BLUE)Cleaning build artifacts...$(COLOR_RESET)"
	@rm -rf $(BUILD_DIR)
	@rm -rf $(DIST_DIR)
	@rm -rf tmp/
	@rm -f build-errors.log
	@echo "$(COLOR_GREEN)✓ Clean complete$(COLOR_RESET)"

# Install locally for testing (to ~/.local/bin)
install: build-prod
	@echo "$(COLOR_BOLD)$(COLOR_BLUE)Installing $(BINARY_NAME) to ~/.local/bin...$(COLOR_RESET)"
	@mkdir -p ~/.local/bin
	@cp $(BUILD_DIR)/$(BINARY_NAME) ~/.local/bin/
	@chmod +x ~/.local/bin/$(BINARY_NAME)
	@echo "$(COLOR_GREEN)✓ Installed to ~/.local/bin/$(BINARY_NAME)$(COLOR_RESET)"
	@echo ""
	@if echo "$$PATH" | grep -q "$$HOME/.local/bin"; then \
		echo "Ready to use: $(BINARY_NAME) --version"; \
	else \
		echo "$(COLOR_YELLOW)Warning:$(COLOR_RESET) ~/.local/bin is not in your PATH"; \
		echo "Add to PATH: export PATH=\"$$HOME/.local/bin:$$PATH\";"; \
	fi

# Run with hot reload (requires air)
# Usage: make dev ARGS="-r"
dev:
	@# For --help or --version, run directly without Air
	@if echo "$(ARGS)" | grep -qE "(-h|--help|-v|--version)"; then \
		echo "$(COLOR_BLUE)Running without hot reload (--help or --version detected)$(COLOR_RESET)"; \
		$(MAKE) build-dev > /dev/null 2>&1; \
		./$(BUILD_DIR)/$(BINARY_NAME) $(ARGS); \
	elif command -v air >/dev/null; then \
		air -- $(ARGS); \
	else \
		echo "Air is not installed. Install it with: go install github.com/air-verse/air@latest"; \
		echo "Or run without hot reload using: make build-dev && ./build/$(BINARY_NAME) $(ARGS)"; \
		exit 1; \
	fi

# Show version
version:
	@echo "$(BINARY_NAME) v$(VERSION)"
	@echo "Build time: $(BUILD_TIME)"
	@echo "Git commit: $(GIT_COMMIT)"

# Show help
help:
	@echo "$(COLOR_BOLD)RequestBite brick - Build System$(COLOR_RESET)"
	@echo ""
	@echo "$(COLOR_BOLD)Usage:$(COLOR_RESET)"
	@echo "  make [target]"
	@echo ""
	@echo "$(COLOR_BOLD)Targets:$(COLOR_RESET)"
	@echo "  build-dev  - Build for current platform using .env.dev"
	@echo "  build-prod - Build for current platform using .env.prod"
	@echo "  dev        - Run with hot reload using Air, against .env.dev (for development)"
	@echo "  build-all  - Build for all platforms (darwin/amd64, darwin/arm64, linux/amd64, windows/amd64)"
	@echo "  release    - Build all platforms and create release archives with checksums"
	@echo "  clean      - Remove all build artifacts"
	@echo "  install    - Build using .env.prod and install to ~/.local/bin (for testing)"
	@echo "  version    - Show version information"
	@echo "  help       - Show this help message"
	@echo ""
	@echo "$(COLOR_BOLD)Examples:$(COLOR_RESET)"
	@echo "  make build-dev                                 # Quick build against .env.dev"
	@echo "  make build-prod                                # Quick build against .env.prod"
	@echo "  make dev                                       # Run with hot reload"
	@echo "  make dev ARGS="-r"                             # Run with CLI arguments"
	@echo "  make build-all                                # Build for all platforms"
	@echo "  make release                                  # Create release archives"
	@echo "  make install                                  # Install locally"
	@echo ""
	@echo "$(COLOR_BOLD)Current version:$(COLOR_RESET) $(VERSION)"
