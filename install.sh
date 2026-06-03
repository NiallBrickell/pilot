#!/bin/sh
# pilot bootstrap installer.
# Downloads the latest release binary for this OS/arch into ~/.pilot/bin and
# starts pilot. Re-run `pilot upgrade` later to update.
#
#   curl -fsSL https://raw.githubusercontent.com/erdoai/pilot/main/install.sh | sh
set -eu

REPO="erdoai/pilot"
PILOT_HOME="${PILOT_HOME:-$HOME/.pilot}"
BIN_DIR="$PILOT_HOME/bin"

os="$(uname -s)"
case "$os" in
	Darwin) os="darwin" ;;
	Linux)  os="linux" ;;
	*) echo "pilot: unsupported OS '$os' (darwin and linux only)" >&2; exit 1 ;;
esac

arch="$(uname -m)"
case "$arch" in
	arm64|aarch64) arch="arm64" ;;
	x86_64|amd64)  arch="amd64" ;;
	*) echo "pilot: unsupported architecture '$arch' (arm64 and amd64 only)" >&2; exit 1 ;;
esac

asset="pilot-${os}-${arch}"
url="https://github.com/${REPO}/releases/latest/download/${asset}"

echo "Downloading ${asset} from ${REPO}…"
mkdir -p "$BIN_DIR"
tmp="$BIN_DIR/.download.$$"
trap 'rm -f "$tmp"' EXIT
curl -fsSL -o "$tmp" "$url"
chmod +x "$tmp"
# Atomic swap — never overwrite a running binary in place.
mv -f "$tmp" "$BIN_DIR/pilot"
trap - EXIT
echo "Installed pilot to $BIN_DIR/pilot"

# `pilot start` requires an Anthropic API key. Surface a clear message rather
# than letting start fail cryptically.
if [ -z "${ANTHROPIC_API_KEY:-}" ] && ! grep -qs ANTHROPIC_API_KEY "$PILOT_HOME/.env" 2>/dev/null; then
	cat >&2 <<EOF

pilot is installed but not started: no ANTHROPIC_API_KEY found.
Set it, then run '$BIN_DIR/pilot start':

  echo 'ANTHROPIC_API_KEY=sk-ant-...' > "$PILOT_HOME/.env"
  "$BIN_DIR/pilot" start

Add "$BIN_DIR" to your PATH to call 'pilot' directly.
EOF
	exit 0
fi

"$BIN_DIR/pilot" start
echo
echo "Done. Add $BIN_DIR to your PATH to call 'pilot' directly."
