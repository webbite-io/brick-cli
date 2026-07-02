#!/usr/bin/env bash
#
# install.sh - Install brick from GitHub releases
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/requestbite/brick/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/requestbite/brick/main/install.sh | bash -s -- --prefix=$HOME/bin
#   curl -fsSL https://raw.githubusercontent.com/requestbite/brick/main/install.sh | bash -s -- --version 0.0.1
#

set -euo pipefail

# Configuration
BINARY_NAME="brick"
GITHUB_REPO="requestbite/brick"
DEFAULT_INSTALL_DIR="$HOME/.local/bin"

# Parse command line arguments
VERSION=""
PREFIX=""

while [[ $# -gt 0 ]]; do
  case $1 in
  --version)
    VERSION="$2"
    shift 2
    ;;
  --prefix)
    PREFIX="$2"
    shift 2
    ;;
  --help)
    cat <<EOF
brick - Installation Script

Usage:
  install.sh [options]

Options:
  --version VERSION    Install specific version (e.g., 0.0.1)
  --prefix PATH        Install to PATH (default: ~/.local/bin)
  --help               Show this help message

Examples:
  # Install latest version to default location
  ./install.sh

  # Install specific version
  ./install.sh --version 0.0.1

  # Install to custom location
  ./install.sh --prefix \$HOME

  # One-line install from GitHub
  curl -fsSL https://raw.githubusercontent.com/requestbite/brick/main/install.sh | bash

EOF
    exit 0
    ;;
  *)
    echo "Unknown option: $1"
    echo "Run with --help for usage information"
    exit 1
    ;;
  esac
done

# Colors (only if terminal supports it)
if [ -t 1 ]; then
  COLOR_RESET='\033[0m'
  COLOR_BOLD='\033[1m'
  COLOR_GREEN='\033[32m'
  COLOR_BLUE='\033[34m'
  COLOR_RED='\033[31m'
  COLOR_YELLOW='\033[33m'
else
  COLOR_RESET=''
  COLOR_BOLD=''
  COLOR_GREEN=''
  COLOR_BLUE=''
  COLOR_RED=''
  COLOR_YELLOW=''
fi

# Utility functions
info() {
  echo -e "\n${COLOR_BOLD}${COLOR_BLUE}==>${COLOR_RESET} ${COLOR_BOLD}$*${COLOR_RESET}"
}

success() {
  echo -e "${COLOR_GREEN}✓${COLOR_RESET} $*"
}

error() {
  echo -e "${COLOR_RED}✗ Error:${COLOR_RESET} $*" >&2
}

warning() {
  echo -e "${COLOR_YELLOW}⚠${COLOR_RESET} $*"
}

die() {
  error "$*"
  exit 1
}

# Check if command exists
command_exists() {
  command -v "$1" >/dev/null 2>&1
}

# Cleanup function
TEMP_DIR=""
cleanup() {
  if [ -n "$TEMP_DIR" ] && [ -d "$TEMP_DIR" ]; then
    rm -rf "$TEMP_DIR"
  fi
}
trap cleanup EXIT INT TERM

# Check prerequisites
check_prerequisites() {
  local missing=()

  if ! command_exists curl; then
    missing+=("curl")
  fi

  if ! command_exists tar; then
    missing+=("tar")
  fi

  if ! command_exists shasum && ! command_exists sha256sum; then
    missing+=("shasum or sha256sum")
  fi

  if [ ${#missing[@]} -gt 0 ]; then
    error "Missing required tools:"
    for tool in "${missing[@]}"; do
      echo "  - $tool"
    done
    exit 1
  fi
}

# Detect operating system
detect_os() {
  local os
  os="$(uname -s)"

  case "$os" in
  Linux*)
    echo "linux"
    ;;
  Darwin*)
    echo "darwin"
    ;;
  *)
    die "Unsupported operating system: $os (supported: Linux, macOS)"
    ;;
  esac
}

