# Running the server

The server node holds the authoritative copy of every synced folder and
decides which devices may sync. This guide covers running and looking after
it. To install it and make it reachable, start with
[deployment](deployment.md).

## Running it

The packages and `install.sh --server` install a systemd service:

```sh
sudo systemctl enable --now dropgitbox-server
journalctl -u dropgitbox-server -f
```

It runs as an ephemeral, unprivileged user in a sandbox, reads
`/etc/dropgitbox/config.toml`, and keeps its data in `/var/lib/dropgitbox`.
Without the service, run `dropgitbox server` yourself; it then uses your
own config and `~/.local/share/dropgitbox`.

## Settings

The server reads the `[server]` table of the config. The ones you're most
likely to set:

```toml
[server]
addr = "127.0.0.1:8080"                  # listen address (default ":8080")
public_url = "https://box.example.com"   # the address devices use; goes into invites
data_dir = "/var/lib/dropgitbox"
```

`public_url` matters most: every invite carries it, so it must be the
address devices actually reach the server at, through any proxy or VPN.
The server warns at startup if it's missing or isn't a valid URL. TLS,
trusted proxies, a shared token and the rest are in
[configuration](configuration.md#server).

Restart the service after changing the config
(`sudo systemctl restart dropgitbox-server`). Device changes don't need a
restart.

## Data and backups

Everything lives in the data directory:

| Path | Contents |
| --- | --- |
| `NAME.git/` | One bare git repository per folder |
| `devices.json` | Registered devices, with SHA-256 hashes of their tokens (never the tokens) |
| `activity.json` | When each device was last seen, and its last reported state per folder |

To back up the server, back up this directory. Git's files don't change
once written, so a backup taken while the server runs is usable; for an
exact snapshot, stop the service while copying. Every device also holds the
full history of the folders it syncs, so a lost server can be rebuilt from
them: set up a new server, then [move each device to it](devices.md#moving-to-another-server).

## Managing devices

Run these on the server node, with `sudo` for the packaged service. They
work on the data directory directly and take effect at once, without a
restart.

### Adding a device

```console
$ sudo dropgitbox device add laptop
Registered device "laptop".

On that device, install dropgitbox and run:

  dropgitbox join dgb1.eyJzZXJ2ZXIiOiJodHRwczovL2JveC5leGFtcGxlLmNvbSIs...

The invite contains the device's access token: send it over a private
channel. To cut the device off later: dropgitbox device revoke laptop
```

The invite holds `public_url`, the device name, and a new 256-bit token.
**Treat it like a password.** `add` then checks that `public_url` answers
as a dropgitbox server from the server itself, and warns if it doesn't,
which catches a wrong host or port before you send the invite. (It can't
check that the device can reach it.) `--url URL` uses another address for
this one invite.

### Seeing how devices are doing

```console
$ sudo dropgitbox device list
DEVICE   LAST SEEN  LAST PUSH  STATUS           VERSION  ADDED
laptop   just now   2m ago     in sync          0.1.0    2026-11-23
desktop  5m ago     1h ago     CONFLICT in Box  0.1.0    2026-11-23
```

Devices report to the server after every sync attempt: in sync, paused on
a conflict, or failed. The `STATUS` column summarises a device across its
folders: `in sync`, `behind in ...` (it hasn't fetched the latest
changes), `CONFLICT in ...`, `ERROR in ...`, or `never synced`.

`device show NAME` has the detail for one device: when it was added, when
it was last seen and from which address, and per folder its reported
state, when it last fetched and pushed, its head and the server's, its
conflicted files and its last error.

Activity is saved every few seconds, so a change can take a moment to show.

### Replacing a token

```sh
sudo dropgitbox device rotate laptop
```

issues a new token and prints a new invite. The old token stops working
immediately and the device's open connections are closed. On the device,
run `dropgitbox join --force` with the new invite and restart its daemon
(see [using a device](devices.md#when-the-token-changes)). Its folders stay
as they are. Rotate a token you think may have leaked.

### Revoking a device

```sh
sudo dropgitbox device revoke laptop     # also: device rm
```

The token stops working and the device's connections close at once. The
files already on the device stay there: revoking stops further syncing, it
doesn't erase anything. To let the device back in, add it again.

## Deleting a folder

There's no command for it, because it destroys history. To delete a folder
for good:

1. On every device, stop syncing it: remove its `[[folders]]` entry (see
   [using a device](devices.md#stopping-syncing-a-folder)).
2. On the server, delete its repository:
   `sudo rm -rf /var/lib/dropgitbox/NAME.git`.

A device still syncing the folder would fail to sync it, and the next
`setup` with that name would create it again, empty.

## Monitoring

- **Logs:** `journalctl -u dropgitbox-server`. Failed authentication is
  logged with the client's address (see [security](security.md)).
- **Devices:** `device list` shows which devices have stopped syncing or
  need attention.
- **Liveness:** `GET /healthz` answers `ok` without authentication, for
  uptime checks and load balancers.

## Upgrading

Install the new package, or re-run `install.sh --server`, then restart the
service. The server and devices can be upgraded independently;
`device list` shows each device's version.

## HTTP API

Every route except `/healthz` needs a device token, as
`Authorization: Bearer TOKEN`, or as the password in HTTP Basic auth.

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check (no authentication) |
| `GET /api/v1/whoami` | The device a token belongs to |
| `POST /api/v1/status` | A device reports its sync state for a folder (`ok`, `conflict` or `error`) |
| `GET /api/v1/repos` | List the repositories and their heads |
| `GET /api/v1/repos/{name}` | One repository's head |
| `POST /api/v1/repos/{name}` | Create a repository (idempotent) |
| `GET /api/v1/repos/{name}/events` | Server-Sent Events: `{"repo","head"}` on connect, then after every push |
| `/git/{name}.git/...` | git smart HTTP: clone, fetch and push |
