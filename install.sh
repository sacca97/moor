#!/bin/sh
# Install or update moor from a GitHub release. Running it again installs the
# release it belongs to over the existing binary.
#
#   curl -fsSL https://github.com/sacca97/moor/releases/latest/download/install.sh | sh
#
# This file is a template. "make dist" writes the copy attached to each
# release with that release's version and the SHA-256 of every archive filled
# in below, so the script only ever installs the exact build it was made for
# and refuses anything else.
#
# Environment:
#   MOOR_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   MOOR_BASE_URL     override the download URL (mirrors, testing)

set -eu

REPO="sacca97/moor"
VERSION="@VERSION@"
CHECKSUMS="@CHECKSUMS@"
INSTALL_DIR="${MOOR_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf 'moor-install: %s\n' "$*"; }
die() { printf 'moor-install: error: %s\n' "$*" >&2; exit 1; }

# Everything runs inside main, called on the last line, so a download that
# is cut short when piped into sh never executes half a script.
main() {
    # download URL FILE
    if command -v curl >/dev/null 2>&1; then
        download() { curl -fsSL "$1" -o "$2"; }
    elif command -v wget >/dev/null 2>&1; then
        download() { wget -qO "$2" "$1"; }
    else
        die "curl or wget is required"
    fi

    if command -v sha256sum >/dev/null 2>&1; then
        sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
    elif command -v shasum >/dev/null 2>&1; then
        sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
    else
        die "sha256sum or shasum is required"
    fi

    case "$(uname -s)" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) die "unsupported OS: $(uname -s) (moor supports Linux and macOS)" ;;
    esac
    case "$(uname -m)" in
        x86_64 | amd64) arch=amd64 ;;
        aarch64 | arm64) arch=arm64 ;;
        *) die "unsupported architecture: $(uname -m)" ;;
    esac
    [ "$os-$arch" != darwin-amd64 ] || die "Intel Macs are not supported"

    case "$VERSION" in
        @*) die "this is the unreleased template; use the install.sh attached to a release" ;;
    esac
    base="${MOOR_BASE_URL:-https://github.com/$REPO/releases/download/$VERSION}"

    old="$("$INSTALL_DIR/moor" --version 2>/dev/null || true)"
    if [ "$old" = "moor $VERSION" ]; then
        say "moor $VERSION is already installed in $INSTALL_DIR"
        return 0
    fi

    asset="moor_${os}_$arch.tar.gz"
    want="$(printf '%s\n' "$CHECKSUMS" | awk -v f="$asset" '$2 == f { print $1 }')"
    [ -n "$want" ] || die "no checksum for $asset in this release"

    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT INT TERM

    say "downloading $asset ($VERSION)"
    download "$base/$asset" "$tmp/$asset" || die "could not download $base/$asset"
    # The expected hash is part of this script, not fetched with the archive.
    [ "$(sha256 "$tmp/$asset")" = "$want" ] || die "checksum mismatch for $asset"

    tar -xzf "$tmp/$asset" -C "$tmp" moor || die "could not extract $asset"
    mkdir -p "$INSTALL_DIR"
    # Move into place atomically; replacing a running binary in place would
    # fail. The temporary file is created exclusively, under an unpredictable
    # name, in the target directory so the final mv is a rename.
    new="$(mktemp "$INSTALL_DIR/.moor.XXXXXX")"
    trap 'rm -rf "$tmp" "$new"' EXIT INT TERM
    cp "$tmp/moor" "$new"
    chmod 755 "$new"
    mv -f "$new" "$INSTALL_DIR/moor"

    if [ -n "$old" ]; then
        say "updated ${old#moor } -> $VERSION in $INSTALL_DIR"
    else
        say "installed moor $VERSION to $INSTALL_DIR/moor"
    fi
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) ;;
        *) say "add $INSTALL_DIR to your PATH to run it" ;;
    esac
}

main