# Detect architecture
detect_arch() {
  local arch
  arch="$(uname -m)"

  case "$arch" in
  x86_64)
    echo "amd64"
    ;;
  arm64 | aarch64)
    echo "arm64"
    ;;
  *)
    die "Unsupported architecture: $arch (supported: x86_64, arm64)"
    ;;
  esac
}

# Get latest version from GitHub
get_latest_version() {
  info "Fetching latest version from GitHub..." >&2

  local version
  version=$(curl -fsSL -H "User-Agent: brick-installer" "https://api.github.com/repos/$GITHUB_REPO/releases/latest" |
    grep '"tag_name"' |
    sed -E 's/.*"tag_name": *"v?([^"]+)".*/\1/' || echo "")

  if [ -z "$version" ]; then
    die "Failed to fetch latest version from GitHub API"
  fi

  echo "$version"
}

# Determine installation directory
determine_install_dir() {
  local install_dir

  if [ -n "$PREFIX" ]; then
    install_dir="$PREFIX"
  elif [ -d "$DEFAULT_INSTALL_DIR" ] || mkdir -p "$DEFAULT_INSTALL_DIR" 2>/dev/null; then
    install_dir="$DEFAULT_INSTALL_DIR"
  else
    install_dir="/usr/local/bin"
    warning "Cannot create $DEFAULT_INSTALL_DIR, will try system-wide installation"
    warning "This may require sudo privileges"
  fi

  echo "$install_dir"
}

# Check if directory is writable
is_writable() {
  local dir="$1"

  if [ -d "$dir" ]; then
    [ -w "$dir" ]
  else
    # Check parent directory
    local parent
    parent="$(dirname "$dir")"
    [ -w "$parent" ]
  fi
}

# Download and verify archive
download_and_verify() {
  local url="$1"
  local archive_name="$2"
  local checksum_url="$3"

  if ! curl -fsSL --progress-bar "$url" -o "$archive_name"; then
    die "Failed to download $url"
  fi
  success "Downloaded $archive_name"

  if ! curl -fsSL "$checksum_url" -o SHA256SUMS 2>/dev/null; then
    return 0
  fi

  local expected_checksum
  expected_checksum=$(grep "$archive_name" SHA256SUMS | awk '{print $1}')

  if [ -z "$expected_checksum" ]; then
    return 0
  fi

  local actual_checksum
  if command_exists shasum; then
    actual_checksum=$(shasum -a 256 "$archive_name" | awk '{print $1}')
  else
    actual_checksum=$(sha256sum "$archive_name" | awk '{print $1}')
  fi

  if [ "$expected_checksum" != "$actual_checksum" ]; then
    die "Checksum verification failed!
Expected: $expected_checksum
Actual:   $actual_checksum"
  fi
}

# Extract archive contents
extract_binary() {
  local archive_name="$1"

  if [[ "$archive_name" == *.tar.gz ]]; then
    tar -xzf "$archive_name"
    if [ ! -f "$BINARY_NAME/$BINARY_NAME" ]; then
      die "Binary not found in archive"
    fi
    mv "$BINARY_NAME/$BINARY_NAME" "${BINARY_NAME}.tmp"
    if [ -d "$BINARY_NAME/completions" ]; then
      mv "$BINARY_NAME/completions" completions
    fi
    if [ -d "$BINARY_NAME/man" ]; then
      mv "$BINARY_NAME/man" man
    fi
    rm -rf "$BINARY_NAME"
    mv "${BINARY_NAME}.tmp" "$BINARY_NAME"
  else
    die "Unsupported archive format: $archive_name"
  fi
}

