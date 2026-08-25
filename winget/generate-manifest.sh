#!/usr/bin/env bash
#
# generate-manifest.sh - Render winget package manifests for a brick release.
#
# Reads the Windows release zip already built by `make release` (or a
# location passed explicitly) and writes the three manifest files winget
# expects into winget/manifests/w/Webbite/Brick/<version>/, ready to be
# copied into a microsoft/winget-pkgs checkout and opened as a PR.
#
# Usage:
#   make release VERSION=1.2.3            # produces dist/brick-1.2.3-windows-amd64.zip
#   ./winget/generate-manifest.sh 1.2.3
#
#   # or point at an already-published release asset instead of dist/:
#   ./winget/generate-manifest.sh 1.2.3 --url https://github.com/webbite-io/brick-cli/releases/download/1.2.3/brick-1.2.3-windows-amd64.zip

set -euo pipefail

PACKAGE_IDENTIFIER="Webbite.Brick"
PUBLISHER="Webbite"
PACKAGE_NAME="Brick"
MONIKER="brick"
GITHUB_REPO="webbite-io/brick-cli"
MANIFEST_SCHEMA_VERSION="1.10.0"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "Usage: $0 <version> [--url <installer-url>]" >&2
  exit 1
fi
shift || true

ZIP_NAME="brick-${VERSION}-windows-amd64.zip"
INSTALLER_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/${ZIP_NAME}"
ZIP_PATH="$REPO_ROOT/dist/${ZIP_NAME}"

if [ "${1:-}" = "--url" ]; then
  INSTALLER_URL="$2"
fi

# Resolve a local copy of the zip to hash: prefer dist/, else download it.
if [ ! -f "$ZIP_PATH" ]; then
  echo "dist/${ZIP_NAME} not found locally; downloading from ${INSTALLER_URL}..." >&2
  TMP_ZIP="$(mktemp -d)/${ZIP_NAME}"
  curl -fsSL -o "$TMP_ZIP" "$INSTALLER_URL"
  ZIP_PATH="$TMP_ZIP"
fi

if command -v sha256sum >/dev/null 2>&1; then
  SHA256="$(sha256sum "$ZIP_PATH" | cut -d' ' -f1)"
else
  SHA256="$(shasum -a 256 "$ZIP_PATH" | cut -d' ' -f1)"
fi
SHA256="$(echo "$SHA256" | tr '[:lower:]' '[:upper:]')"

OUT_DIR="$SCRIPT_DIR/manifests/w/${PUBLISHER}/${PACKAGE_NAME}/${VERSION}"
mkdir -p "$OUT_DIR"

# --- version manifest ---
cat >"$OUT_DIR/${PACKAGE_IDENTIFIER}.yaml" <<EOF
PackageIdentifier: ${PACKAGE_IDENTIFIER}
PackageVersion: ${VERSION}
DefaultLocale: en-US
ManifestType: version
ManifestVersion: ${MANIFEST_SCHEMA_VERSION}
EOF

# --- installer manifest ---
# The release zip is a plain zip, not an MSI/EXE installer, so it's declared
# as a "zip" installer with a nested "portable" exe. winget extracts the zip,
# copies brick.exe into %LOCALAPPDATA%\Microsoft\WinGet\Links\, and links it
# on PATH under the alias below.
cat >"$OUT_DIR/${PACKAGE_IDENTIFIER}.installer.yaml" <<EOF
PackageIdentifier: ${PACKAGE_IDENTIFIER}
PackageVersion: ${VERSION}
Platform:
  - Windows.Desktop
MinimumOSVersion: 10.0.0.0
InstallerType: zip
NestedInstallerType: portable
NestedInstallerFiles:
  - RelativeFilePath: brick.exe
    PortableCommandAlias: brick
Scope: user
InstallModes:
  - interactive
  - silent
UpgradeBehavior: install
Installers:
  - Architecture: x64
    InstallerUrl: ${INSTALLER_URL}
    InstallerSha256: ${SHA256}
ManifestType: installer
ManifestVersion: ${MANIFEST_SCHEMA_VERSION}
EOF

# --- locale manifest ---
cat >"$OUT_DIR/${PACKAGE_IDENTIFIER}.locale.en-US.yaml" <<EOF
PackageIdentifier: ${PACKAGE_IDENTIFIER}
PackageVersion: ${VERSION}
PackageLocale: en-US
Publisher: ${PUBLISHER}
PublisherUrl: https://webbite.io
PublisherSupportUrl: https://github.com/${GITHUB_REPO}/issues
PackageName: ${PACKAGE_NAME}
PackageUrl: https://github.com/${GITHUB_REPO}
License: Proprietary
ShortDescription: Command-line client for Webbite's Storage Sync, keeping a local folder in two-way sync with the Storage API.
Moniker: ${MONIKER}
Tags:
  - sync
  - storage
  - cli
ManifestType: defaultLocale
ManifestVersion: ${MANIFEST_SCHEMA_VERSION}
EOF

echo "Wrote manifests to $OUT_DIR"
echo "InstallerSha256: $SHA256"
