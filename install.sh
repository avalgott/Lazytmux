#!/bin/sh
# lazytmux installer.
#
# Primary path: download the pre-built binary from the latest GitHub release
# (no Go required). Fallback: `go install` from the module directly (requires
# Go 1.25+), which works before the first release has been cut.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/avalgott/Lazytmux/main/install.sh | sh
#
# Override the install directory with LAZYMUX_INSTALL_DIR (default ~/.local/bin).

set -e

REPO="avalgott/Lazytmux"
INSTALL_DIR="${LAZYMUX_INSTALL_DIR:-$HOME/.local/bin}"

detect_platform() {
  OS="$(uname -s)"
  ARCH="$(uname -m)"

  case "$OS" in
    Darwin) OS="darwin" ;;
    Linux)  OS="linux" ;;
    *)      echo "Unsupported OS: $OS" >&2; exit 1 ;;
  esac

  case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *)             echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
  esac

  echo "${OS}_${ARCH}"
}

get_latest_version() {
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name"' \
    | sed 's/.*"tag_name": *"//;s/".*//'
}

install_from_release() {
  PLATFORM="$(detect_platform)"
  VERSION="$(get_latest_version)"

  if [ -z "$VERSION" ]; then
    return 1
  fi

  TARBALL="lazytmux_${VERSION#v}_${PLATFORM}.tar.gz"
  URL="https://github.com/${REPO}/releases/download/${VERSION}/${TARBALL}"

  echo "Installing lazytmux ${VERSION} (${PLATFORM})..."

  TMPDIR="$(mktemp -d)"
  trap 'rm -rf "$TMPDIR"' EXIT

  curl -fsSL "$URL" -o "${TMPDIR}/${TARBALL}"
  tar -xzf "${TMPDIR}/${TARBALL}" -C "$TMPDIR"

  mkdir -p "$INSTALL_DIR"
  install -m 755 "${TMPDIR}/lazytmux" "${INSTALL_DIR}/lazytmux"

  echo "Installed to ${INSTALL_DIR}/lazytmux"
}

install_from_source() {
  echo "No release found — building from source (requires git and Go 1.25+)..."

  if ! command -v go >/dev/null 2>&1 || ! command -v git >/dev/null 2>&1; then
    echo "Error: git and Go 1.25+ are required to build from source." >&2
    echo "Wait for the first release and re-run this script instead." >&2
    exit 1
  fi

  TMPDIR="$(mktemp -d)"
  trap 'rm -rf "$TMPDIR"' EXIT

  # A plain `go install ...@latest` does not work here: the vendored TUI
  # forks (gocui/tcell) use relative replace directives, which the module
  # proxy rejects. Building from a clone sidesteps that — the same way
  # lazyclaude does it.
  git clone --depth 1 "https://github.com/${REPO}.git" "$TMPDIR/src"
  (
    cd "$TMPDIR/src"
    go build -o lazytmux ./cmd/lazytmux
  )

  mkdir -p "$INSTALL_DIR"
  install -m 755 "$TMPDIR/src/lazytmux" "${INSTALL_DIR}/lazytmux"

  echo "Installed to ${INSTALL_DIR}/lazytmux"
}

main() {
  if ! install_from_release; then
    install_from_source
  fi

  if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
    echo ""
    echo "Add to your PATH if not already:"
    echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
  fi
}

main
