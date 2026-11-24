# Configuration

dropgitbox reads one TOML file, with a table for the server node, one for
devices, one for notifications, and a list of synced folders. A machine
only needs the parts for its role. You rarely have to edit it by hand:
`join` and `setup` fill in the device parts, and the server package ships
a minimal server config.
[`packaging/config.example.toml`](../packaging/config.example.toml)
lists every setting with comments.

## Where it's read from

The first of these that exists is used:

1. `--config FILE`, or `$DROPGITBOX_CONFIG`
2. `$XDG_CONFIG_HOME/dropgitbox/config.toml`, by default
   `~/.config/dropgitbox/config.toml` (on macOS too)
3. `$XDG_CONFIG_DIRS/dropgitbox/config.toml`, by default
   `/etc/xdg/dropgitbox/config.toml`
4. `/etc/dropgitbox/config.toml`, the packaged server's config

`dropgitbox config` shows which file is used, and where `join` and `setup`
save settings: the file in use, unless it's one of the system-wide files
(3 or 4), in which case they write your own `~/.config/dropgitbox/config.toml`.
An explicit `--config` file is always written to.

**Precedence:** a command-line flag beats an environment variable, which
beats the config file, which beats the default.

**Paths** in the file may use `~/` and environment variables (`$HOME`,
systemd's `$CREDENTIALS_DIRECTORY`), and relative paths are relative to the
config file's directory.

**Unknown keys are rejected**, since they're almost always typos.

## Example

```toml
[server]                          # server node
addr = "127.0.0.1:8080"
public_url = "https://box.example.com"

[client]                          # written by `dropgitbox join`
server = "https://box.example.com"
device = "laptop"

[[folders]]                       # added by `dropgitbox setup`
path = "~/Box"
repo = "Box"

[[folders]]
path = "~/Documents/notes"
repo = "notes"

[notify]
command = ["~/bin/dropgitbox-hook"]
```

## `[server]`

Used by `dropgitbox server` and `dropgitbox device`.

| Key | Default | Meaning |
| --- | --- | --- |
| `addr` | `":8080"` | Address to listen on. `127.0.0.1:8080` behind a reverse proxy on the same machine |
| `public_url` | | The URL devices use to reach the server, put into every invite. Must be reachable from the devices, through any proxy or VPN |
| `data_dir` | `$XDG_DATA_HOME/dropgitbox` (`~/.local/share/dropgitbox`) | Repositories, devices and activity. The packaged server uses `/var/lib/dropgitbox` |
| `tls_cert`, `tls_key` | | Serve HTTPS directly with this certificate and key. Set both or neither |
| `trusted_proxies` | loopback | Reverse proxies whose `X-Forwarded-For` is believed, as IPs or CIDRs. See [security](security.md#real-client-addresses) |
| `token_file` | | File holding an optional shared token, accepted besides device tokens. At least 24 characters |
| `token` | | The shared token inline. Prefer `token_file`, or better, per-device tokens |
| `no_auth` | `false` | Turn off authentication entirely. Only on a network where you trust everyone |

## `[client]`

Used on devices. `dropgitbox join` sets `server` and `device`.

| Key | Default | Meaning |
| --- | --- | --- |
| `server` | | The server's URL |
| `device` | the hostname | This device's name, recorded in commits |
| `token_file` | `token` next to the config file | File holding the device token |
| `token` | | The token inline. Not recommended: the config then can't be shared |
| `debounce` | `"2s"` | How long a folder must be quiet before changes are committed |
| `max_delay` | `"30s"` | The longest a burst of changes can postpone a commit |
| `poll` | `"5m"` | Sync at least this often, in case a notice from the server was missed. Also caps the retry backoff |

Durations are written like `"500ms"`, `"10s"`, `"5m"` or `"1h"`.

## `[[folders]]`

One entry per synced folder; `dropgitbox setup` adds them. The daemon syncs
every entry, sets up any that don't exist on this device yet, and follows
changes to the list while it runs.

| Key | Meaning |
| --- | --- |
| `path` | Where the folder is on this device. Use `~/` so the file works for other users and machines |
| `repo` | The repository on the server |

Since the file holds no secrets, the list can live in a dotfiles repository
and bring every folder to each new device.

## `[notify]`

Notifications from the sync daemon. See
[notifications and status](notifications.md).

| Key | Default | Meaning |
| --- | --- | --- |
| `desktop` | `true` | Show desktop notifications |
| `desktop_events` | `["conflict", "resolved", "offline", "online", "unauthorized"]` | Which events pop up |
| `offline_after` | `"10m"` | How long syncing must keep failing before `offline` is reported |
| `command` | | A program to run for events, as a list: the program, then its arguments. Run directly, not through a shell |
| `command_events` | the desktop events, plus `remote_changes`, `synced` and `error` | Which events run `command` |

Event lists may use `"*"` for every event.

## Tokens and file permissions

The device token is kept in its own file, `~/.config/dropgitbox/token`,
written by `join` with mode 0600, so that `config.toml` can be shared. A
token file that other users can read is refused, with the `chmod` command
that fixes it. A config file that holds an inline token and that others can
read gets a warning.

## Environment variables

| Variable | Meaning |
| --- | --- |
| `DROPGITBOX_CONFIG` | The config file, unless `--config` is given |
| `DROPGITBOX_TOKEN` | The device token (on a device) or the shared token (on the server), instead of the config's |
| `DROPGITBOX_SOCKET` | Where the daemon's control socket is |
| `XDG_CONFIG_HOME`, `XDG_CONFIG_DIRS` | Where config files are looked for |
| `XDG_DATA_HOME` | The default server data directory |
| `XDG_RUNTIME_DIR`, `XDG_STATE_HOME` | Where the daemon's socket goes |

## Files

| Path | What |
| --- | --- |
| `~/.config/dropgitbox/config.toml` | Your config |
| `~/.config/dropgitbox/token` | The device token |
| `/etc/dropgitbox/config.toml` | The packaged server's config |
| `/var/lib/dropgitbox`, `~/.local/share/dropgitbox` | Server data (see [running the server](server.md#data-and-backups)) |
| `FOLDER/.git` | Each synced folder's repository and its dropgitbox settings |
| daemon socket | The first of: `$DROPGITBOX_SOCKET`; `$XDG_RUNTIME_DIR/dropgitbox/daemon.sock`; `$XDG_STATE_HOME/dropgitbox/daemon.sock`; `~/.local/state/dropgitbox/daemon.sock`; a private directory in the system's temporary directory. A derived path too long for a Unix socket (over 103 bytes) also falls back to the temporary directory; a `$DROPGITBOX_SOCKET` that long is an error |
