#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Install aws-sso-sync from GitHub Releases.

Usage:
  ./scripts/install.sh --repo Oatelaus/aws-sso-sync [--version <vX.Y.Z|latest>] [--install-dir <dir>]

Examples:
  ./scripts/install.sh --repo Oatelaus/aws-sso-sync
  ./scripts/install.sh --repo Oatelaus/aws-sso-sync --version v1.2.0
  ./scripts/install.sh --repo Oatelaus/aws-sso-sync --install-dir "$HOME/.local/bin"
EOF
}

REPO=""
VERSION="latest"
INSTALL_DIR="${HOME}/.local/bin"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      REPO="${2:-}"
      shift 2
      ;;
    --version)
      VERSION="${2:-}"
      shift 2
      ;;
    --install-dir)
      INSTALL_DIR="${2:-}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if [[ -z "$REPO" ]]; then
  echo "--repo is required" >&2
  usage
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required" >&2
  exit 1
fi

OS_RAW="$(uname -s)"
ARCH_RAW="$(uname -m)"

case "$OS_RAW" in
  Linux) OS="linux" ;;
  Darwin) OS="darwin" ;;
  MINGW*|MSYS*|CYGWIN*) OS="windows" ;;
  *)
    echo "unsupported OS: $OS_RAW" >&2
    exit 1
    ;;
esac

case "$ARCH_RAW" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *)
    echo "unsupported architecture: $ARCH_RAW" >&2
    exit 1
    ;;
esac

EXT="tar.gz"
if [[ "$OS" == "windows" ]]; then
  EXT="zip"
fi

if [[ "$VERSION" == "latest" ]]; then
  API_URL="https://api.github.com/repos/${REPO}/releases/latest"
  VERSION="$(curl -fsSL "$API_URL" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
  if [[ -z "$VERSION" ]]; then
    echo "unable to resolve latest release tag for ${REPO}" >&2
    exit 1
  fi
fi

VERSION_NO_V="${VERSION#v}"
ASSET="aws-sso-sync_${VERSION_NO_V}_${OS}_${ARCH}.${EXT}"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"

echo "Installing ${REPO} ${VERSION} (${OS}/${ARCH})"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

archive_path="$tmpdir/$ASSET"
curl -fL "$URL" -o "$archive_path"

mkdir -p "$INSTALL_DIR"

if [[ "$EXT" == "zip" ]]; then
  if ! command -v unzip >/dev/null 2>&1; then
    echo "unzip is required for Windows archives" >&2
    exit 1
  fi
  unzip -q "$archive_path" -d "$tmpdir"
  bin_path="$tmpdir/aws-sso-sync.exe"
  install -m 0755 "$bin_path" "$INSTALL_DIR/aws-sso-sync.exe"
  echo "Installed to $INSTALL_DIR/aws-sso-sync.exe"
else
  tar -xzf "$archive_path" -C "$tmpdir"
  bin_path="$tmpdir/aws-sso-sync"
  install -m 0755 "$bin_path" "$INSTALL_DIR/aws-sso-sync"
  echo "Installed to $INSTALL_DIR/aws-sso-sync"
fi

echo "If needed, add this directory to PATH: $INSTALL_DIR"
