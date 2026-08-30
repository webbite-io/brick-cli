# bash completion for brick
# Source this file or place it in:
#   Linux:  ~/.local/share/bash-completion/completions/brick
#   macOS:  $(brew --prefix)/etc/bash_completion.d/brick

_brick() {
    local cur prev words cword
    _init_completion || return

    local global_flags=(
        -h --help
        -v --version
        --no-upgrade-check
        --no-control-api
        --self-test
        --setup-and-exit
    )
    local commands=(login switch-accounts whoami restart uninstall sync upload download)

    # Find the subcommand word, if any: the first word (after the program
    # name) that isn't itself a flag. Global flags must precede it, so any
    # leading -flags are skipped.
    local command= i
    for ((i = 1; i < cword; i++)); do
        case "${words[i]}" in
            -*) ;;
            *) command="${words[i]}"; break ;;
        esac
    done

    if [[ -z "$command" ]]; then
        if [[ "$cur" == -* ]]; then
            COMPREPLY=( $(compgen -W "${global_flags[*]}" -- "$cur") )
        else
            COMPREPLY=( $(compgen -W "${commands[*]}" -- "$cur") )
        fi
        return 0
    fi

    case "$command" in
        sync)
            case "$prev" in
                --agent-root)
                    _filedir -d
                    return 0
                    ;;
            esac
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "-d --daemon --json -r --remote-control --agent-root -s --selective-sync --list-selective-sync" -- "$cur") )
            fi
            ;;
        upload)
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "-r --recursive -s --silent --overwrite" -- "$cur") )
            else
                _filedir
            fi
            ;;
        download)
            if [[ "$cur" == -* ]]; then
                COMPREPLY=( $(compgen -W "-r --recursive -s --silent" -- "$cur") )
            else
                _filedir -d
            fi
            ;;
        login|switch-accounts|whoami|restart|uninstall)
            # These take no flags or arguments.
            ;;
    esac
}

complete -F _brick brick
