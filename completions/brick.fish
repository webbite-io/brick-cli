# fish completion for brick
# Place this file at:
#   ~/.config/fish/completions/brick.fish

# Disable file completion for brick by default (re-enabled per flag below)
complete -c brick -f

# ── Account management ────────────────────────────────────────────────────────
complete -c brick -l login \
    -d 'Log in via browser'

complete -c brick -l switch-accounts \
    -d 'Switch the active account'

complete -c brick -l whoami \
    -d 'Show logged-in user and account details'

complete -c brick -l restart \
    -d 'Clear existing settings and configure Brick from scratch'

# ── Storage sync ──────────────────────────────────────────────────────────────
# Running brick with no other options syncs storageSyncFolder with the Storage
# API and watches for changes.
complete -c brick -s d -l daemon \
    -d 'Detach into the background once logged in and the Storage API is reachable'

complete -c brick -s r -l remote-control \
    -d 'Allow the Storage API to remotely list/browse/transfer files on this device'

complete -c brick -l agent-root \
    -d 'Additional directory to expose to remote clients when remote control is enabled' -r

complete -c brick -s s -l selective-sync \
    -d 'Choose which folders to exclude from sync'

complete -c brick -l list-selective-sync \
    -d 'List the folders currently excluded from sync'

# ── Other ─────────────────────────────────────────────────────────────────────
complete -c brick -l no-upgrade-check \
    -d 'Disable automatic upgrade check on startup'

complete -c brick -l no-control-api \
    -d 'Disable the local status/control API'

complete -c brick -l uninstall \
    -d 'Uninstall brick (interactive)'

complete -c brick -s h -l help \
    -d 'Show help information'

complete -c brick -s v -l version \
    -d 'Show version and build information'