# Install binary
install_binary() {
  local install_dir="$1"
  local use_sudo=false

  # Create install directory if needed
  if [ ! -d "$install_dir" ]; then
    if ! mkdir -p "$install_dir" 2>/dev/null; then
      use_sudo=true
    fi
  fi

  # Check if we need sudo
  if ! is_writable "$install_dir"; then
    use_sudo=true
  fi

  info "Installing $BINARY_NAME $VERSION:"

  if [ "$use_sudo" = true ]; then
    warning "Installation requires elevated privileges"
    if ! command_exists sudo; then
      die "sudo is required but not available"
    fi

    sudo mkdir -p "$install_dir"
    sudo rm -f "$install_dir/$BINARY_NAME"
    sudo cp "$BINARY_NAME" "$install_dir/"
    sudo chmod +x "$install_dir/$BINARY_NAME"
  else
    mkdir -p "$install_dir"
    rm -f "$install_dir/$BINARY_NAME"
    cp "$BINARY_NAME" "$install_dir/"
    chmod +x "$install_dir/$BINARY_NAME"
  fi

  success "Installed $BINARY_NAME to $install_dir"
}

# Install shell completions and man page
install_completions() {
  local os="$1"

  # Fish completion — user-level, works on both Linux and macOS
  local fish_completion_dir="$HOME/.config/fish/completions"
  if command_exists fish; then
    mkdir -p "$fish_completion_dir"
    if [ -f "completions/brick.fish" ]; then
      cp "completions/brick.fish" "$fish_completion_dir/brick.fish"
      success "Installed Fish completion to $fish_completion_dir/brick.fish"
    fi
  fi

  # Zsh completion
  local zsh_completion_dir="$HOME/.local/share/zsh/site-functions"
  if [ "$os" = "darwin" ] && command_exists brew; then
    local brew_prefix
    brew_prefix="$(brew --prefix 2>/dev/null)"
    if [ -d "$brew_prefix/share/zsh/site-functions" ]; then
      zsh_completion_dir="$brew_prefix/share/zsh/site-functions"
    fi
  fi
  if [ -f "completions/_brick" ]; then
    mkdir -p "$zsh_completion_dir"
    cp "completions/_brick" "$zsh_completion_dir/_brick"
    success "Installed Zsh completion to $zsh_completion_dir/_brick"
  fi

  # Man page
  local man_dir="$HOME/.local/share/man/man1"
  if [ -f "man/brick.1" ]; then
    mkdir -p "$man_dir"
    cp "man/brick.1" "$man_dir/brick.1"
    success "Installed man page to $man_dir/brick.1"
    if command_exists mandb; then
      mandb -q "$HOME/.local/share/man" 2>/dev/null || true
    elif command_exists makewhatis; then
      makewhatis "$HOME/.local/share/man" 2>/dev/null || true
    fi
  fi

  # Bash completion
  local bash_completion_dir="$HOME/.local/share/bash-completion/completions"
  if [ "$os" = "darwin" ] && command_exists brew; then
    local brew_prefix
    brew_prefix="$(brew --prefix 2>/dev/null)"
    if [ -d "$brew_prefix/share/bash-completion/completions" ]; then
      bash_completion_dir="$brew_prefix/share/bash-completion/completions"
    fi
  fi
  if [ -f "completions/brick.bash" ]; then
    mkdir -p "$bash_completion_dir"
    cp "completions/brick.bash" "$bash_completion_dir/brick"
    success "Installed Bash completion to $bash_completion_dir/brick"
  fi
}

# Verify installation
verify_installation() {
  local install_dir="$1"
  local binary_path="$install_dir/$BINARY_NAME"

  if [ ! -f "$binary_path" ]; then
    die "Installation verification failed: $binary_path not found"
  fi

  if [ ! -x "$binary_path" ]; then
    die "Installation verification failed: $binary_path is not executable"
  fi

  # Try to run --version
  if "$binary_path" --version >/dev/null 2>&1; then
    success "Installation verified"
  else
    warning "Binary exists but --version check failed"
  fi
}

