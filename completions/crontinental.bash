# bash completion for crontinental: verbs, job names, and flags.
# Names come from the script's hidden `names` verb, so this never reads the
# database itself and can't drift from its schema.

_crontinental() {
    local cur prev verb i
    cur=${COMP_WORDS[COMP_CWORD]}
    prev=${COMP_WORDS[COMP_CWORD-1]}

    # Everything after a literal `--` is the job's own command: complete it as one.
    for ((i = 1; i < COMP_CWORD; i++)); do
        if [[ ${COMP_WORDS[i]} == -- ]]; then
            _comp_command_offset $((i + 1)) 2>/dev/null || _command_offset $((i + 1))
            return
        fi
    done

    verb=${COMP_WORDS[1]}
    if ((COMP_CWORD == 1)); then
        COMPREPLY=($(compgen -W "in at every list show edit log run rm enable disable prune tick path" -- "$cur"))
        return
    fi

    case $prev in
        --catchup) COMPREPLY=($(compgen -W "run skip" -- "$cur")); return ;;
        --notify)  COMPREPLY=($(compgen -W "never fail always" -- "$cur")); return ;;
        --workdir) COMPREPLY=($(compgen -d -- "$cur")); return ;;
        --name|--timeout|--until|--note|--by|--days|-n|--lines|--every|--at|--in) return ;;
    esac

    local IFS=$'\n'
    case $verb in
        in|at|every)
            [[ $cur == -* ]] && COMPREPLY=($(compgen -W $'--name\n--workdir\n--timeout\n--catchup\n--until\n--until-done\n--forever\n--now\n--notify\n--note\n--by\n--' -- "$cur")) ;;
        list)  COMPREPLY=($(compgen -W $'--all\n--json' -- "$cur")) ;;
        prune) COMPREPLY=($(compgen -W '--days' -- "$cur")) ;;
        log)
            if [[ $cur == -* ]]; then
                COMPREPLY=($(compgen -W $'--lines\n--follow' -- "$cur"))
            elif ((COMP_CWORD == 2)); then
                COMPREPLY=($(compgen -W "$(crontinental names 2>/dev/null)" -- "$cur"))
            fi ;;
        edit)
            if [[ $cur == -* ]]; then
                COMPREPLY=($(compgen -W $'--every\n--at\n--in\n--until\n--no-until\n--until-done\n--no-until-done\n--forever\n--workdir\n--timeout\n--catchup\n--notify\n--note\n--' -- "$cur"))
            elif ((COMP_CWORD == 2)); then
                COMPREPLY=($(compgen -W "$(crontinental names 2>/dev/null)" -- "$cur"))
            fi ;;
        show|run|rm)
            ((COMP_CWORD == 2)) && COMPREPLY=($(compgen -W "$(crontinental names 2>/dev/null)" -- "$cur")) ;;
        enable)
            ((COMP_CWORD == 2)) && COMPREPLY=($(compgen -W "$(crontinental names --disabled 2>/dev/null)" -- "$cur")) ;;
        disable)
            ((COMP_CWORD == 2)) && COMPREPLY=($(compgen -W "$(crontinental names --enabled 2>/dev/null)" -- "$cur")) ;;
    esac
}
complete -F _crontinental crontinental
