package main

import (
	"fmt"
	"strings"
)

var subcommands = map[string][]string{
	"inbox":   {"rm", "clear", "hold", "resume"},
	"token":   {"create", "ls", "rm"},
	"drive":   {"ls", "rm"},
	"service": {"ls", "status", "logs"},
	"creds":   {"set"},
}

func runCompletion(_ *globals, args []string) error {
	if len(args) != 1 {
		return usageError("completion bash|zsh")
	}
	var names []string
	for _, c := range commands() {
		names = append(names, c.name)
	}
	var cases strings.Builder
	for _, c := range sortedKeysOf(subcommands) {
		fmt.Fprintf(&cases, "    %s) words=%q ;;\n", c, strings.Join(subcommands[c], " "))
	}
	switch args[0] {
	case "bash":
		fmt.Printf(`# a12s bash completion: source <(a12s completion bash)
_a12s() {
  local cur=${COMP_WORDS[COMP_CWORD]} words
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=($(compgen -W %q -- "$cur")); return
  fi
  case ${COMP_WORDS[1]} in
%s    *) words="--fleet --json" ;;
  esac
  COMPREPLY=($(compgen -W "$words" -- "$cur"))
}
complete -F _a12s a12s
`, strings.Join(names, " "), cases.String())
	case "zsh":
		fmt.Printf(`#compdef a12s
# a12s zsh completion: source <(a12s completion zsh)
_a12s() {
  local words_
  if (( CURRENT == 2 )); then
    compadd -- %s; return
  fi
  case ${words[2]} in
%s    *) words="--fleet --json" ;;
  esac
  compadd -- ${=words}
}
compdef _a12s a12s
`, strings.Join(names, " "), strings.ReplaceAll(cases.String(), "words=", "words="))
	default:
		return usageError("completion bash|zsh")
	}
	return nil
}

func sortedKeysOf(m map[string][]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
