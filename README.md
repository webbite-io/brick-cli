# Webbite Brick CLI

## About

This is the repo of the Webbite Brick CLI, a command-line client for
[Brick][rb].

The CLI app `brick` keeps a local folder in two-way sync with Brick — it uploads
local-only files, downloads remote-only files, and propagates deletions in
either direction — deleting a file or folder locally moves it to trash on Brick,
and a file or folder trashed on the server is removed locally. When both sides
edit the same file, the online version wins.

After the initial pass it watches the folder for filesystem changes and polls
Brick periodically, so both sides stay in sync until interrupted.

Read more at <https://docs.webbite.io/>.

[rb]: https://webbite.io

## Installation

### Quick Install

Install the latest release on macOS or Linux like so:

```bash
curl -fsSL https://webbite.io/cli/install.sh | bash
```

The binary will be installed to `~/.local/bin` by default.

### Custom Installation Directory

To install the latest release to a custom directory, do like so:

```bash
curl -fsSL https://webbite.io/cli/install.sh | bash -s -- --prefix $HOME/bin
```

## Documentation

Read more about how to use `brick` at <https://docs.webbite.io/en/cli/>
