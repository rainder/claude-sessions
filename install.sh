#!/usr/bin/env bash
# Installer for claude-sessions. Downloads the latest release binary for the
# current OS/arch, verifies its SHA256 against the release manifest, and
# installs to ~/.local/bin (override via INSTALL_DIR=/path).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/rainder/claude-sessions/main/install.sh | bash
#   curl -fsSL https://.../install.sh | VERSION=v1.0.0 bash
#   curl -fsSL https://.../install.sh | INSTALL_DIR=/usr/local/bin bash

set -euo pipefail

REPO="rainder/claude-sessions"
BIN_NAME="claude-sessions"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${VERSION:-latest}"

err() { echo "error: $*" >&2; exit 1; }

command -v curl >/dev/null || err "curl is required"

case "$(uname -s)" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux"  ;;
  *) err "unsupported OS: $(uname -s) (supported: Darwin, Linux)" ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) err "unsupported arch: $(uname -m) (supported: x86_64, arm64)" ;;
esac

case "$OS/$ARCH" in
  darwin/arm64|linux/amd64|linux/arm64) ;;
  *) err "no prebuilt binary for $OS/$ARCH (build from source: make install)" ;;
esac

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
  [ -n "$VERSION" ] || err "could not resolve latest version from GitHub API"
fi

ASSET="${BIN_NAME}-${OS}-${ARCH}"
URL_BASE="https://github.com/$REPO/releases/download/$VERSION"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "==> downloading $ASSET ($VERSION)"
curl -fSL --progress-bar "$URL_BASE/$ASSET"      -o "$TMPDIR/$ASSET"
curl -fsSL                "$URL_BASE/SHA256SUMS" -o "$TMPDIR/SHA256SUMS"

echo "==> verifying checksum"
expected=$(awk -v a="$ASSET" '$2 == a { print $1 }' "$TMPDIR/SHA256SUMS")
[ -n "$expected" ] || err "no SHA256 entry for $ASSET in SHA256SUMS"
if command -v sha256sum >/dev/null; then
  actual=$(sha256sum "$TMPDIR/$ASSET" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$TMPDIR/$ASSET" | awk '{print $1}')
fi
[ "$expected" = "$actual" ] || err "checksum mismatch (expected $expected, got $actual)"

echo "==> installing to $INSTALL_DIR/$BIN_NAME"
mkdir -p "$INSTALL_DIR"
mv "$TMPDIR/$ASSET" "$INSTALL_DIR/$BIN_NAME"
chmod +x "$INSTALL_DIR/$BIN_NAME"

echo
echo "installed $BIN_NAME $VERSION"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo
    echo "note: $INSTALL_DIR is not in your PATH. Add this to your shell rc:"
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac
