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

# ── Storage sync ──────────────────────────────────────────────────────────────
complete -c brick -s s -l sync \
    -d 'Sync storageSyncFolder with the Storage API and watch for changes'

# ── Other ─────────────────────────────────────────────────────────────────────
complete -c brick -l no-upgrade-check \
    -d 'Disable automatic upgrade check on startup'

complete -c brick -l uninstall \
    -d 'Uninstall brick (interactive)'

complete -c brick -s h -l help \
    -d 'Show help information'

complete -c brick -s v -l version \
    -d 'Show version and build information'
