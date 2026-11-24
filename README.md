# dropgitbox

A Dropbox-like folder sync service that uses git behind the scenes. Every
change is **auto-committed before it is synced**, so every device and the
server hold the complete history of every file. Any past version can be
listed and restored, and history can never be rewritten on the server.

One binary, `dropgitbox`, is both the server and the client. You run the
server yourself.

```
 laptop                         server node                    desktop
┌──────────────────┐          ┌───────────────────┐          ┌──────────────────┐
│ ~/Box (git tree) │  push    │ /var/lib/         │  push    │ ~/Box (git tree) │
│ dropgitbox watch │ ───────► │  dropgitbox/      │ ◄─────── │ dropgitbox watch │
│  fsnotify        │ ◄─────── │  Box.git (bare)   │ ───────► │  fsnotify        │
│  auto-commit     │  fetch   │ /events (SSE) ────┼─ notify ►│  auto-commit     │
└──────────────────┘          └───────────────────┘          └──────────────────┘
```

- **Syncs within seconds.** The daemon commits a change once the folder has
  been quiet for 2 seconds, and the server tells every other device about a
  push the moment it lands.
- **Full history, safe by design.** Every version of every file is kept,
  `dropgitbox restore` brings any of them back, and the server refuses
  force-pushes and branch deletion.
- **Conflicts stop and ask.** When two devices change the same lines, that
  folder pauses and tells you. Nothing half-merged is ever synced.
- **Works anywhere.** Devices only make outbound HTTP(S) connections, so
  they work behind NAT. The server can sit on a tailnet, a VPS or a home
  server.
- **Built for a public server.** Per-device revocable tokens, brute-force
  blocking, and a sandboxed systemd service.
- **Tells you what's going on.** Desktop notifications, a one-line status
  for your status bar, a live event stream, and a hook for your own
  scripts.

Linux and macOS, amd64 and arm64. Every machine needs **git 2.28+**.

## Install

```sh
# Any Linux/macOS machine: installs to ~/.local/bin (or /usr/local/bin as root)
curl -fsSL https://raw.githubusercontent.com/dhruvasagar/dropgitbox/main/install.sh | sh

# Debian/Ubuntu, Fedora/RHEL: packages ship systemd units for server and client
sudo apt install ./dropgitbox_<version>_amd64.deb
sudo dnf install ./dropgitbox-<version>-1.x86_64.rpm

# From source
go install github.com/dhruvasagar/dropgitbox/cmd/dropgitbox@latest
```

Packages and archives are on the
[releases page](https://github.com/dhruvasagar/dropgitbox/releases). Both
installers include the `dropgitbox(1)` man page, and the packages also
install shell completion. [Deployment](docs/deployment.md#2-set-up-the-server-node)
lists the installer's options.

## Quick start

To try everything on one machine first, follow the
[local demo](docs/demo.md).

**1. Set up the server node.** Decide how devices will reach it (see
[deployment](docs/deployment.md)), then:

```sh
curl -fsSL https://raw.githubusercontent.com/dhruvasagar/dropgitbox/main/install.sh \
  | sudo sh -s -- --server --public-url https://box.example.com
```

**2. Invite a device.** On the server:

```sh
sudo dropgitbox device add laptop     # prints: dropgitbox join dgb1.eyJ...
```

**3. Connect the device.** On the device, with that invite:

```sh
curl -fsSL https://raw.githubusercontent.com/dhruvasagar/dropgitbox/main/install.sh | sh -s -- --service
dropgitbox join dgb1.eyJ...               # connect to the server
dropgitbox setup --repo Box --dir ~/Box   # sync a folder (created on the server if new)
dropgitbox status                         # check that it's in sync
```

Repeat steps 2 and 3 for every device. The invite holds the device's
token, so send it privately. `dropgitbox --help` lists every command, and
`dropgitbox COMMAND --help` explains each one.

## Documentation

| Guide | Covers |
| --- | --- |
| [How it works](docs/how-it-works.md) | The sync cycle, commits, history protection, offline devices, limitations |
| [Local demo](docs/demo.md) | A server and two devices on one machine, step by step |
| [Deployment](docs/deployment.md) | Installing the server and making it reachable: Tailscale, reverse proxies, TLS, hardening |
| [Running the server](docs/server.md) | Server settings, data and backups, managing devices, monitoring, the HTTP API |
| [Using a device](docs/devices.md) | Joining, choosing folders, the sync daemon, status, tokens, moving and removing folders |
| [Conflicts](docs/conflicts.md) | What happens when two devices change the same thing, and how to resolve it |
| [History and restoring](docs/history.md) | Browsing history, bringing back old versions, using plain git |
| [Notifications and status](docs/notifications.md) | Desktop notifications, status bars, the event stream, hook commands |
| [Configuration](docs/configuration.md) | Every setting, config file locations, environment variables |
| [Command line](docs/cli.md) | Conventions, help, the man page, shell completion, and the [command reference](docs/cli-reference.md) |
| [Security](docs/security.md) | What the server protects against, and how |
| [Troubleshooting](docs/troubleshooting.md) | Error messages and what to do about them |
| [Development](docs/development.md) | Building, testing, CI and releasing |

## License

[MIT](LICENSE)
