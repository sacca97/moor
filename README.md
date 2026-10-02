# moor

Keep a shell running after you close the terminal. Detach with Ctrl-\ twice or
Ctrl-b d, attach again later from any terminal.

## Install

Linux (amd64, arm64) and Apple Silicon macOS:

```sh
curl -fsSL https://github.com/sacca97/moor/releases/latest/download/install.sh | sh
```

This puts `moor` in `~/.local/bin` (set `MOOR_INSTALL_DIR` to change that). The
script is generated for each release with the SHA-256 of every archive built
into it, and refuses to install a download that does not match.

To install a specific version, use that release's script:

```sh
curl -fsSL https://github.com/sacca97/moor/releases/download/0.1.0/install.sh | sh
```

From source (needs Go):

```sh
git clone https://github.com/sacca97/moor && cd moor
make install              # PREFIX=/usr/local to change the location
```

Run `moor --help` to get started.
