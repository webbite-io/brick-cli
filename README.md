[![Release](https://github.com/webbite-io/brick-cli/actions/workflows/release.yml/badge.svg)](https://github.com/webbite-io/brick-cli/actions/workflows/release.yml)

# Webbite Brick CLI

## About

This repository hosts the Webbite Brick CLI, the command-line client for
[Webbite][rb]'s Storage Sync feature. `brick` keeps a local folder in
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

Read more at <https://docs.webbite.io/>.

[rb]: https://webbite.io

## Installation

### Quick Install

Install the latest release on macOS or Linux like so:

```bash
curl -fsSL https://raw.githubusercontent.com/webbite-io/brick-cli/main/install.sh | bash
```

The binary will be installed to `~/.local/bin` by default.

### Custom Installation Directory

To install the latest release to a custom directory, do like so:

```bash
curl -fsSL https://raw.githubusercontent.com/webbite-io/brick-cli/main/install.sh | bash -s -- --prefix $HOME/bin
```

### Install Older Version

To install a specific version (in this example, version 0.0.1), do like so:

```bash
curl -fsSL https://raw.githubusercontent.com/webbite-io/brick-cli/main/install.sh | bash -s -- --version 0.0.1
```

### Manual Download

Download pre-built binaries from [GitHub
Releases](https://github.com/webbite-io/brick-cli/releases).

**Supported Platforms:**

- macOS (amd64, arm64)
- Linux (amd64)
- Windows (amd64)

## Usage

> **Breaking change:** account management and sync options used to be flags
> on the bare `brick` command. They're now subcommands — flags stay flags
> only where a command has more than one action to modify (`sync`, `upload`,
> `download`).
>
> | Old                                          | New                              |
> | --------------------------------------------- | -------------------------------- |
> | `brick --login`                               | `brick login`                    |
> | `brick --switch-accounts`                     | `brick switch-accounts`          |
> | `brick --whoami`                              | `brick whoami`                   |
> | `brick --restart`                             | `brick restart`                  |
> | `brick --uninstall`                           | `brick uninstall`                |
> | `brick` (bare, syncs)                         | `brick sync`                     |
> | `brick -d` / `--daemon`                       | `brick sync -d`                  |
> | `brick -d --json`                             | `brick sync -d --json`           |
> | `brick -r` / `--remote-control`               | `brick sync -r`                  |
> | `brick --agent-root PATH`                     | `brick sync --agent-root PATH`   |
> | `brick -s` / `--selective-sync`               | `brick sync -s`                  |
> | `brick --list-selective-sync`                 | `brick sync --list-selective-sync` |
>
> `-h`/`-v` and the global toggles below are unchanged, but — like any global
> flag — must come before the subcommand: `brick --no-upgrade-check sync -d`,
> not `brick sync -d --no-upgrade-check`.

```
Usage:
  brick [global flags] <command> [command flags] [args]

Global flags (must come before the command)
============================================
  -h, --help                  Show help information
  -v, --version               Show version information
      --no-upgrade-check      Disable automatic upgrade check
      --no-control-api        Disable the local status/control API (used by tray apps)
      --self-test             Print a readiness check as JSON, without syncing
      --setup-and-exit        Run interactive setup, then exit without syncing

Account Mgmt
============
  login                       Log in via browser
  switch-accounts             Switch the active account
  whoami                      Show logged-in user and account details
  restart                     Clear existing settings and configure Brick from scratch

Storage Sync
============
  sync [options]              Sync storageSyncFolder with the Storage API and watch for changes
    -d, --daemon                Detach into the background once logged in and the Storage API is reachable
        --json                  With -d: print one JSON status line instead of running interactively
    -r, --remote-control        Allow remote control via Brick webapp (also possible to enable via config file)
        --agent-root PATH       Directory to expose to remote clients when remote control is enabled (repeatable)
    -s, --selective-sync        Choose which folders to exclude from sync (deletes their local copies)
        --list-selective-sync   List the folders currently excluded from sync

Transfer
========
  upload <file|dir> [target]  Upload a local file or folder
    -r, --recursive             Required to upload a folder
    -s, --silent                Suppress all output except errors
        --overwrite             Replace an existing remote file instead of creating a copy
  download <uuid|path> [dir]  Download a remote file or folder
    -r, --recursive             Required to download a folder
    -s, --silent                Suppress all output except errors

Other
=====
  uninstall                   Uninstall brick
```

Log in, pick an account, then sync:

```bash
brick login
brick switch-accounts   # only needed if your user has more than one account
brick sync
```

On first run, `brick sync` prompts for the local folder to sync and remembers
it (`storageSyncFolder` in `~/.config/brick/config.yaml`) for subsequent runs.

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

### Re-authenticating after a revoked or expired session

If the stored access token is rejected and the refresh token can't renew it
either (revoked, expired, or otherwise invalid — the API returns
`invalid_grant`), brick prompts to log in again rather than just failing:

```
⚠️ Authentication failed! Do you want to login again (Y/n):
```

Answering `Y` runs the normal browser login flow and, if it succeeds, resumes
from scratch — a normal `brick sync`/`brick sync -d` run starts syncing,
`--setup-and-exit` re-verifies setup and prints its usual success message
instead of syncing. Answering `n` (or anything else) exits non-zero with an
error. This applies to the default sync start, `brick sync -d` in the
foreground, and `--setup-and-exit`; it never fires for `brick sync -d --json`
or the detached daemon child, which must never prompt on a terminal they
don't have.

### Daemon mode

Pass `brick sync -d` (or `--daemon`) to run every interactive step (login,
sync-folder selection, first-run onboarding) attached to the current terminal
as usual, then detach into the background once brick is logged in and the
Storage API is reachable, handing control back to the shell. Not supported on
Windows.

`--json` is an additional flag for `sync -d`/`--daemon`, meant for a
companion app that starts `brick` in daemon mode itself rather than a human at
a terminal. With `--json`:

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
| `setup_required`      | Not logged in, no active account, or no sync folder configured yet — run `brick sync` (or `login`/`switch-accounts`) interactively first. |
| `already_running`     | brick is already running for this user (instance lock held).          |
| `unsupported_platform`| Daemon mode was requested on Windows.                                  |
| `start_failed`        | Setup succeeded but starting the background process failed (e.g. the Storage API is unreachable); see `message`. |
| `internal_error`      | Reading local config failed.                                          |

```json
{"status":"error","code":"setup_required","message":"brick is not logged in; run 'brick login' first"}
```

### Self-test mode

`--self-test` is a global flag for a companion app to check whether brick is
expected to be able to sync successfully, without actually starting a sync.
It never prompts, never checks for updates, and never touches the sync folder
— the only state it may change is refreshing a stale access token, exactly as
`brick whoami` does.

Exactly one line of JSON is printed to stdout and brick exits **0**, whether
or not the checks passed — pass/fail is carried entirely by the `ready` field
and each check's own `status`, never by the process exit code, so a companion
app never has to special-case a "failed" self-test as a crash:

```json
{"status":"ok","version":"1.4.2","ready":false,"checks":[
  {"id":"instance_lock","status":"ok","message":"No other brick instance is running."},
  {"id":"configuration","status":"fail","code":"no_active_account","message":"No account selected; run 'brick switch-accounts'."},
  {"id":"authentication","status":"fail","code":"not_logged_in","message":"Not logged in; run 'brick login'."},
  {"id":"api_reachable","status":"ok","message":"Reached https://api.brick.example."},
  {"id":"storage_reachable","status":"skipped","code":"not_configured","message":"Skipped: brick is not fully configured."}
]}
```

Every `message` is normalized (capitalized, trailing period) so it can be
shown directly in a UI without reformatting — including messages built from
raw error text.

### Setup-and-exit mode

`--setup-and-exit` is a global flag that runs exactly the same steps a normal
`brick sync` start does — login (with its usual prompt if not already logged
in), sync-folder selection, first-run onboarding, and confirming the Storage
API is reachable — but stops right before a sync would actually start. It's
meant for a companion app that wants to drive brick's real interactive setup
once (e.g. from its own installer) and get a definitive pass/fail rather than
having to launch a real sync and watch for it to start working.

Unlike `--self-test`, this is not read-only: it's the genuine first-run flow,
so it will prompt for login and sync-folder choices exactly as an ordinary
`brick sync` invocation would if setup isn't already complete.

On success, it prints and exits **0**:

```
✅ Brick CLI is correctly configured and can reach the Brick API.
```

On failure (already running, login declined, Storage API unreachable, etc.)
it behaves exactly like a normal `brick sync` run would: an error is printed
to stderr and it exits non-zero, except a declined login prompt, which exits
`0` quietly (again, exactly like a normal run).

Each entry in `checks` has:

| Field     | Meaning                                                                 |
| --------- | ------------------------------------------------------------------------ |
| `id`      | Stable identifier for the check (see table below).                      |
| `status`  | `"ok"`, `"fail"`, or `"skipped"` (a prerequisite check already failed, so this one couldn't meaningfully run). |
| `code`    | Present on `"fail"`/`"skipped"`: a stable, machine-readable reason.       |
| `message` | Human-readable detail, safe to log or show a user.                       |

Checks run independently and in this order, so a companion app gets every
failing reason at once rather than only the first:

| `id`                 | Question                                            | `fail`/`skipped` codes                                                                                   |
| -------------------- | ---------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `instance_lock`      | Is another brick instance already running?           | `already_running`, `lock_error`, `lock_path_error`                                                          |
| `configuration`      | Is there an active account with a valid sync folder?  | `no_active_account`, `no_sync_folder`, `sync_folder_missing`, `config_error`                                 |
| `authentication`     | Is the stored access/refresh token still valid?       | `not_logged_in`, `session_expired`, `request_failed`, `unexpected_status`                                    |
| `api_reachable`      | Can brick reach the Brick API at all (unauthenticated)? | `unreachable`                                                                                              |
| `storage_reachable`  | Can it reach the Storage API and resolve the account's root folder? | `unreachable`; skipped as `not_configured`/`not_authenticated` if an earlier check failed |

`ready` at the top level is `true` only when every check is `"ok"` — i.e. the
next `brick sync -d` (or `brick sync -d --json`) is expected to succeed.

## Upload & Download

`brick upload` and `brick download` transfer a single file or an entire
folder outside of the two-way sync loop — useful for a one-off transfer
without setting up (or touching) a synced folder.

```bash
brick upload [-r] [-s] [--overwrite] <local-file|local-dir> [remote-path|uuid]
brick download [-r] [-s] <uuid|remote-path> [local-target-dir]
```

Both commands accept either a node UUID or a path when one is needed:

- `download`'s source and `upload`'s target may be a node UUID (e.g. copied
  from the Brick webapp) or a path like `/Documents/Reports` — resolved from
  the account root the same way the webapp resolves it.
- `upload`'s target may be omitted entirely, in which case the file or folder
  is uploaded to the account root.
- `download`'s target directory may be omitted, in which case it defaults to
  the current directory. It's created if it doesn't already exist.

**Recursive folders.** A folder source requires `-r`/`--recursive` — without
it, brick does nothing and exits non-zero rather than guessing you meant the
whole tree. With `-r`, brick first prints a summary (`Uploading 42 files in 6
folders (18.2 MB)...`) before transferring anything, then shows a per-file
progress bar as it goes.

**Errors during a recursive transfer** don't abort the whole thing — brick
continues with the remaining files, then prints every failure at the end and
exits non-zero if any occurred.

**Conflicts.**

- `download` overwrites an existing local file with the same name.
- `upload` creates a copy (`report.pdf` → `report (copy).pdf`) by default,
  matching how the Brick webapp itself handles a name already taken by a
  sibling. Pass `--overwrite` to replace the existing remote file's content
  instead.

**Silent mode.** `-s`/`--silent` suppresses everything but errors — no
pre-flight summary, no progress bars, no final summary line. Useful for a
cron job or script that only cares about the exit code.

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
| GET    | `/v1/quota`     | Account storage quota and usage (`?refresh=1` to force a live fetch). |
| POST   | `/v1/pause`     | Stop reconciling until resumed (the filesystem watcher keeps running). |
| POST   | `/v1/resume`    | Resume reconciling immediately.                                     |
| POST   | `/v1/quit`      | Gracefully shut down `brick` (same path as Ctrl+C).                 |

## Development

This repo uses a `Makefile` for building:

```bash
make build-dev   # build ./cmd/brick for the current platform, using .env.dev
make build-prod  # build ./cmd/brick for the current platform, using .env.prod
make build-all   # cross-compile for macOS/Linux/Windows
make dev         # hot-reload with air, against .env.dev (make dev ARGS="sync -s")
make install     # build using .env.prod and install to ~/.local/bin
make release     # cross-compile + package release archives
```

Copy `.env.example` to `.env.dev` (and/or `.env.prod`) and fill in
`OAUTH_CLIENT_ID` once an OIDC client has been created for brick; the API URLs
already point at the same local dev backend used by
[brick](https://github.com/webbite-io/brick-cli). `make build-dev`/`make
build-prod` error out if the corresponding file doesn't exist.

Man page and shell completions live in `man/` and `completions/` and are
bundled into release archives and installed by `install.sh`.
