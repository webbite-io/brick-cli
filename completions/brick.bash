# bash completion for brick
# Source this file or place it in:
#   Linux:  ~/.local/share/bash-completion/completions/brick
#   macOS:  $(brew --prefix)/etc/bash_completion.d/brick

_brick() {
    local cur prev words cword
    _init_completion || return

    local all_flags=(
        --login
        --switch-accounts
        --whoami
        --restart
        -d --daemon
        -r --remote-control
        --agent-root
        -s --selective-sync
        --list-selective-sync
        --no-upgrade-check
        --no-control-api
        --uninstall
        -h --help
        -v --version
    )

    case "$prev" in
        --agent-root)
            _filedir -d
            return 0
            ;;
    esac

    # Complete flags
    if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "${all_flags[*]}" -- "$cur") )
        return 0
    fi
}

complete -F _brick brick
