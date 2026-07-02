[![Release](https://github.com/requestbite/brick/actions/workflows/release.yml/badge.svg)](https://github.com/requestbite/brick/actions/workflows/release.yml)

# RequestBite Brick CLI

## About

This repository hosts the RequestBite Brick CLI, the command-line client for
[RequestBite][rb]'s Storage Sync feature. `brick` keeps a local folder in
two-way sync with the Storage API: it uploads local-only files, downloads
remote-only files, and resolves conflicts with the remote copy winning. After
the initial pass it watches the folder for filesystem changes and polls the
API periodically, so both sides stay in sync until interrupted.

`brick` also handles logging in via OIDC and managing which account is active
for sync.

Read more at <https://docs.requestbite.com/>.

[rb]: https://requestbite.com

## Installation

### Quick Install

Install the latest release on macOS or Linux like so:

```bash
curl -fsSL https://raw.githubusercontent.com/requestbite/brick/main/install.sh | bash
```

The binary will be installed to `~/.local/bin` by default.

### Custom Installation Directory

To install the latest release to a custom directory, do like so:

```bash
curl -fsSL https://raw.githubusercontent.com/requestbite/brick/main/install.sh | bash -s -- --prefix $HOME/bin
```

### Install Older Version

To install a specific version (in this example, version 0.0.1), do like so:

```bash
curl -fsSL https://raw.githubusercontent.com/requestbite/brick/main/install.sh | bash -s -- --version 0.0.1
```

### Manual Download

Download pre-built binaries from [GitHub
Releases](https://github.com/requestbite/brick/releases).

**Supported Platforms:**

- macOS (amd64, arm64)
- Linux (amd64)
- Windows (amd64)

## Usage

```
Account Mgmt
============
      --login                 Log in via browser
      --switch-accounts       Switch the active account
      --whoami                Show logged-in user and account details

Storage Sync
============
  -s, --sync                  Sync storageSyncFolder with the Storage API and watch for changes

Other
=====
      --no-upgrade-check      Disable automatic upgrade check
      --uninstall             Uninstall brick
  -h, --help                  Show help information
  -v, --version               Show version information
```

Log in, pick an account, then sync:

```bash
brick --login
brick --switch-accounts   # only needed if your user has more than one account
brick --sync
```

On first run, `--sync` prompts for the local folder to sync and remembers it
(`storageSyncFolder` in `~/.config/brick/config.yaml`) for subsequent runs.

## Development

This repo uses a `Makefile` for building:

```bash
make build       # build ./cmd/brick for the current platform, output in build/
make build-all   # cross-compile for macOS/Linux/Windows
make dev         # hot-reload with air (make dev ARGS="-s")
make install     # install to ~/.local/bin
make release     # cross-compile + package release archives
```

Copy `.env.example` to `.env.dev` and fill in `OAUTH_CLIENT_ID` once an OIDC
client has been created for brick; the API URLs already point at the same
local dev backend used by [rbite](https://github.com/requestbite/rbite).

Man page and shell completions live in `man/` and `completions/` and are
bundled into release archives and installed by `install.sh`.
