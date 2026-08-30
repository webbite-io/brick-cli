# fish completion for brick
# Place this file at:
#   ~/.config/fish/completions/brick.fish

# Disable file completion for brick by default (re-enabled per rule below)
complete -c brick -f

# ── Global flags (must precede the command) ─────────────────────────────────
complete -c brick -n '__fish_use_subcommand' -s h -l help \
    -d 'Show help information'

complete -c brick -n '__fish_use_subcommand' -s v -l version \
    -d 'Show version information'

complete -c brick -n '__fish_use_subcommand' -l no-upgrade-check \
    -d 'Disable automatic upgrade check'

complete -c brick -n '__fish_use_subcommand' -l no-control-api \
    -d 'Disable the local status/control API'

complete -c brick -n '__fish_use_subcommand' -l self-test \
    -d 'Print a readiness check as JSON, without syncing'

complete -c brick -n '__fish_use_subcommand' -l setup-and-exit \
    -d 'Run interactive setup, then exit without syncing'

# ── Commands ─────────────────────────────────────────────────────────────────
complete -c brick -n '__fish_use_subcommand' -a login \
    -d 'Log in via browser'

complete -c brick -n '__fish_use_subcommand' -a switch-accounts \
    -d 'Switch the active account'

complete -c brick -n '__fish_use_subcommand' -a whoami \
    -d 'Show logged-in user and account details'

complete -c brick -n '__fish_use_subcommand' -a restart \
    -d 'Clear existing settings and configure Brick from scratch'

complete -c brick -n '__fish_use_subcommand' -a uninstall \
    -d 'Uninstall brick (interactive)'

complete -c brick -n '__fish_use_subcommand' -a sync \
    -d 'Sync storageSyncFolder with the Storage API and watch for changes'

complete -c brick -n '__fish_use_subcommand' -a upload \
    -d 'Upload a local file or folder'

complete -c brick -n '__fish_use_subcommand' -a download \
    -d 'Download a remote file or folder'

# ── sync ─────────────────────────────────────────────────────────────────────
complete -c brick -n '__fish_seen_subcommand_from sync' -s d -l daemon \
    -d 'Detach into the background once logged in and the Storage API is reachable'

complete -c brick -n '__fish_seen_subcommand_from sync' -l json \
    -d 'With -d: print one JSON status line instead of running interactively'

complete -c brick -n '__fish_seen_subcommand_from sync' -s r -l remote-control \
    -d 'Allow the Storage API to remotely list/browse/transfer files on this device'

complete -c brick -n '__fish_seen_subcommand_from sync' -l agent-root \
    -d 'Additional directory to expose to remote clients when remote control is enabled' -r

complete -c brick -n '__fish_seen_subcommand_from sync' -s s -l selective-sync \
    -d 'Choose which folders to exclude from sync'

complete -c brick -n '__fish_seen_subcommand_from sync' -l list-selective-sync \
    -d 'List the folders currently excluded from sync'

# ── upload ───────────────────────────────────────────────────────────────────
complete -c brick -n '__fish_seen_subcommand_from upload' -s r -l recursive \
    -d 'Required to upload a folder'

complete -c brick -n '__fish_seen_subcommand_from upload' -s s -l silent \
    -d 'Suppress all output except errors'

complete -c brick -n '__fish_seen_subcommand_from upload' -l overwrite \
    -d 'Replace an existing remote file instead of creating a copy'

# Re-enable file completion for upload's local file/dir argument.
complete -c brick -n '__fish_seen_subcommand_from upload' -F

# ── download ─────────────────────────────────────────────────────────────────
complete -c brick -n '__fish_seen_subcommand_from download' -s r -l recursive \
    -d 'Required to download a folder'

complete -c brick -n '__fish_seen_subcommand_from download' -s s -l silent \
    -d 'Suppress all output except errors'
