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

To try everything on one machine first (a server, two devices, a
conflict, notifications, revoking a device), follow the
[local demo](docs/demo.md).

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

### 3. Manage devices (on the server)

```console
$ sudo dropgitbox device list
DEVICE   LAST SEEN  LAST PUSH  STATUS           VERSION  ADDED
laptop   just now   2m ago     in sync          0.1.0    2026-11-23
desktop  5m ago     1h ago     CONFLICT in Box  0.1.0    2026-11-23

$ sudo dropgitbox device show desktop     # per-folder state, heads, conflicted files, last error
$ sudo dropgitbox device rotate laptop    # new token + invite; the old token stops working
$ sudo dropgitbox device revoke desktop   # cut the device off
```

Each client reports to the server after every sync attempt: synced, paused
on a conflict, or failed with an error. Together with the server's own
record of each device's last request, fetch and push, `list` and `show`
tell you which devices are healthy and which need attention. Rotation and
revocation take effect immediately, with no restart, and close the
device's open connections.

## Commands

| Command | What it does |
| --- | --- |
| `server` | Run the server (`-addr`, `-data`, `-tls-cert/-tls-key`, `-no-auth`, `-v`) |
| `device add\|list\|show\|rotate\|revoke` | Manage devices: invite, inspect sync status, replace a token, revoke (run on the server node) |
| `join INVITE` | Connect this device to a server |
| `repos` | List folders on the server, and where each one is synced locally |
| `setup -repo NAME -dir DIR` | Start syncing a folder, and register it in the config |
| `watch` | Run the sync daemon for all configured folders, or `-dir DIR` |
| `sync` | Sync now. With the daemon running, it asks the daemon and waits for the result |
| `status [-short\|-json]` | Live daemon state per folder, plus head, ahead/behind, uncommitted changes and any conflict |
| `events [-json]` | Stream the daemon's events live |
| `notify test` | Send a test desktop notification and run the hook command |
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

## Notifications and status

The daemon (`dropgitbox watch`) turns everything that happens to a folder
into events, and makes them visible in three ways.

**Events**

| Event | When | Desktop notification by default |
| --- | --- | --- |
| `pending` | Local changes detected, waiting for the folder to go quiet | |
| `syncing` | A sync started | |
| `synced` | A sync finished (with counts of committed and pulled files) | |
| `remote_changes` | A sync brought in changes from other devices | |
| `conflict` | Syncing paused on a conflict, and you need to act | ✔ |
| `resolved` | The conflict was resolved and syncing resumed | ✔ |
| `error` | A sync attempt failed; it is retried automatically | |
| `offline` | Syncing has kept failing for `offline_after` (default 10 minutes), so brief network blips stay quiet | ✔ |
| `online` | Syncing works again after `offline` or `unauthorized` | ✔ |
| `unauthorized` | The server rejected this device's token (revoked or rotated) | ✔ |

**1. Desktop notifications** on Linux use `notify-send`, falling back to
`gdbus` (libnotify or GLib, present on most desktops). On macOS they use
`osascript`, the built-in Notification Center. By default only events that
need your attention, or tell you a problem is over, pop up. Routine syncing
stays quiet, and is visible in `status` instead. Run `dropgitbox notify
test` to check that notifications work.

**2. Live status** comes from the daemon over a private Unix socket:
`$XDG_RUNTIME_DIR/dropgitbox/daemon.sock`, or
`~/.local/state/dropgitbox/daemon.sock`; the socket is mode 0600 and its
directory 0700.

```console
$ dropgitbox status -short
dropgitbox: CONFLICT Box
$ dropgitbox status
Daemon:  running (pid 4242, version 0.1.0, up 3h2m)

Folder:  /home/me/Box
State:   PAUSED: conflict in notes.txt
...
$ dropgitbox events
13:43:31  syncing        Box: syncing
13:43:31  conflict       Box: sync paused, conflict: Conflicting changes in notes.txt. ...
13:43:35  resolved       Box: conflict resolved: Syncing has resumed.
```

`status -short` prints one line, the most urgent state across folders:
`synced`, `pending`, `syncing`, `retrying`, `offline`, `CONFLICT` or
`access denied`. Use it in status bars and prompts:

