#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
fi
cat <<'MSG'
dropgitbox installed.

  Server node:   sudo systemctl enable --now dropgitbox-server
                 (set public_url in /etc/dropgitbox/config.toml first)
                 sudo dropgitbox device add laptop
  Client device: dropgitbox join <invite>
                 dropgitbox setup -repo NAME -dir ~/NAME
                 systemctl --user enable --now dropgitbox

Docs: https://github.com/dhruvasagar/dropgitbox
MSG
