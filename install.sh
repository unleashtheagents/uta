#!/usr/bin/env bash
# uta installer — downloads the latest release for your OS+arch from GitHub,
# verifies its sha256, and drops the binary in /usr/local/bin (or
# $HOME/.local/bin if /usr/local/bin isn't writable).
#
# Usage:
#   curl -fsSL https://unleashtheagents.ai/install.sh | sh
#   curl -fsSL https://unleashtheagents.ai/install.sh | UTA_VERSION=v0.2.0 sh
#   curl -fsSL https://unleashtheagents.ai/install.sh | UTA_INSTALL_DIR=$HOME/bin sh

set -eu

REPO="${UTA_REPO:-unleashtheagents/uta}"
VERSION="${UTA_VERSION:-latest}"
INSTALL_DIR="${UTA_INSTALL_DIR:-}"

err() { printf "error: %s\n" "$*" >&2; exit 1; }
note() { printf "%s\n" "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || err "missing required tool: $1"
}
need curl
need tar
need uname

OS_RAW="$(uname -s)"
ARCH_RAW="$(uname -m)"
case "$OS_RAW" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux" ;;
  MINGW*|MSYS*|CYGWIN*) err "windows: please download the zip from https://github.com/${REPO}/releases" ;;
  *) err "unsupported OS: $OS_RAW" ;;
esac
case "$ARCH_RAW" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) err "unsupported arch: $ARCH_RAW" ;;
esac

if [ "$VERSION" = "latest" ]; then
  note "resolving latest release of ${REPO}…"
  TAG="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | \
    awk -F\" '/"tag_name":/ {print $4; exit}')"
  [ -n "$TAG" ] || err "could not resolve latest tag"
else
  TAG="$VERSION"
fi

VERSION_NO_V="${TAG#v}"
ASSET="uta_${VERSION_NO_V}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
CHECKSUM_URL="https://github.com/${REPO}/releases/download/${TAG}/checksums.txt"

# Choose install dir.
if [ -z "$INSTALL_DIR" ]; then
  if [ -w "/usr/local/bin" ] || [ "$(id -u)" = "0" ]; then
    INSTALL_DIR="/usr/local/bin"
  else
    INSTALL_DIR="${HOME}/.local/bin"
  fi
fi
mkdir -p "$INSTALL_DIR"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

note "downloading $ASSET…"
curl -fsSL -o "$TMP/$ASSET" "$URL" || err "download failed: $URL"
curl -fsSL -o "$TMP/checksums.txt" "$CHECKSUM_URL" || err "checksums download failed"

# Verify sha256.
EXPECTED="$(awk -v a="$ASSET" '$2 == a {print $1; exit}' "$TMP/checksums.txt")"
[ -n "$EXPECTED" ] || err "no checksum entry for $ASSET in checksums.txt"

if command -v shasum >/dev/null 2>&1; then
  GOT="$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')"
elif command -v sha256sum >/dev/null 2>&1; then
  GOT="$(sha256sum "$TMP/$ASSET" | awk '{print $1}')"
else
  err "no sha256 tool available (need shasum or sha256sum)"
fi
[ "$GOT" = "$EXPECTED" ] || err "checksum mismatch for $ASSET (got $GOT, expected $EXPECTED)"

tar -xzf "$TMP/$ASSET" -C "$TMP"
install -m 0755 "$TMP/uta" "$INSTALL_DIR/uta"

note ""
note "installed uta ${TAG} to ${INSTALL_DIR}/uta"
if ! echo ":$PATH:" | grep -q ":$INSTALL_DIR:"; then
  note ""
  note "warning: $INSTALL_DIR is not on your PATH."
  note "add this to your shell profile:"
  note "  export PATH=\"$INSTALL_DIR:\$PATH\""
fi
note ""
note "next: uta doctor"