# Detect the user's shell config file
detect_shell_config() {
  if [ -n "${FISH_VERSION:-}" ] || [[ "${SHELL:-}" == */fish ]]; then
    echo "$HOME/.config/fish/config.fish"
  elif [ -n "${ZSH_VERSION:-}" ] || [[ "${SHELL:-}" == */zsh ]]; then
    echo "$HOME/.zshrc"
  elif [ -n "${BASH_VERSION:-}" ] || [[ "${SHELL:-}" == */bash ]]; then
    # Prefer .bash_profile on macOS (login shell), .bashrc on Linux
    if [ -f "$HOME/.bash_profile" ]; then
      echo "$HOME/.bash_profile"
    else
      echo "$HOME/.bashrc"
    fi
  else
    echo ""
  fi
}

# Check if install directory is in PATH; offer to add it interactively
check_path() {
  local install_dir="$1"

  # Already in PATH — nothing to do
  if echo ":${PATH}:" | grep -q ":${install_dir}:"; then
    return 0
  fi

  local shell_config
  shell_config="$(detect_shell_config)"

  local config_display="${shell_config/#$HOME/\~}"

  if [ -z "$shell_config" ]; then
    warning "$install_dir is not in your PATH"
    echo "Add the following line to your shell configuration file:"
    echo "  export PATH=\"$install_dir:\$PATH\""
    echo ""
    return 0
  fi

  # Determine the export/set line for the detected shell
  local export_line
  if [[ "$shell_config" == */fish/config.fish ]]; then
    export_line="fish_add_path $install_dir"
  else
    export_line="export PATH=\"$install_dir:\$PATH\""
  fi

  # Ask the user (default yes)
  echo ""
  printf "${COLOR_YELLOW}⚠${COLOR_RESET} ${COLOR_BOLD}brick${COLOR_RESET} is not in your PATH."
  echo ""
  printf "  Should I add ${COLOR_BOLD}$install_dir${COLOR_RESET} to ${COLOR_BOLD}$config_display${COLOR_RESET}? [Y/n] "

  # Read from /dev/tty so it works even when script is piped from curl
  local answer
  if read -r answer </dev/tty 2>/dev/null; then
    : # got input
  else
    answer="n"
  fi

  case "${answer:-Y}" in
  [Yy]* | "")
    echo "" >>"$shell_config"
    echo "# Added by brick installer" >>"$shell_config"
    echo "$export_line" >>"$shell_config"
    success "Added to $config_display"

    # Update PATH in the current shell session
    export PATH="$install_dir:$PATH"
    success "Updated PATH for this session — $BINARY_NAME is ready to use now"
    ;;
  *)
    warning "Skipped. Add the following line to $config_display manually:"
    echo "  $export_line"
    echo "Then restart your shell or run:  source $shell_config"
    echo ""
    ;;
  esac
}

# Main installation function
main() {
  info "brick - Installation Script"
  echo ""

  # Check prerequisites
  check_prerequisites

  # Detect platform
  local os
  local arch
  os=$(detect_os)
  arch=$(detect_arch)

  echo "Detected platform: $os/$arch"

  # Get version
  if [ -z "$VERSION" ]; then
    VERSION=$(get_latest_version)
  fi

  # Determine installation directory
  local install_dir
  install_dir=$(determine_install_dir)
  echo "Install directory: $install_dir"

  # Construct download URL
  local archive_name="${BINARY_NAME}-${VERSION}-${os}-${arch}.tar.gz"
  local base_url="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}"
  local download_url="${base_url}/${archive_name}"
  local checksum_url="${base_url}/SHA256SUMS"

  # Create temporary directory
  TEMP_DIR=$(mktemp -d)
  cd "$TEMP_DIR"

  # Download everything first
  info "Downloading from GitHub releases..."
  download_and_verify "$download_url" "$archive_name" "$checksum_url"

  # Extract brick
  extract_binary "$archive_name"

  # Install brick (binary + completions + man page)
  install_binary "$install_dir"
  install_completions "$os"

  # Verify
  verify_installation "$install_dir"

  echo ""
  success "Installation complete!"
  echo ""

  # Check PATH
  check_path "$install_dir"

  # Show usage
  echo "Usage:"
  echo "  $BINARY_NAME --help"
  echo "  $BINARY_NAME --version"
  echo ""
}

# Run main function
main
