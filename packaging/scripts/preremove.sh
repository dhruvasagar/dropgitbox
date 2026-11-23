#!/bin/sh
set -e
# Stop the server on package removal (not on upgrade). Data in
# /var/lib/dropgitbox is left in place.
case "$1" in
	remove|0)
		if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
			systemctl disable --now dropgitbox-server.service >/dev/null 2>&1 || true
		fi
		;;
esac
