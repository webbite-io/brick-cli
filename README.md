[![Release](https://github.com/requestbite/brick/actions/workflows/release.yml/badge.svg)](https://github.com/requestbite/brick/actions/workflows/release.yml)

# RequestBite Brick CLI

## About

This repository hosts the RequestBite Brick CLI, the command-line client for
[RequestBite][rb]'s Storage Sync feature. `brick` keeps a local folder in
two-way sync with the Storage API: it uploads local-only files, downloads
remote-only files, and propagates deletions in either direction — deleting a
file or folder locally moves it to trash on the server, and a file or folder
trashed on the server is removed locally. When both sides edit the same file,
the server's copy wins. After the initial pass it watches the folder for
filesystem changes and polls the API periodically, so both sides stay in sync
until
interrupted.

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
  Running brick with no other options syncs storageSyncFolder with the Storage API and watches for changes
  -r, --remote-control        Allow the Storage API to remotely list/browse/transfer files on this device
      --agent-root PATH       Additional directory to expose to remote clients when remote control is enabled (repeatable)

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
brick
```

On first run, `brick` prompts for the local folder to sync and remembers it
(`storageSyncFolder` in `~/.config/brick/config.yaml`) for subsequent runs.

Pass `-r`/`--remote-control` to also allow the Storage API to remotely
list, browse, and transfer files on this device while syncing. Without it,
the local agent refuses any such request.

To exclude folders from sync (Dropbox-style selective sync), add an
`excludeDirs` list to `~/.config/brick/config.yaml`, with paths relative to
`storageSyncFolder`:

```yaml
excludeDirs:
  - folder/subfolder
  - other-folder
```

Files inside an excluded folder (or any folder below it) are never uploaded
or downloaded; changes to them are detected and logged, but otherwise
ignored.

### Daemon mode

Pass `-d`/`--daemon` to run every interactive step (login, sync-folder
selection, first-run onboarding) attached to the current terminal as usual,
then detach into the background once brick is logged in and the Storage API
is reachable, handing control back to the shell. Not supported on Windows.

`--json` is an additional, undocumented (not listed in `-h`) flag for
`-d`/`--daemon`, meant for a companion app that starts `brick` in daemon mode
itself rather than a human at a terminal. With `--json`:

- Nothing interactive ever runs — login, account selection and sync-folder
  setup must already be complete from a prior ordinary run, otherwise brick
  reports `setup_required` instead of prompting.
- Exactly one line of JSON is printed to stdout and brick exits; there is no
  other output to parse around.

On success:

```json
{"status":"ok","pid":12345,"logPath":"/home/user/.config/brick/daemon.log","folder":"/home/user/Brick"}
```

On failure, `status` is `"error"` and `code` is one of:

| Code                  | Meaning                                                              |
| --------------------- | --------------------------------------------------------------------- |
| `setup_required`      | Not logged in, no active account, or no sync folder configured yet — run `brick` (or `--login`/`--switch-accounts`) interactively first. |
| `already_running`     | brick is already running for this user (instance lock held).          |
| `unsupported_platform`| Daemon mode was requested on Windows.                                  |
| `start_failed`        | Setup succeeded but starting the background process failed (e.g. the Storage API is unreachable); see `message`. |
| `internal_error`      | Reading local config failed.                                          |

```json
{"status":"error","code":"setup_required","message":"brick is not logged in; run 'brick --login' first"}
```

## Local Status/Control API

While syncing, `brick` runs a local, loopback-only control API so another
local process on the same machine — e.g. a system-tray companion app — can
read live sync status and issue commands, without touching the terminal
`brick` is attached to. The full request/response schema is in
[`openapi.yaml`](openapi.yaml).

It's on by default; disable it with `--no-control-api` if you don't want any
local IPC surface (for example, running `brick` unattended on a server).

**Transport.** The API is served over a Unix domain socket (this works on
Windows too — Go/Windows have supported `AF_UNIX` sockets since Go 1.20 — so
there's no separate named-pipe code path). It is never bound to a
network-reachable address.

**Discovery.** On startup, `brick` writes a small JSON file so a client can
find the running instance and how to talk to it:

| OS      | Path                                                |
| ------- | ---------------------------------------------------- |
| Linux   | `$XDG_RUNTIME_DIR/brick/agent.json` (falls back to `~/.config/brick/run/agent.json`) |
| macOS   | `~/Library/Application Support/brick/run/agent.json` |
| Windows | `%LOCALAPPDATA%\brick\run\agent.json`                |

```json
{
  "pid": 48213,
  "version": "1.4.2",
  "protocolVersion": 1,
  "transport": "unix",
  "address": "/run/user/1000/brick/control.sock",
  "token": "base64-random-32-bytes",
  "startedAt": "2026-07-05T12:00:00Z"
}
```

The file (and the socket) are mode `0600` in a `0700` directory, and are
removed on clean shutdown. A client should treat a leftover file as stale —
and safely overwrite-able — if `pid` isn't a live process.

**Auth.** Every endpoint except `/v1/health` requires the `token` from the
discovery file in an `X-Brick-Control-Secret` header. The token is
regenerated every time `brick` starts.

**Single instance.** `brick` takes an exclusive lock (`~/.config/brick/brick.lock`)
before doing anything else, so only one sync engine ever runs per user per
machine — a second invocation fails fast with "brick is already running for
this user" instead of racing the first.

**Endpoints** (see `openapi.yaml` for full schemas):

| Method | Path            | Description                                                       |
| ------ | --------------- | ------------------------------------------------------------------ |
| GET    | `/v1/health`    | Liveness check; no auth required.                                  |
| GET    | `/v1/status`    | Current sync state, counters, in-flight transfer, last error.       |
| GET    | `/v1/activity`  | Recent upload/download/delete events (`?limit=`, default 50).      |
| GET    | `/v1/account`   | The logged-in account/client ID.                                   |
| POST   | `/v1/pause`     | Stop reconciling until resumed (the filesystem watcher keeps running). |
| POST   | `/v1/resume`    | Resume reconciling immediately.                                     |
| POST   | `/v1/quit`      | Gracefully shut down `brick` (same path as Ctrl+C).                 |

## Development

This repo uses a `Makefile` for building:

```bash
make build-dev   # build ./cmd/brick for the current platform, using .env.dev
make build-prod  # build ./cmd/brick for the current platform, using .env.prod
make build-all   # cross-compile for macOS/Linux/Windows
make dev         # hot-reload with air, against .env.dev (make dev ARGS="-s")
make install     # build using .env.prod and install to ~/.local/bin
make release     # cross-compile + package release archives
```

Copy `.env.example` to `.env.dev` (and/or `.env.prod`) and fill in
`OAUTH_CLIENT_ID` once an OIDC client has been created for brick; the API
URLs already point at the same local dev backend used by
[rbite](https://github.com/requestbite/rbite). `make build-dev`/`make
build-prod` error out if the corresponding file doesn't exist.

Man page and shell completions live in `man/` and `completions/` and are
bundled into release archives and installed by `install.sh`.
