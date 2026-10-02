package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sacca/moor/internal/session"
)

// The prompt marker is a dim "[moor:name] " (or "[moor] " for an unnamed
// session) prefixed to the shell's prompt, so a shell inside moor is
// recognizable at a glance. It is added by wrapping the shell's startup files: the user's own configuration is loaded unchanged
// first, then a prompt hook re-adds the marker before every prompt (so themes
// that rebuild the prompt each time keep it). Set MOOR_PROMPT=0 to disable.
//
// Supported shells are zsh, bash and fish; others start unmodified and can
// use $MOOR_SESSION / $MOOR_SESSION_NAME in their own prompt.

// zsh reads its startup files from $ZDOTDIR, so ZDOTDIR points at a directory
// of wrappers that restore the user's ZDOTDIR around sourcing the real files.
const zshenv = `_moor_zd=$ZDOTDIR
if [[ -n $MOOR_USER_ZDOTDIR ]]; then ZDOTDIR=$MOOR_USER_ZDOTDIR; else unset ZDOTDIR; fi
[[ -r ${ZDOTDIR:-$HOME}/.zshenv ]] && source ${ZDOTDIR:-$HOME}/.zshenv
MOOR_USER_ZDOTDIR=${ZDOTDIR-}
ZDOTDIR=$_moor_zd
unset _moor_zd
`

// venvSh re-activates the virtualenv moor was started from, after the user's
// startup files have run. The environment is inherited as is, but activation
// is more than variables: the files may put other directories ahead of the
// venv on PATH, and the shell never saw "deactivate" or the prompt prefix.
const venvSh = `if [ -n "${VIRTUAL_ENV-}" ] && [ -r "$VIRTUAL_ENV/bin/activate" ]; then . "$VIRTUAL_ENV/bin/activate"; fi
`

// Each shell's rc is the user's own files, the venv hook, and (unless
// MOOR_PROMPT=0) the prompt marker, in that order.
const zshrcHead = `if [[ -n $MOOR_USER_ZDOTDIR ]]; then ZDOTDIR=$MOOR_USER_ZDOTDIR; else unset ZDOTDIR; fi
unset MOOR_USER_ZDOTDIR
[[ -r ${ZDOTDIR:-$HOME}/.zshrc ]] && source ${ZDOTDIR:-$HOME}/.zshrc
` + venvSh

const zshrcPrompt = `typeset -g _moor_mark=$'%{\e[2m%}[@MARK@]%{\e[22m%} '
_moor_prompt() { [[ $PROMPT == *$_moor_mark* ]] || PROMPT="$_moor_mark$PROMPT" }
precmd_functions+=(_moor_prompt)
`

const bashrcHead = `[ -r ~/.bashrc ] && . ~/.bashrc
` + venvSh

const bashrcPrompt = `_moor_mark='\[\e[2m\][@MARK@]\[\e[22m\] '
_moor_prompt() { case "$PS1" in *"$_moor_mark"*) ;; *) PS1="$_moor_mark$PS1" ;; esac; }
if [[ "$(declare -p PROMPT_COMMAND 2>/dev/null)" == "declare -a"* ]]; then
  PROMPT_COMMAND+=(_moor_prompt)
else
  PROMPT_COMMAND="${PROMPT_COMMAND:+$PROMPT_COMMAND$'\n'}_moor_prompt"
fi
`

const fishHead = `if set -q VIRTUAL_ENV; and test -r $VIRTUAL_ENV/bin/activate.fish
    source $VIRTUAL_ENV/bin/activate.fish
end
`

const fishPrompt = `if functions -q fish_prompt
    functions -c fish_prompt _moor_orig_prompt
else
    function _moor_orig_prompt; end
end
function _moor_status; return $argv[1]; end
function fish_prompt
    set -l s $status
    set_color --dim; printf '[@MARK@] '; set_color normal
    _moor_status $s
    _moor_orig_prompt
end
`

// shellCommand returns the command that starts shell interactively, wrapped
// so that it shows the prompt marker for session name (unless MOOR_PROMPT=0)
// and re-activates the virtualenv moor was started from, if any. dir is the
// session's runtime directory, where the wrapper files are written.
func shellCommand(shell, name, dir string) *exec.Cmd {
	plain := exec.Command(shell)
	marker := os.Getenv("MOOR_PROMPT") != "0" && name != ""
	if !marker && os.Getenv("VIRTUAL_ENV") == "" {
		return plain
	}
	mark := "moor"
	if !session.IsDefaultName(name) {
		mark += ":" + name
	}
	// rc builds a shell's startup file from its parts.
	rc := func(head, prompt string) []byte {
		if marker {
			head += prompt
		}
		return []byte(strings.ReplaceAll(head, "@MARK@", mark))
	}

	switch strings.TrimPrefix(filepath.Base(shell), "-") {
	case "zsh":
		zdir := filepath.Join(dir, "zsh")
		if os.Mkdir(zdir, 0o700) != nil ||
			os.WriteFile(filepath.Join(zdir, ".zshenv"), []byte(zshenv), 0o600) != nil ||
			os.WriteFile(filepath.Join(zdir, ".zshrc"), rc(zshrcHead, zshrcPrompt), 0o600) != nil {
			return plain
		}
		cmd := exec.Command(shell)
		cmd.Env = []string{"ZDOTDIR=" + zdir}
		if zd, ok := os.LookupEnv("ZDOTDIR"); ok {
			cmd.Env = append(cmd.Env, "MOOR_USER_ZDOTDIR="+zd)
		}
		return cmd
	case "bash":
		path := filepath.Join(dir, "bashrc")
		if os.WriteFile(path, rc(bashrcHead, bashrcPrompt), 0o600) != nil {
			return plain
		}
		return exec.Command(shell, "--rcfile", path)
	case "fish":
		path := filepath.Join(dir, "prompt.fish")
		if os.WriteFile(path, rc(fishHead, fishPrompt), 0o600) != nil {
			return plain
		}
		return exec.Command(shell, "-C", "source '"+strings.ReplaceAll(path, "'", `\'`)+"'")
	}
	return plain
}
