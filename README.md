# moor

You moor a shell so it stays put while you're away. `moor` starts your normal
shell inside a persistent PTY. Press **Ctrl-\ twice** to detach, and attach to
it again later from any terminal. The shell and everything running in it keep
going in the meantime.

It is not a terminal multiplexer. It has no panes, windows, status bar, copy
mode or config. Your terminal emulator still handles rendering, scrollback,
selection, search and the clipboard.

## Install

```sh
go install github.com/sacca/moor/cmd/moor@latest
# or, from a checkout:
make install              # installs to ~/.local/bin (PREFIX=/usr/local to change)
```

## Usage

```sh
moor                          # new shell, attached
moor -n work                  # named shell
moor start codex              # shell that runs codex, attached
moor start "cd ~/project && codex"
moor run codex                # same, but left detached
moor -n work run codex

moor ps                       # list sessions
moor attach 0                 # attach by ID ...
moor -a work                  # ... or by name
moor kill work                # terminate a session
```

`start` and `run` type the command into an interactive shell instead of
`exec`ing it, so the shell is still there when the program exits.

Session IDs are the lowest free integers. Names come from `-n`, otherwise
from the command (`cd ~/x && codex` becomes `codex`), otherwise `shell`.
Names are unique among running sessions.

## Inside a session

The prompt gets a dim `[moor]` prefix (`[moor:name]` for named or
command sessions), so you can tell you are inside a moored shell:

```text
[moor:work] ➜  project
```

This works for zsh, bash and fish. Your own startup files (`.zshrc`,
`.bashrc`, `config.fish`) still load normally, and the marker is added after
them, so themes, plugins and history behave as usual. Set `MOOR_PROMPT=0` to
turn it off. Every session also exports `MOOR_SESSION` (the ID) and
`MOOR_SESSION_NAME`, which you can use in a custom prompt or with other
shells.

Attaching clears the terminal's screen and scrollback first, then replays the
session's own output (the last 8 MiB). A new session starts on a blank screen,
and reattaching never mixes the session with what was on the terminal before.

## Detaching

Press Ctrl-\ twice within 400 ms to detach. A single Ctrl-\ still reaches the
program, after that delay. Every other key, Esc included, passes through
immediately. The detach key also works when a program such as Codex has turned
on the kitty keyboard protocol or xterm's modifyOtherKeys. It is ignored inside
bracketed pastes.

Only one client can be attached to a session at a time.

## How it works

Each session has its own background server (`moor __serve`). The server owns
the PTY and the shell and listens on a Unix socket in
`$XDG_RUNTIME_DIR/moor/<id>/` (or `/tmp/moor-$UID/<id>/`). It keeps
reading the PTY when nobody is attached, so programs never block, and keeps
the last 8 MiB of output. That output is replayed when you attach, so it ends
up in your terminal's own scrollback.

When the shell exits, the session and its directory go away. A directory
whose server no longer responds is removed the next time moor scans for
sessions.

## Tests

```sh
go test ./...
```

The integration tests in `cmd/moor` build the binary and drive it through
real PTYs.
