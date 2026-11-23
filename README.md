# dropgitbox

A Dropbox-like folder sync service that uses git behind the scenes. Every
change is **auto-committed before it is synced**, so every device and the
server hold the complete history of every file. Any past version can be
listed and restored, and history can never be rewritten on the server.

One binary, `dropgitbox`, is both the server and the client.

```
 laptop                         server node                    desktop
┌──────────────────┐          ┌───────────────────┐          ┌──────────────────┐
│ ~/Box (git tree) │  push    │ /var/lib/         │  push    │ ~/Box (git tree) │
│ dropgitbox watch │ ───────► │  dropgitbox/      │ ◄─────── │ dropgitbox watch │
│  fsnotify        │ ◄─────── │  Box.git (bare)   │ ───────► │  fsnotify        │
│  auto-commit     │  fetch   │ /events (SSE) ────┼─ notify ►│  auto-commit     │
└──────────────────┘          └───────────────────┘          └──────────────────┘
```

## How it works

There is **one server node** and any number of **client devices**. Devices
only talk to the server, never to each other, and always over outbound
HTTP(S). Only the server needs to be reachable. See
[Networking](#networking).

**Server** (`dropgitbox server`) keeps one bare git repository per synced
folder. It serves git's smart HTTP protocol through `git http-backend`, and
after every push it notifies subscribed devices over a Server-Sent Events
stream, so other devices sync within moments instead of polling. Each
device has its own revocable token. Force-pushes and branch deletion are
refused (`receive.denyNonFastForwards`, `receive.denyDeletes`), so history
is append-only.

**Client daemon** (`dropgitbox watch`) watches every configured folder
recursively. After a change it waits for a quiet period (2s by default,
30s at most during continuous writes), then:

1. `git add --all` and commits with a descriptive message, for example
   `Modified notes/todo.txt` or `3 files: 2 added, 1 deleted`. The commit
   body names the device and lists the changed files.
2. fetches from the server and merges (a fast-forward when possible).
3. pushes, retrying when another device pushed in between.

It also syncs when the server announces a push, every 5 minutes as a safety
net, and with backoff after a network error. A device that is offline
keeps committing locally and catches up when it reconnects.

**Conflicts stop and ask.** If two devices change the same lines, the merge
leaves standard git conflict markers in the file, and that folder **pauses
syncing**: nothing is auto-committed, so the markers can never be synced by
accident. You get a log warning and, if `notify-send` is available, a
desktop notification. Once you run `dropgitbox resolve`, syncing resumes by
itself. Edits to different parts of a file, or to different files, merge
automatically.

## Install

Linux and macOS, amd64 and arm64. Every machine needs **git 2.28+**.

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
[releases page](https://github.com/dhruvasagar/dropgitbox/releases).
`install.sh` verifies the release checksum before installing. It also
accepts:

| Option | Effect |
| --- | --- |
| `--service` | Install and start the per-user sync daemon: a systemd user unit on Linux, a launchd agent on macOS |
| `--server [--public-url URL]` | Set up this machine as a server node (Linux with systemd, as root) |
| `--version vX.Y.Z` | Install a specific release |
| `--bin-dir DIR` | Install the binary somewhere else |

## Quick start

### 1. Set up the server node

```sh
curl -fsSL https://raw.githubusercontent.com/dhruvasagar/dropgitbox/main/install.sh \
  | sudo sh -s -- --server --public-url https://box.example.com
```

With a `.deb` or `.rpm` instead, set `public_url` in
`/etc/dropgitbox/config.toml` and run
`sudo systemctl enable --now dropgitbox-server`.

The service runs as a sandboxed, unprivileged user and keeps everything in
`/var/lib/dropgitbox`. **`public_url` is the address devices will use**:
see [Networking](#networking) for how to make it reachable.

### 2. Add devices

Each device gets its own token, delivered as an invite. On the server:

```console
$ sudo dropgitbox device add laptop
Registered device "laptop".

On that device, install dropgitbox and run:

  dropgitbox join dgb1.eyJzZXJ2ZXIiOiJodHRwczovL2JveC5leGFtcGxlLmNvbSIs...
```

On the device:

```sh
dropgitbox join dgb1.eyJ...               # saves server, device name and token
dropgitbox setup -repo Box -dir ~/Box     # sync a folder (created on the server if new)
systemctl --user enable --now dropgitbox  # keep it synced (or: install.sh --service)
```

Repeat for every device. `dropgitbox repos` shows the folders already on
the server, so a new device can pick which ones to sync. The invite
contains the token, so **send it privately**.

**Faster for more devices:** copy the `[[folders]]` list from an existing
device's `~/.config/dropgitbox/config.toml` to the new one. The file holds
no secrets and uses `~` paths, so it suits a dotfiles repo. Then run
`join` and `dropgitbox watch`, which sets up every listed folder.

Manage devices on the server: `sudo dropgitbox device list`, and
`sudo dropgitbox device revoke laptop`. Revocation takes effect
immediately, with no restart.

## Commands

| Command | What it does |
| --- | --- |
| `server` | Run the server (`-addr`, `-data`, `-tls-cert/-tls-key`, `-no-auth`, `-v`) |
| `device add NAME` / `list` / `revoke NAME` | Manage devices (run on the server node) |
| `join INVITE` | Connect this device to a server |
| `repos` | List folders on the server, and where each one is synced locally |
| `setup -repo NAME -dir DIR` | Start syncing a folder, and register it in the config |
| `watch` | Run the sync daemon for all configured folders, or `-dir DIR` |
| `sync` | Commit and sync once |
| `status` | Show head, ahead/behind, uncommitted changes and any conflict |
| `resolve [-ours\|-theirs] [-force] [PATH...]` | Finish a paused merge, then sync |
| `log [-n N] [PATH...]` | Show history, optionally for specific files |
| `restore -rev REV PATH...` | Bring back an old version. It is committed as a *new* change, so nothing is lost |
| `config` | Show which config file is in use |
| `version` | Print the version |

`sync` and `status` act on the folder containing the current directory, or
on every configured folder when run elsewhere. Every command accepts
`-config FILE`.

### Resolving a conflict

```console
$ dropgitbox status
...
SYNC PAUSED in /home/me/Box: changes on this device conflict with changes from the server.
Conflicted files:
  notes/todo.txt

$ $EDITOR ~/Box/notes/todo.txt        # remove the <<<<<<< ======= >>>>>>> markers
$ dropgitbox resolve -dir ~/Box
Conflict resolved in commit faf87cdbdc.
Synced.
```

Or keep one side without editing: `resolve -ours` keeps this device's
version and `-theirs` keeps the server's, either for every conflicted file
or only for the paths you list. Both versions stay in history either way.

## Configuration

dropgitbox follows the XDG Base Directory spec. The first file found is
used:

1. `-config FILE`, or `$DROPGITBOX_CONFIG`
2. `$XDG_CONFIG_HOME/dropgitbox/config.toml` (default
   `~/.config/dropgitbox/config.toml`, on macOS too)
3. `$XDG_CONFIG_DIRS/dropgitbox/config.toml` (default `/etc/xdg/...`)
4. `/etc/dropgitbox/config.toml` (the packaged server's config)

Precedence: command-line flag > environment variable > config file >
default. Server data defaults to `$XDG_DATA_HOME/dropgitbox`
(`~/.local/share/dropgitbox`). The packaged server uses
`/var/lib/dropgitbox`.

```toml
[server]                          # server node
addr = ":8080"
data_dir = "/var/lib/dropgitbox"
public_url = "https://box.example.com"

[client]                          # written by `dropgitbox join`
server = "https://box.example.com"
device = "laptop"
debounce = "2s"
poll = "5m"

[[folders]]                       # added by `dropgitbox setup`
path = "~/Box"
repo = "Box"
```

The client token lives in a separate file, `~/.config/dropgitbox/token`
(mode 0600), so `config.toml` can be shared. Unknown keys are rejected,
since they are almost always typos. A token in a file that other users can
read is refused or warned about. See
[`packaging/config.example.toml`](packaging/config.example.toml) for every
option.

## Networking

Clients make only **outbound** connections to the server: git over HTTP(S),
plus one long-lived event-stream connection per folder. No ports are opened
on clients, and they work behind NAT. The server must be reachable from
every device, at the address in `public_url`. Typical choices:

- **Tailscale or WireGuard (recommended for personal use).** Install it on
  the server and every device. `tailscale serve --bg 8080` gives the server
  an HTTPS address like `https://box.tail1234.ts.net`, with no open ports
  and nothing exposed to the internet.
- **A VPS, or a home server with port forwarding,** behind Caddy or nginx
  with Let's Encrypt. Use dynamic DNS if your home IP changes.
- **Cloudflare Tunnel,** for no open ports and no VPN client. Pushes are
  capped at 100 MB per request on the free plan.
- **LAN only,** `http://homeserver.local:8080`, on a trusted network.

[docs/deployment.md](docs/deployment.md) covers each option step by step,
including proxy configs. Reverse proxies need streaming enabled, and nginx
needs `client_max_body_size 0`.

## Server API

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check (no auth) |
| `GET /api/v1/whoami` | The device a token belongs to |
| `GET /api/v1/repos` | List repositories and their heads |
| `GET /api/v1/repos/{name}` | One repository's head |
| `POST /api/v1/repos/{name}` | Create a repository (idempotent) |
| `GET /api/v1/repos/{name}/events` | SSE stream of `{"repo","head"}` events: one on connect, then one per push |
| `/git/{name}.git/...` | git smart HTTP (clone, fetch, push) |

Authentication is `Authorization: Bearer <token>`, or HTTP Basic auth with
the token as the password. Every synced folder is an ordinary git
repository, so plain git works too:
`git clone https://x:$TOKEN@box.example.com/git/Box.git`.

## Development

```
cmd/dropgitbox/      CLI: server/device commands, client commands
internal/config/     XDG config discovery, TOML loading and in-place editing
internal/server/     HTTP server: device auth, repos, git-http-backend, SSE hub
internal/client/     setup, sync engine, conflicts, watcher, daemon, invites
internal/gitx/       thin wrapper around the git CLI
packaging/           systemd units, launchd agent, configs, package scripts
```

```sh
go test -race ./...
```

The integration tests start real servers and drive several simulated
devices through syncing, merging, conflicts (including modify/delete),
restores, history-rewrite protection, device revocation, and live syncing
via the daemon.

### Releasing

Releases are built by [GoReleaser](https://goreleaser.com) from
[`.goreleaser.yaml`](.goreleaser.yaml). It produces tarballs, `.deb` and
`.rpm` packages, and checksums. Push a tag and the `Release` workflow
publishes them on GitHub:

```sh
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

Test a release locally with `goreleaser release --snapshot --clean`. The
output goes to `dist/`.

## Known limitations

- **Large or binary files.** Every version of every file is kept forever,
  in full, on every device. That is the point, but it makes dropgitbox a
  poor fit for large media. Git LFS support would be the natural next step.
- **Empty directories** are not synced, because git does not track them.
- **Nested git repositories** inside a synced folder are recorded as
  gitlinks, not their contents. Don't put other repos inside a synced folder.
- **Single user.** Every device can access every folder on the server.
  There are no per-folder permissions.
- Binary-file conflicts produce no markers, and this device's version is
  left in place. Pick one with `-ours` or `-theirs`, or put the right file
  in place, before running `resolve`.
- Windows is not supported yet.

## License

[MIT](LICENSE)