```sh
# tmux
set -g status-right '#(dropgitbox status -short)'
# waybar / polybar / i3blocks: a custom module running `dropgitbox status -short` every few seconds
# macOS menu bar: a SwiftBar or xbar plugin that prints `dropgitbox status -short`
```

`status -json` and `events -json` give machine-readable state and events
for richer integrations. While the daemon runs, `dropgitbox sync` asks it
to sync and waits for the outcome, instead of running git in the same
folder concurrently. Only one daemon runs per user, and starting a second
is refused.

**3. Hook command** runs your own program for chosen events, to forward
notifications anywhere: ntfy, Pushover, email, a chat webhook, a sound.

```toml
[notify]
command = ["~/bin/dropgitbox-hook"]
command_events = ["conflict", "resolved", "offline", "online", "unauthorized", "remote_changes"]
```

The command runs directly, not through a shell. Each event arrives as
JSON on stdin, and in environment variables:

| Variable | Contents |
| --- | --- |
| `DROPGITBOX_EVENT` | Event type, e.g. `conflict` |
| `DROPGITBOX_FOLDER`, `DROPGITBOX_REPO` | Which folder |
| `DROPGITBOX_TITLE`, `DROPGITBOX_BODY` | Ready-made notification text |
| `DROPGITBOX_URGENT` | `true` for events that need action |
| `DROPGITBOX_FILES` | Conflicted or changed files, one per line |
| `DROPGITBOX_MESSAGE` | Error or detail message |
| `DROPGITBOX_TIME` | RFC 3339 timestamp |

```sh
#!/bin/sh
# ~/bin/dropgitbox-hook: forward to a phone through ntfy
curl -s -H "Title: $DROPGITBOX_TITLE" -d "$DROPGITBOX_BODY" https://ntfy.sh/my-private-topic >/dev/null
```

Hooks run one at a time, each with a 30 second limit, and failures are
logged. Run `dropgitbox notify test` to try yours with a test event.

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

[notify]                          # client daemon notifications
desktop = true
offline_after = "10m"
command = ["~/bin/dropgitbox-hook"]
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

## Security

The server is designed to be exposed publicly, behind TLS:

- **Every route except `/healthz` requires a valid token.** This is checked
  before anything else, so without one you cannot even learn which folders
  exist. Device tokens are 256-bit random values, stored on the server only
  as SHA-256 hashes. An optional shared token must be at least 24
  characters.
