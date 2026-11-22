#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="${XDG_BIN_HOME:-$HOME/.local/bin}"
SHARE_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/renderroute"
SYMLINK_DMS=0

for arg in "$@"; do
  case "$arg" in
    --symlink|-s)
      SYMLINK_DMS=1
      ;;
    --help|-h)
      echo "Usage: ./install.sh [--symlink]"
      echo "  Builds renderroute, installs to ~/.local/bin, and installs the DankMaterialShell plugin."
      echo "  --symlink  Symlink dms-plugin/ directly into ~/.config/dms/plugins/renderRoute (for development)."
      exit 0
      ;;
  esac
done

echo "==> Building renderroute..."
cd "$REPO_DIR"
go build -trimpath -ldflags="-s -w" -o renderroute ./cmd/renderroute

echo "==> Installing binary to $BIN_DIR/renderroute..."
mkdir -p "$BIN_DIR" "$SHARE_DIR"
install -m 0755 "$REPO_DIR/renderroute" "$BIN_DIR/renderroute"
rm -rf "$SHARE_DIR/dms-plugin"
cp -r "$REPO_DIR/dms-plugin" "$SHARE_DIR/dms-plugin"

echo "==> Installing DankMaterialShell widget plugin..."
if [[ "$SYMLINK_DMS" -eq 1 ]]; then
  "$BIN_DIR/renderroute" dms install --symlink --source "$REPO_DIR/dms-plugin"
else
  "$BIN_DIR/renderroute" dms install --source "$SHARE_DIR/dms-plugin"
fi

echo
echo "Done! Make sure '$BIN_DIR' is in your PATH."
echo "In DankMaterialShell: Settings -> Plugins -> Scan for plugins -> Enable RenderRoute -> Add to DankBar or Control Center."
