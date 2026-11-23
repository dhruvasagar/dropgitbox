#!/bin/sh
# dropgitbox installer.
#
#   curl -fsSL https://raw.githubusercontent.com/dhruvasagar/dropgitbox/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --service           # + client sync daemon
#   curl -fsSL .../install.sh | sudo sh -s -- --server       # server node (Linux, systemd)
#
# Options:
#   --version VERSION   install a specific release (e.g. v0.1.0); default: latest
#   --bin-dir DIR       where to put the binary
#                       (default: /usr/local/bin as root, ~/.local/bin otherwise)
#   --service           install and start the per-user sync daemon
#                       (systemd user unit on Linux, launchd agent on macOS)
#   --server            set up this machine as a server node: system service,
#                       /etc/dropgitbox/config.toml, data in /var/lib/dropgitbox
#   --public-url URL    with --server: the URL devices use to reach it
#
# Environment:
#   DROPGITBOX_DOWNLOAD_BASE  download from here instead of GitHub releases
#                             (a URL or file:// directory holding the archives)
set -eu

REPO="dhruvasagar/dropgitbox"
VERSION="latest"
BIN_DIR=""
SERVICE=0
SERVER=0
PUBLIC_URL=""

say() { printf '%s\n' "$*"; }
err() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || err "required command not found: $1"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--version) VERSION="${2:?--version needs a value}"; shift 2 ;;
	--bin-dir) BIN_DIR="${2:?--bin-dir needs a value}"; shift 2 ;;
	--service) SERVICE=1; shift ;;
	--server) SERVER=1; shift ;;
	--public-url) PUBLIC_URL="${2:?--public-url needs a value}"; shift 2 ;;
	-h | --help) sed -n '2,22p' "$0" 2>/dev/null || true; exit 0 ;;
	*) err "unknown option: $1" ;;
	esac
done

need uname
need tar
need curl

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
linux | darwin) ;;
*) err "unsupported OS: $os (Linux and macOS are supported)" ;;
esac
arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) err "unsupported architecture: $arch" ;;
esac

uid=$(id -u)
if [ "$SERVER" = 1 ]; then
	[ "$os" = linux ] || err "--server needs Linux with systemd"
	[ "$uid" = 0 ] || err "--server must run as root (pipe to 'sudo sh -s -- --server')"
	[ -d /run/systemd/system ] || err "--server needs systemd"
fi
if [ "$SERVICE" = 1 ] && [ "$uid" = 0 ]; then
	err "--service installs a per-user daemon; run it as the user who owns the folders, not root"
fi

if [ -z "$BIN_DIR" ]; then
	if [ "$uid" = 0 ]; then BIN_DIR=/usr/local/bin; else BIN_DIR="$HOME/.local/bin"; fi
fi

# ---- resolve version --------------------------------------------------------
if [ "$VERSION" = latest ] && [ -z "${DROPGITBOX_DOWNLOAD_BASE:-}" ]; then
	VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
	[ -n "$VERSION" ] || err "could not determine the latest release"
fi
[ "$VERSION" != latest ] || err "set --version when using DROPGITBOX_DOWNLOAD_BASE"
num=${VERSION#v}
base=${DROPGITBOX_DOWNLOAD_BASE:-"https://github.com/$REPO/releases/download/$VERSION"}
archive="dropgitbox_${num}_${os}_${arch}.tar.gz"

# ---- download and verify ----------------------------------------------------
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading dropgitbox $VERSION ($os/$arch)..."
curl -fsSL -o "$tmp/$archive" "$base/$archive" || err "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || err "download failed: $base/checksums.txt"

want=$(awk -v f="$archive" '$2 == f || $2 == "*"f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || err "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
else
	need shasum
	got=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
fi
[ "$want" = "$got" ] || err "checksum mismatch for $archive (expected $want, got $got)"

mkdir -p "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x"
[ -f "$tmp/x/dropgitbox" ] || err "archive does not contain the dropgitbox binary"

# ---- install binary ---------------------------------------------------------
mkdir -p "$BIN_DIR"
install -m 0755 "$tmp/x/dropgitbox" "$BIN_DIR/dropgitbox"
say "Installed $("$BIN_DIR/dropgitbox" version) to $BIN_DIR/dropgitbox"

case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*) say "NOTE: $BIN_DIR is not on your PATH; add it, e.g.: export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac
command -v git >/dev/null 2>&1 || say "WARNING: git is not installed; dropgitbox needs it (git 2.28+)."

# ---- client service ---------------------------------------------------------
if [ "$SERVICE" = 1 ]; then
	if [ "$os" = linux ]; then
		need systemctl
		unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
		mkdir -p "$unit_dir"
		sed "s|/usr/bin/dropgitbox|$BIN_DIR/dropgitbox|" \
			"$tmp/x/packaging/systemd/dropgitbox.service" >"$unit_dir/dropgitbox.service"
		systemctl --user daemon-reload
		systemctl --user enable --now dropgitbox.service
		say "Started the sync daemon: systemctl --user status dropgitbox"
		say "  logs: journalctl --user -u dropgitbox -f"
	else
		agents="$HOME/Library/LaunchAgents"
		logs="$HOME/Library/Logs"
		label=com.github.dhruvasagar.dropgitbox
		mkdir -p "$agents" "$logs"
		sed -e "s|@BINDIR@|$BIN_DIR|" -e "s|@LOGDIR@|$logs|" \
			"$tmp/x/packaging/launchd/$label.plist" >"$agents/$label.plist"
		launchctl bootout "gui/$uid/$label" 2>/dev/null || true
		launchctl bootstrap "gui/$uid" "$agents/$label.plist"
		say "Started the sync daemon (launchd agent $label)."
		say "  logs: tail -f $logs/dropgitbox.log"
	fi
fi

# ---- server node ------------------------------------------------------------
if [ "$SERVER" = 1 ]; then
	mkdir -p /etc/dropgitbox
	if [ ! -f /etc/dropgitbox/config.toml ]; then
		install -m 0644 "$tmp/x/packaging/config.server.toml" /etc/dropgitbox/config.toml
	fi
	if [ -n "$PUBLIC_URL" ]; then
		if grep -q '^public_url' /etc/dropgitbox/config.toml; then
			sed -i "s|^public_url.*|public_url = \"$PUBLIC_URL\"|" /etc/dropgitbox/config.toml
		else
			sed -i "s|^# public_url.*|public_url = \"$PUBLIC_URL\"|" /etc/dropgitbox/config.toml
		fi
	fi
	sed "s|/usr/bin/dropgitbox|$BIN_DIR/dropgitbox|" \
		"$tmp/x/packaging/systemd/dropgitbox-server.service" >/etc/systemd/system/dropgitbox-server.service
	systemctl daemon-reload
	systemctl enable --now dropgitbox-server.service
	say ""
	say "Server running: systemctl status dropgitbox-server  (config: /etc/dropgitbox/config.toml)"
	grep -q '^public_url' /etc/dropgitbox/config.toml ||
		say "Set public_url in /etc/dropgitbox/config.toml (the URL devices will use), then:"
	say "Register your first device:  sudo dropgitbox device add laptop"
fi

if [ "$SERVICE" = 0 ] && [ "$SERVER" = 0 ]; then
	say ""
	say "Next steps:"
	say "  server node:   sudo sh install.sh --server   (or see the README)"
	say "  client device: dropgitbox join <invite>; dropgitbox setup -repo NAME -dir ~/NAME"
	say "                 then re-run with --service, or run: dropgitbox watch"
fi