- **Brute-force protection.** A client that presents 20 invalid tokens
  within 5 minutes is blocked for 15 minutes (HTTP 429). IPv6 clients are
  grouped per /64. Requests without credentials, such as git's first
  anonymous probe, don't count. Every failure is logged with the client's
  address, so tools like fail2ban can also block at the firewall (see
  [docs/deployment.md](docs/deployment.md#hardening-a-public-server)).
- **Real client addresses behind a proxy.** `X-Forwarded-For` is believed
  only from `server.trusted_proxies` (default: loopback), using the
  rightmost untrusted address, so clients cannot spoof their address.
- **Minimal git surface.** Only git's three smart-HTTP endpoints are
  served. Raw repository files, the legacy "dumb" protocol, and paths
  outside a repository return 404. History protection is enforced for
  every repository regardless of its own config: no force-pushes, no
  branch deletion, and `receive.fsckObjects` checks the integrity of
  everything pushed.
- **Hardened HTTP.** Header and idle timeouts, a 64 KiB header limit,
  TLS 1.2+ when serving TLS directly, size limits on API request bodies, a
  per-device cap on open event streams, no internal details in error
  responses, and conservative security headers (`nosniff`, `no-store`,
  `frame-ancestors 'none'`).
- **Revocation is immediate.** `device revoke` and `device rotate` take
  effect on the next request, and close the device's open event streams.
- **Sandboxed service.** The packaged systemd unit runs as an ephemeral
  unprivileged user, with a read-only system, no home directories, and no
  capabilities.
- **Clients** keep tokens in 0600 files and pass them to git through the
  environment, never in URLs, `.git/config`, or process arguments. They
  warn when a server URL uses plain HTTP.

What a token holder can do: any valid device token can read and write
every folder on the server, because this is a single-user design. Revoke
lost devices promptly. History cannot be destroyed through the server, so
even a compromised device can only add commits, and every earlier version
stays recoverable.

## Server API

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check (no auth) |
| `GET /api/v1/whoami` | The device a token belongs to |
| `POST /api/v1/status` | A client reports its sync state for a folder (`ok`, `conflict`, `error`) |
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

### Continuous integration

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on every push
to `main` and on every pull request. It runs three jobs:

- **lint**: `gofmt`, `go mod tidy`, `go vet`, `shellcheck` on all shell
  scripts, the release scripts' self-test, and
  [govulncheck](https://go.dev/blog/govulncheck) for known vulnerabilities
  in the code paths actually used.
- **test**: `go test -race` on Linux and macOS.
- **package**: `goreleaser check`, plus a full snapshot build of every
  release artifact, so packaging problems show up before you tag.

`go.mod` pins a patched Go toolchain (`toolchain go1.26.11`), and the go
command downloads it automatically if yours is older. When govulncheck
reports a standard-library issue, raise that line to the fixed version.

### Releasing

Versions follow [semantic versioning](https://semver.org): `vMAJOR.MINOR.PATCH`,
and `vX.Y.Z-rc.N` for prereleases. Cut a release with
[`scripts/release.sh`](scripts/release.sh):

```sh
git push origin main          # wait for CI to pass
scripts/release.sh            # or: patch | minor | major | 1.4.0 | minor --pre rc
```

The script:

1. Checks you're on an up-to-date, clean `main`.
2. Picks the next version. Without an argument it suggests one from the
   commits since the last release:
   - **major** for a breaking change: `feat!:` style, or `BREAKING CHANGE`
     in a commit body. Before 1.0.0 this becomes **minor**, as semver
     allows for 0.x.
   - **minor** if anything was added.
   - **patch** otherwise.

   `--pre rc` makes `v1.3.0-rc.1`, then `rc.2` on the next run.
3. Drafts release notes from those commits, grouped as Breaking changes,
   New, Fixes, Changes and Documentation, each with its commit hash and a
   full-changelog link. Commits starting with "Add …", "Fix …" and so on
   are grouped automatically, as are Conventional Commit prefixes
   (`feat:`, `fix:`, …). Maintenance commits (`chore:`, `ci:`, `test:`,
   `refactor:`) are left out.
4. Opens the draft in `$VISUAL` or `$EDITOR`. Reword it, add a summary,
   or reorder it: **what you save is the release text on GitHub**. Save an
   empty file to abort. An aborted draft is kept in
   `.git/RELEASE_NOTES_<version>.md` and reused next time.
5. Creates an **annotated** tag whose message holds the notes, and checks
   it with [`scripts/check-release-tag.sh`](scripts/check-release-tag.sh).
6. Asks, then pushes the tag.

Useful options: `--dry-run` (show the version and the draft, change
nothing), `--no-edit`, `--no-push`, `-y`.

Pushing a `v*` tag starts
[`.github/workflows/release.yml`](.github/workflows/release.yml):

1. **validate** runs `check-release-tag.sh` again, so a tag pushed by hand
   gets the same checks. The tag must be strict semver, annotated, with
   notes, on `main`, and higher than every earlier tag.
2. **ci** runs the whole CI workflow above against the tagged commit.
3. **publish** runs GoReleaser. It builds the archives, `.deb` and `.rpm`
   packages and checksums, and creates the GitHub release named
   `dropgitbox vX.Y.Z`. The tag's notes are the body, with install
   instructions appended. `-rc` and other prerelease versions are marked
   as prereleases.

Nothing is published unless all three pass. If a release goes wrong,
delete the GitHub release and the tag (`git push origin :refs/tags/vX.Y.Z`
and `git tag -d vX.Y.Z`), fix the problem, and release again. Use a new
patch version if anyone may already have downloaded it.

To try the packaging locally, run `goreleaser release --snapshot --clean`.
The output goes to `dist/`.

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
